// Package prefork provides a way to prefork a fasthttp server.
package prefork

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

const (
	preforkChildEnvVariable = "FASTHTTP_PREFORK_CHILD"
	preforkChildEnvValue    = "1"
	defaultNetwork          = "tcp4"

	// inheritedListenerFD is the file descriptor used by the master to pass
	// the bound listener to a child process via ExtraFiles. Children open the
	// listener via os.NewFile(inheritedListenerFD, ...) when Reuseport is false.
	inheritedListenerFD = 3

	// masterPollInterval is the period of the watchMaster ppid-poll on Unix.
	masterPollInterval = 500 * time.Millisecond

	// defaultShutdownGracePeriod is how long the master waits for children to
	// exit cleanly after sending SIGTERM before forcibly killing them.
	defaultShutdownGracePeriod = 5 * time.Second
)

var (
	defaultLogger = Logger(log.New(os.Stderr, "", log.LstdFlags))

	// ErrOverRecovery is returned when child prefork process restarts exceed
	// the value of RecoverThreshold.
	ErrOverRecovery = errors.New("exceeding the value of RecoverThreshold")

	// ErrOnlyReuseportOnWindows is returned when running on Windows without Reuseport.
	ErrOnlyReuseportOnWindows = errors.New("windows only supports Reuseport = true")

	// ErrCommandProducerNilCmd is returned when a CommandProducer returns
	// (nil, nil) instead of a started command.
	ErrCommandProducerNilCmd = errors.New("prefork: CommandProducer returned nil command")

	// ErrCommandProducerNotStarted is returned when a CommandProducer returns
	// an *exec.Cmd whose Process is nil (i.e. cmd.Start() was not called).
	ErrCommandProducerNotStarted = errors.New("prefork: CommandProducer must return a started command")
)

// Logger is used for logging formatted messages. Its method set is intentionally
// identical to fasthttp.Logger so that *fasthttp.Server.Logger can be assigned
// directly.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...any)
}

// Compile-time check that fasthttp.Logger satisfies the local Logger interface;
// keeps the two types in sync if either side ever evolves.
var _ Logger = fasthttp.Logger(nil)

// Prefork implements fasthttp server prefork.
//
// Preforks master process (with all cores) between several child processes
// increases performance significantly, because Go doesn't have to share
// and manage memory between cores.
//
// WARNING: using prefork prevents the use of any global state!
// Things like in-memory caches won't work.
type Prefork struct {
	// Logger receives diagnostic output. By default the standard log package
	// logger writing to stderr is used.
	Logger Logger

	ln net.Listener

	ServeFunc         func(ln net.Listener) error
	ServeTLSFunc      func(ln net.Listener, certFile, keyFile string) error
	ServeTLSEmbedFunc func(ln net.Listener, certData, keyData []byte) error

	// Network must be "tcp", "tcp4" or "tcp6". Default is "tcp4".
	Network string

	files []*os.File

	// RecoverThreshold caps how often crashed children are respawned before
	// the master returns ErrOverRecovery. New() sets it to max(1, GOMAXPROCS/2).
	// When constructing a Prefork directly without New(), a zero value will
	// terminate the master after the very first child crash.
	RecoverThreshold int

	// RecoverInterval, when > 0, makes the master sleep for the given duration
	// before respawning a crashed child. Useful as crash-loop backoff.
	RecoverInterval time.Duration

	// ShutdownGracePeriod is the time the master waits for children to exit
	// after sending SIGTERM before falling back to SIGKILL. Defaults to 5s
	// when zero. On Windows SIGTERM is not delivered, so this is unused there.
	ShutdownGracePeriod time.Duration

	// Reuseport selects a reuseport listener instead of fd-passing.
	// See: https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/
	// Disabled by default.
	Reuseport bool

	// OnMasterDeath, when non-nil, enables monitoring of the master process
	// in child processes. If the master process dies unexpectedly, this
	// callback is invoked. This allows custom cleanup before shutdown.
	//
	// It is recommended to set this to func() { os.Exit(1) } if no custom
	// cleanup is needed.
	//
	// Threading: invoked once from a watcher goroutine in the child. Must not
	// block the goroutine for long; must not call Prefork methods.
	OnMasterDeath func()

	// OnChildSpawn is called in the master after a new child process is
	// successfully started, both during initial spawn and during recovery.
	// It receives the PID of the newly spawned child process.
	//
	// If this callback returns an error, all already-running children are
	// killed and the prefork operation returns that error.
	//
	// Threading: invoked synchronously from the master goroutine. Must not
	// block; must not call Prefork methods. Panics are recovered and surfaced
	// as the returned error.
	OnChildSpawn func(pid int) error

	// OnMasterReady is called in the master process exactly once, after all
	// initial children have been spawned and before the supervision loop runs.
	// It receives a slice of all initial child PIDs.
	//
	// If this callback returns an error, the prefork operation aborts and
	// returns that error after killing the children.
	//
	// Threading: invoked synchronously from the master goroutine. The slice
	// is owned by the caller after the call returns. Panics are recovered.
	OnMasterReady func(childPIDs []int) error

	// OnChildRecover is called in the master after a crashed child has been
	// replaced. It receives the PID of the old (crashed) process and the PID
	// of its replacement.
	//
	// If this callback returns an error, all running children are killed and
	// the prefork operation returns that error.
	//
	// Threading: invoked synchronously from the master goroutine, after
	// OnChildSpawn for the new child. Panics are recovered and surfaced as
	// the returned error.
	OnChildRecover func(oldPID, newPID int) error

	// CommandProducer creates and starts a child process command.
	// If nil, the default implementation re-executes the current binary
	// with FASTHTTP_PREFORK_CHILD=1 in the environment, stdout/stderr
	// inherited from the parent, and the given files as ExtraFiles.
	//
	// A custom producer must:
	//   - Set FASTHTTP_PREFORK_CHILD=1 in the child's environment
	//     (otherwise IsChild() returns false and the child won't serve)
	//   - Call cmd.Start() before returning (the returned command must be
	//     started so cmd.Process is non-nil)
	//   - Pass the provided files as cmd.ExtraFiles when Reuseport is false
	//
	// Primarily useful for testing (injecting dummy commands) or for
	// frameworks that need custom child process setup.
	CommandProducer func(files []*os.File) (*exec.Cmd, error)
}

// IsChild reports whether the current process is a prefork child.
func IsChild() bool {
	return os.Getenv(preforkChildEnvVariable) == preforkChildEnvValue
}

// New wraps the fasthttp server to run with preforked processes.
// It seeds Network and RecoverThreshold to sensible defaults; existing
// fields on s (Logger, Serve*) are captured.
func New(s *fasthttp.Server) *Prefork {
	return &Prefork{
		Network:           defaultNetwork,
		RecoverThreshold:  defaultRecoverThreshold(),
		Logger:            s.Logger,
		ServeFunc:         s.Serve,
		ServeTLSFunc:      s.ServeTLS,
		ServeTLSEmbedFunc: s.ServeTLSEmbed,
	}
}

func defaultRecoverThreshold() int {
	return max(1, runtime.GOMAXPROCS(0)/2)
}

func (p *Prefork) logger() Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return defaultLogger
}

// invokeHook runs fn under a panic recovery, returning the panic as an error
// so a misbehaving callback never tears down the master. Diagnostic logging
// is left to the call site so the same condition is not reported twice.
func (p *Prefork) invokeHook(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("prefork: %s panicked: %v", name, r)
		}
	}()
	return fn()
}

func (p *Prefork) watchMaster(masterPID int) {
	if runtime.GOOS == "windows" {
		// On Windows, os.Getppid() returns a static PID that doesn't change
		// when the parent exits (no reparenting). Use FindProcess+Wait instead.
		proc, err := os.FindProcess(masterPID)
		if err != nil {
			p.logger().Printf("watchMaster: failed to find master process %d: %v", masterPID, err)
			p.OnMasterDeath()
			return
		}
		if _, err = proc.Wait(); err != nil {
			p.logger().Printf("watchMaster: error waiting for master process %d: %v", masterPID, err)
		}
		p.logger().Printf("master process %d died", masterPID)
		p.OnMasterDeath()
		return
	}

	// Unix/Linux/macOS: When the master exits, the OS reparents the child
	// to another process, causing Getppid() to change. Comparing against
	// the original masterPID (instead of hardcoding 1) ensures this works
	// correctly when the master itself is PID 1 (e.g. in Docker containers).
	ticker := time.NewTicker(masterPollInterval)
	defer ticker.Stop()

	for range ticker.C {
		if os.Getppid() != masterPID {
			p.logger().Printf("master process %d died", masterPID)
			p.OnMasterDeath()
			return
		}
	}
}

func (p *Prefork) listen(addr string) (net.Listener, error) {
	runtime.GOMAXPROCS(1)

	if p.Network == "" {
		p.Network = defaultNetwork
	}

	if p.Reuseport {
		return reuseport.Listen(p.Network, addr)
	}

	// fd inheritedListenerFD is the first ExtraFiles entry passed by the
	// master process when Reuseport is false. Naming the file gives clearer
	// errors from net.FileListener if the fd is invalid.
	//
	// net.FileListener dups the fd, so we close the wrapping *os.File after
	// it returns to avoid leaking the original descriptor. The returned
	// listener owns its own dup'd fd and is unaffected by this close.
	f := os.NewFile(inheritedListenerFD, "fasthttp-prefork-listener")
	ln, err := net.FileListener(f)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("prefork: close inherited listener fd: %w", closeErr)
	}
	if err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		return nil, err
	}
	return ln, nil
}

// listenAsChild performs the common child process setup: creates the listener
// and starts watching the master process if OnMasterDeath is configured.
func (p *Prefork) listenAsChild(addr string) (net.Listener, error) {
	ln, err := p.listen(addr)
	if err != nil {
		return nil, err
	}

	p.ln = ln

	if p.OnMasterDeath != nil {
		go p.watchMaster(os.Getppid())
	}

	return ln, nil
}

func (p *Prefork) setTCPListenerFiles(addr string) error {
	if p.Network == "" {
		p.Network = defaultNetwork
	}

	tcpAddr, err := net.ResolveTCPAddr(p.Network, addr)
	if err != nil {
		return fmt.Errorf("prefork: resolve %s/%s: %w", p.Network, addr, err)
	}

	tcpListener, err := net.ListenTCP(p.Network, tcpAddr)
	if err != nil {
		return fmt.Errorf("prefork: listen tcp %s: %w", addr, err)
	}

	listenerFile, err := tcpListener.File()
	if err != nil {
		// Close the bound listener so we don't leak the socket/fd when
		// File() fails. p.ln is intentionally only assigned after this
		// point so the caller never sees a half-initialised state.
		_ = tcpListener.Close()
		return fmt.Errorf("prefork: dup listener fd: %w", err)
	}

	p.ln = tcpListener
	p.files = []*os.File{listenerFile}

	return nil
}

// childEnv returns os.Environ() with the prefork child marker variable set,
// stripping any pre-existing value to avoid duplicate keys with last-wins
// semantics.
func childEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+1)
	prefix := preforkChildEnvVariable + "="
	for _, kv := range src {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, preforkChildEnvVariable+"="+preforkChildEnvValue)
	return out
}

func (p *Prefork) doCommand() (*exec.Cmd, error) {
	if p.CommandProducer != nil {
		cmd, err := p.CommandProducer(p.files)
		if err != nil {
			return nil, fmt.Errorf("prefork: CommandProducer: %w", err)
		}
		if cmd == nil {
			return nil, ErrCommandProducerNilCmd
		}
		if cmd.Process == nil {
			return nil, ErrCommandProducerNotStarted
		}
		return cmd, nil
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("prefork: resolve executable: %w", err)
	}

	args := append([]string{executable}, os.Args[1:]...)

	cmd := &exec.Cmd{
		Path:       executable,
		Args:       args,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Env:        childEnv(),
		ExtraFiles: p.files,
	}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("prefork: start child %q: %w", executable, err)
	}
	return cmd, nil
}

type childExit struct {
	err error
	pid int
}

// shutdownChildren signals every entry in childProcs first with SIGTERM (on
// platforms where it is supported) and waits up to grace for them to exit.
// Survivors are then killed unconditionally. wg tracks the per-child Wait
// goroutines and must be drained before returning so no goroutine outlives
// prefork().
func (p *Prefork) shutdownChildren(
	childProcs map[int]*exec.Cmd,
	wg *sync.WaitGroup,
	cancel context.CancelFunc,
	grace time.Duration,
) {
	if grace <= 0 {
		grace = defaultShutdownGracePeriod
	}

	if runtime.GOOS != "windows" {
		for pid, proc := range childProcs {
			if proc == nil || proc.Process == nil {
				continue
			}
			if termErr := proc.Process.Signal(syscall.SIGTERM); termErr != nil &&
				!errors.Is(termErr, os.ErrProcessDone) {
				p.logger().Printf("prefork: SIGTERM child %d: %v", pid, termErr)
			}
		}
	}

	// Wait for graceful exits, with a timeout fallback to SIGKILL.
	graceful := make(chan struct{})
	go func() {
		wg.Wait()
		close(graceful)
	}()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-graceful:
	case <-timer.C:
	}

	for pid, proc := range childProcs {
		if proc == nil || proc.Process == nil {
			continue
		}
		if killErr := proc.Process.Kill(); killErr != nil &&
			!errors.Is(killErr, os.ErrProcessDone) {
			p.logger().Printf("prefork: kill child %d: %v", pid, killErr)
		}
	}

	// Cancel the per-Wait goroutines' send-context so any still blocked on
	// sigCh send unblock cleanly, then wait for all of them to exit.
	cancel()
	wg.Wait()
}

func (p *Prefork) prefork(addr string) (err error) { //nolint:gocyclo
	if !p.Reuseport {
		if runtime.GOOS == "windows" {
			return ErrOnlyReuseportOnWindows
		}

		if err = p.setTCPListenerFiles(addr); err != nil {
			return err
		}

		// Close listener fds opened by setTCPListenerFiles. Both the original
		// tcpListener (p.ln) and the duped fd (p.files[0]) belong to the
		// master only; children inherit independent dup'd copies via fork+exec.
		defer func() {
			err = errors.Join(err, p.ln.Close())
			for _, f := range p.files {
				if closeErr := f.Close(); closeErr != nil {
					p.logger().Printf("prefork: close listener fd: %v", closeErr)
				}
			}
			p.files = nil
		}()
	}

	// ctx cancels per-child Wait goroutines so they unblock from sigCh sends
	// once the supervision loop is gone.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch SIGTERM/SIGINT in the master so we run our shutdown path instead
	// of being killed by the OS without children getting a graceful chance.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signalCh)

	goMaxProcs := runtime.GOMAXPROCS(0)
	// Buffer is sized to initial fleet; per-child goroutines fall back to a
	// context-aware select on send so capacity is not load-bearing.
	sigCh := make(chan childExit, goMaxProcs)
	childProcs := make(map[int]*exec.Cmd, goMaxProcs)

	var wg sync.WaitGroup
	startWait := func(cmd *exec.Cmd, pid int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := childExit{pid: pid, err: cmd.Wait()}
			select {
			case sigCh <- result:
			case <-ctx.Done():
			}
		}()
	}

	defer func() {
		p.shutdownChildren(childProcs, &wg, cancel, p.ShutdownGracePeriod)
	}()

	childPIDs := make([]int, 0, goMaxProcs)

	for range goMaxProcs {
		var cmd *exec.Cmd
		if cmd, err = p.doCommand(); err != nil {
			p.logger().Printf("prefork: failed to start a child process: %v", err)
			return err
		}

		pid := cmd.Process.Pid
		childProcs[pid] = cmd
		childPIDs = append(childPIDs, pid)

		// Start Wait goroutine before the user callback so a panic / error
		// return from OnChildSpawn cannot leave a zombie behind.
		startWait(cmd, pid)

		if p.OnChildSpawn != nil {
			pid := pid
			if hookErr := p.invokeHook("OnChildSpawn", func() error {
				return p.OnChildSpawn(pid)
			}); hookErr != nil {
				p.logger().Printf("prefork: OnChildSpawn for PID %d: %v", pid, hookErr)
				return hookErr
			}
		}
	}

	if p.OnMasterReady != nil {
		pids := append([]int(nil), childPIDs...)
		if hookErr := p.invokeHook("OnMasterReady", func() error {
			return p.OnMasterReady(pids)
		}); hookErr != nil {
			p.logger().Printf("prefork: OnMasterReady: %v", hookErr)
			return hookErr
		}
	}

	var exitedProcs int
	for {
		select {
		case sig := <-signalCh:
			p.logger().Printf("prefork: received signal %v, shutting down", sig)
			return nil

		case sig := <-sigCh:
			delete(childProcs, sig.pid)

			if sig.err != nil {
				p.logger().Printf("prefork: child PID %d exited: %v", sig.pid, sig.err)
			} else {
				p.logger().Printf("prefork: child PID %d exited cleanly", sig.pid)
			}

			exitedProcs++
			if exitedProcs > p.RecoverThreshold {
				p.logger().Printf(
					"prefork: child exits (%d) exceed RecoverThreshold (%d), terminating master",
					exitedProcs, p.RecoverThreshold,
				)
				return ErrOverRecovery
			}

			if p.RecoverInterval > 0 {
				timer := time.NewTimer(p.RecoverInterval)
				select {
				case <-timer.C:
				case <-signalCh:
					if !timer.Stop() {
						<-timer.C
					}
					return nil
				}
			}

			cmd, doErr := p.doCommand()
			if doErr != nil {
				p.logger().Printf("prefork: recovery doCommand: %v", doErr)
				return doErr
			}
			newPID := cmd.Process.Pid
			childProcs[newPID] = cmd

			startWait(cmd, newPID)

			if p.OnChildSpawn != nil {
				newPID := newPID
				if hookErr := p.invokeHook("OnChildSpawn", func() error {
					return p.OnChildSpawn(newPID)
				}); hookErr != nil {
					p.logger().Printf("prefork: OnChildSpawn for recovered PID %d: %v", newPID, hookErr)
					return hookErr
				}
			}

			if p.OnChildRecover != nil {
				oldPID := sig.pid
				newPID := newPID
				if hookErr := p.invokeHook("OnChildRecover", func() error {
					return p.OnChildRecover(oldPID, newPID)
				}); hookErr != nil {
					p.logger().Printf("prefork: OnChildRecover (%d -> %d): %v", oldPID, newPID, hookErr)
					return hookErr
				}
			}
		}
	}
}

// ListenAndServe serves HTTP requests from the given TCP addr.
func (p *Prefork) ListenAndServe(addr string) error {
	if IsChild() {
		ln, err := p.listenAsChild(addr)
		if err != nil {
			return err
		}
		return p.ServeFunc(ln)
	}

	return p.prefork(addr)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr.
//
// Note: parameter order is (addr, certKey, certFile) — key path comes
// before cert path. This is preserved for backward compatibility with
// existing callers and differs from fasthttp.Server.ListenAndServeTLS.
// New code should prefer ListenAndServeTLSEmbed.
func (p *Prefork) ListenAndServeTLS(addr, certKey, certFile string) error {
	if IsChild() {
		ln, err := p.listenAsChild(addr)
		if err != nil {
			return err
		}
		return p.ServeTLSFunc(ln, certFile, certKey)
	}

	return p.prefork(addr)
}

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP addr.
//
// certData and keyData must contain valid TLS certificate and key data.
func (p *Prefork) ListenAndServeTLSEmbed(addr string, certData, keyData []byte) error {
	if IsChild() {
		ln, err := p.listenAsChild(addr)
		if err != nil {
			return err
		}
		return p.ServeTLSEmbedFunc(ln, certData, keyData)
	}

	return p.prefork(addr)
}
