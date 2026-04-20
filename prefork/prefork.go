// Package prefork provides a way to prefork a fasthttp server.
package prefork

import (
	"errors"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

const (
	preforkChildEnvVariable = "FASTHTTP_PREFORK_CHILD"
	defaultNetwork          = "tcp4"
)

var (
	defaultLogger = Logger(log.New(os.Stderr, "", log.LstdFlags))
	// ErrOverRecovery is returned when the times of starting over child prefork processes exceed
	// the threshold.
	ErrOverRecovery = errors.New("exceeding the value of RecoverThreshold")

	// ErrOnlyReuseportOnWindows is returned when Reuseport is false.
	ErrOnlyReuseportOnWindows = errors.New("windows only supports Reuseport = true")
)

// Logger is used for logging formatted messages.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...any)
}

// Prefork implements fasthttp server prefork.
//
// Preforks master process (with all cores) between several child processes
// increases performance significantly, because Go doesn't have to share
// and manage memory between cores.
//
// WARNING: using prefork prevents the use of any global state!
// Things like in-memory caches won't work.
type Prefork struct {
	// By default standard logger from log package is used.
	Logger Logger

	ln net.Listener

	ServeFunc         func(ln net.Listener) error
	ServeTLSFunc      func(ln net.Listener, certFile, keyFile string) error
	ServeTLSEmbedFunc func(ln net.Listener, certData, keyData []byte) error

	// The network must be "tcp", "tcp4" or "tcp6".
	//
	// By default is "tcp4"
	Network string

	files []*os.File

	// Child prefork processes may exit with failure and will be started over until the times reach
	// the value of RecoverThreshold, then it will return and terminate the server.
	RecoverThreshold int

	// Flag to use a listener with reuseport, if not a file Listener will be used
	// See: https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/
	//
	// It's disabled by default
	Reuseport bool

	// OnMasterDeath, when non-nil, enables monitoring of the master process
	// in child processes. If the master process dies unexpectedly, this
	// callback is invoked. This allows custom cleanup before shutdown.
	//
	// It is recommended to set this to func() { os.Exit(1) } if no custom
	// cleanup is needed.
	OnMasterDeath func()

	// OnChildSpawn is called in the master process whenever a new child process is spawned.
	// It receives the PID of the newly spawned child process.
	//
	// If this callback returns an error, the prefork operation will be aborted.
	OnChildSpawn func(pid int) error

	// OnMasterReady is called in the master process after all child processes have been spawned.
	// It receives a slice of all child process PIDs.
	//
	// If this callback returns an error, the prefork operation will be aborted.
	OnMasterReady func(childPIDs []int) error

	// OnChildRecover is called in the master process when a child process is restarted
	// after a crash. It receives the PID of the newly recovered child process.
	OnChildRecover func(pid int)

	// CommandProducer creates and starts a child process command.
	// If nil, the default implementation re-executes the current binary
	// with FASTHTTP_PREFORK_CHILD=1 in the environment.
	//
	// The producer receives the files to pass as ExtraFiles and must return
	// an already started command (i.e. cmd.Start() must have been called).
	// The caller is responsible for setting Stdout, Stderr, Env, and ExtraFiles
	// on the command before starting it.
	//
	// This is primarily useful for testing (injecting dummy commands)
	// or for frameworks that need custom child process setup.
	CommandProducer func(files []*os.File) (*exec.Cmd, error)
}

// IsChild checks if the current thread/process is a child.
func IsChild() bool {
	return os.Getenv(preforkChildEnvVariable) == "1"
}

// New wraps the fasthttp server to run with preforked processes.
func New(s *fasthttp.Server) *Prefork {
	return &Prefork{
		Network:           defaultNetwork,
		RecoverThreshold:  runtime.GOMAXPROCS(0) / 2,
		Logger:            s.Logger,
		ServeFunc:         s.Serve,
		ServeTLSFunc:      s.ServeTLS,
		ServeTLSEmbedFunc: s.ServeTLSEmbed,
	}
}

func (p *Prefork) logger() Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return defaultLogger
}

func (p *Prefork) watchMaster(masterPID int) {
	if runtime.GOOS == "windows" {
		// On Windows, os.Getppid() returns a static PID that doesn't change
		// when the parent exits (no reparenting). Use FindProcess+Wait instead.
		proc, err := os.FindProcess(masterPID)
		if err != nil {
			p.logger().Printf("watchMaster: failed to find master process %d: %v\n", masterPID, err)
			return
		}
		if _, err = proc.Wait(); err != nil {
			p.logger().Printf("watchMaster: error waiting for master process %d: %v\n", masterPID, err)
		}
		p.logger().Printf("master process died\n")
		p.OnMasterDeath()
		return
	}

	// Unix/Linux/macOS: When the master exits, the OS reparents the child
	// to another process, causing Getppid() to change. Comparing against
	// the original masterPID (instead of hardcoding 1) ensures this works
	// correctly when the master itself is PID 1 (e.g. in Docker containers).
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if os.Getppid() != masterPID {
			p.logger().Printf("master process died\n")
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

	// File descriptor 3 is the first ExtraFiles entry passed by the master process.
	return net.FileListener(os.NewFile(3, ""))
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
		return err
	}

	tcplistener, err := net.ListenTCP(p.Network, tcpAddr)
	if err != nil {
		return err
	}

	p.ln = tcplistener

	fl, err := tcplistener.File()
	if err != nil {
		return err
	}

	p.files = []*os.File{fl}

	return nil
}

func (p *Prefork) doCommand() (*exec.Cmd, error) {
	// Use custom CommandProducer if provided
	if p.CommandProducer != nil {
		cmd, err := p.CommandProducer(p.files)
		if err != nil {
			return nil, err
		}
		if cmd == nil || cmd.Process == nil {
			return nil, errors.New("prefork: CommandProducer must return a started command")
		}
		return cmd, nil
	}

	// Default implementation using os.Executable() for reliable path resolution
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}

	args := make([]string, len(os.Args))
	args[0] = executable
	copy(args[1:], os.Args[1:])

	cmd := &exec.Cmd{
		Path:       executable,
		Args:       args,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Env:        append(os.Environ(), preforkChildEnvVariable+"=1"),
		ExtraFiles: p.files,
	}
	err = cmd.Start()
	return cmd, err
}

func (p *Prefork) prefork(addr string) (err error) {
	if !p.Reuseport {
		if runtime.GOOS == "windows" {
			return ErrOnlyReuseportOnWindows
		}

		if err = p.setTCPListenerFiles(addr); err != nil {
			return err
		}

		// defer for closing the net.Listener opened by setTCPListenerFiles.
		defer func() {
			e := p.ln.Close()
			if err == nil {
				err = e
			}
		}()
	}

	type procSig struct {
		err error
		pid int
	}

	goMaxProcs := runtime.GOMAXPROCS(0)
	sigCh := make(chan procSig, goMaxProcs)
	childProcs := make(map[int]*exec.Cmd, goMaxProcs)

	defer func() {
		for _, proc := range childProcs {
			_ = proc.Process.Kill()
		}
	}()

	// Collect child PIDs for OnMasterReady callback
	childPIDs := make([]int, 0, goMaxProcs)

	for range goMaxProcs {
		var cmd *exec.Cmd
		if cmd, err = p.doCommand(); err != nil {
			p.logger().Printf("failed to start a child prefork process, error: %v\n", err)
			return err
		}

		pid := cmd.Process.Pid
		childProcs[pid] = cmd
		childPIDs = append(childPIDs, pid)

		// Call OnChildSpawn callback.
		// On error we return early — the child is already in childProcs
		// and will be killed by the deferred cleanup above.
		if p.OnChildSpawn != nil {
			if err = p.OnChildSpawn(pid); err != nil {
				p.logger().Printf("OnChildSpawn callback failed for PID %d: %v\n", pid, err)
				return err
			}
		}

		go func(c *exec.Cmd, pid int) {
			sigCh <- procSig{pid: pid, err: c.Wait()}
		}(cmd, pid)
	}

	// Call OnMasterReady callback after all children are spawned
	if p.OnMasterReady != nil {
		if err = p.OnMasterReady(childPIDs); err != nil {
			p.logger().Printf("OnMasterReady callback failed: %v\n", err)
			return err
		}
	}

	var exitedProcs int
	for sig := range sigCh {
		delete(childProcs, sig.pid)

		p.logger().Printf("one of the child prefork processes exited with "+
			"error: %v", sig.err)

		exitedProcs++
		if exitedProcs > p.RecoverThreshold {
			p.logger().Printf("child prefork processes exit too many times, "+
				"which exceeds the value of RecoverThreshold(%d), "+
				"exiting the master process.\n", p.RecoverThreshold)
			err = ErrOverRecovery
			break
		}

		var cmd *exec.Cmd
		cmd, err = p.doCommand()
		if err != nil {
			break
		}
		pid := cmd.Process.Pid
		childProcs[pid] = cmd

		if p.OnChildRecover != nil {
			p.OnChildRecover(pid)
		}

		go func(c *exec.Cmd, pid int) {
			sigCh <- procSig{pid: pid, err: c.Wait()}
		}(cmd, pid)
	}

	return err
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
// certKey is the path to the TLS private key file.
// certFile is the path to the TLS certificate file.
//
// Note: parameter order is (addr, certKey, certFile) — key before cert.
// Internally forwards to ServeTLSFunc as (certFile, certKey).
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
