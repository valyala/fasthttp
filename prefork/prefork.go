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
	windowsOS               = "windows"
	watchInterval           = 500 * time.Millisecond
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

	// WatchMaster enables monitoring of the master process.
	// If enabled, child processes will automatically exit when the master process dies.
	//
	// It's disabled by default
	WatchMaster bool

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
	//
	// This callback is non-blocking and its error return value is ignored.
	OnChildRecover func(pid int) error

	// CommandProducer is called to create child process commands.
	// If nil, the default implementation using os.Args is used.
	// This is useful for testing or customizing child process behavior.
	//
	// The function receives the files to be passed as ExtraFiles to the child process.
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

func (p *Prefork) listen(addr string) (net.Listener, error) {
	runtime.GOMAXPROCS(1)

	if p.Network == "" {
		p.Network = defaultNetwork
	}

	// Start watching master process if enabled
	if p.WatchMaster {
		go watchMaster()
	}

	if p.Reuseport {
		return reuseport.Listen(p.Network, addr)
	}

	return net.FileListener(os.NewFile(3, ""))
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
		return p.CommandProducer(p.files)
	}

	// Default implementation
	// #nosec G204
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), preforkChildEnvVariable+"=1")
	cmd.ExtraFiles = p.files
	err := cmd.Start()
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
	// Pre-allocate map with expected capacity for better performance
	childProcs := make(map[int]*exec.Cmd, goMaxProcs)

	defer func() {
		for _, proc := range childProcs {
			_ = proc.Process.Kill()
		}
	}()

	// Pre-allocate slice with expected capacity for OnMasterReady callback
	childPIDs := make([]int, 0, goMaxProcs)

	for i := 0; i < goMaxProcs; i++ {
		cmd, err := p.doCommand()
		if err != nil {
			p.logger().Printf("failed to start a child prefork process, error: %v\n", err)
			return err
		}

		pid := cmd.Process.Pid
		childProcs[pid] = cmd
		childPIDs = append(childPIDs, pid)

		// Call OnChildSpawn callback
		if p.OnChildSpawn != nil {
			if err = p.OnChildSpawn(pid); err != nil {
				p.logger().Printf("OnChildSpawn callback failed for PID %d: %v\n", pid, err)
				return err
			}
		}

		// Pass cmd to goroutine to avoid closure capture bug
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
				"exiting the master process.\n", exitedProcs)
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

		// Call OnChildRecover callback (non-blocking, error ignored)
		if p.OnChildRecover != nil {
			_ = p.OnChildRecover(pid)
		}

		// Pass cmd to goroutine to avoid closure capture bug
		go func(c *exec.Cmd, pid int) {
			sigCh <- procSig{pid: pid, err: c.Wait()}
		}(cmd, pid)
	}

	return err
}

// ListenAndServe serves HTTP requests from the given TCP addr.
func (p *Prefork) ListenAndServe(addr string) error {
	if IsChild() {
		ln, err := p.listen(addr)
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeFunc(ln)
	}

	return p.prefork(addr)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr.
//
// certFile and keyFile are paths to TLS certificate and key files.
func (p *Prefork) ListenAndServeTLS(addr, certKey, certFile string) error {
	if IsChild() {
		ln, err := p.listen(addr)
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeTLSFunc(ln, certFile, certKey)
	}

	return p.prefork(addr)
}

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP addr.
//
// certData and keyData must contain valid TLS certificate and key data.
func (p *Prefork) ListenAndServeTLSEmbed(addr string, certData, keyData []byte) error {
	if IsChild() {
		ln, err := p.listen(addr)
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeTLSEmbedFunc(ln, certData, keyData)
	}

	return p.prefork(addr)
}

// watchMaster monitors the master process and exits the child process if the master dies.
// This ensures that orphaned child processes are properly cleaned up.
//
// On Windows: Uses os.FindProcess and Wait() to detect master process exit.
// On Unix/Linux: Periodically checks if parent PID is 1 (init process), indicating orphan status.
func watchMaster() {
	// Windows implementation
	if runtime.GOOS == windowsOS {
		// Find parent process
		p, err := os.FindProcess(os.Getppid())
		if err == nil {
			_, _ = p.Wait() // Wait for parent to exit
		}
		os.Exit(1) //nolint:revive // Exiting child process is intentional
	}

	// Unix/Linux implementation
	// If parent PID becomes 1 (init), it means the master process has died
	// and this child process has been adopted by init
	for range time.NewTicker(watchInterval).C {
		if os.Getppid() == 1 {
			os.Exit(1) //nolint:revive // Exiting child process is intentional
		}
	}
}
