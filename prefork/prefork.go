package prefork

import (
	"flag"
	"net"
	"os"
	"os/exec"
	"runtime"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/reuseport"
)

const preforkChildFlag = "-prefork-child"
const defaultNetwork = "tcp4"

// Prefork implements fasthttp server prefork
//
// Preforks master process (with all cores) between several child processes
// increases performance significantly, because Go doesn't have to share
// and manage memory between cores
//
// WARNING: using prefork prevents the use of any global state!
// Things like in-memory caches won't work.
type Prefork struct {
	// The network must be "tcp", "tcp4" or "tcp6".
	//
	// By default is "tcp4"
	Network string

	// Flag to use a listener with reuseport, if not a file Listener will be used
	// See: https://www.nginx.com/blog/socket-sharding-nginx-release-1-9-1/
	//
	// It's disabled by default
	Reuseport bool

	ServeFunc         func(ln net.Listener) error
	ServeTLSFunc      func(ln net.Listener, certFile, keyFile string) error
	ServeTLSEmbedFunc func(ln net.Listener, certData, keyData []byte) error

	ln    net.Listener
	files []*os.File
}

func init() { //nolint:gochecknoinits
	// Definition flag to not break the program when the user adds their own flags
	// and runs `flag.Parse()`
	flag.Bool(preforkChildFlag[1:], false, "Is a child process")
}

// IsChild checks if the current thread/process is a child
func IsChild() bool {
	for _, arg := range os.Args[1:] {
		if arg == preforkChildFlag {
			return true
		}
	}

	return false
}

// New wraps the fasthttp server to run with preforked processes
func New(s *fasthttp.Server) *Prefork {
	return &Prefork{
		Network:           defaultNetwork,
		ServeFunc:         s.Serve,
		ServeTLSFunc:      s.ServeTLS,
		ServeTLSEmbedFunc: s.ServeTLSEmbed,
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

func (p *Prefork) prefork(addr string) error {
	chErr := make(chan error, 1)

	if !p.Reuseport {
		if err := p.setTCPListenerFiles(addr); err != nil {
			return err
		}

		defer p.ln.Close()
	}

	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		/* #nosec G204 */
		cmd := exec.Command(os.Args[0], append(os.Args[1:], preforkChildFlag)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.ExtraFiles = p.files

		go func() {
			chErr <- cmd.Run()
		}()
	}

	return <-chErr
}

// ListenAndServe serves HTTP requests from the given TCP addr
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

// ListenAndServeTLS serves HTTPS requests from the given TCP addr
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

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP addr
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
