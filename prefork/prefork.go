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

const preforkChildFlag = "prefork-child"
const defaultNetwork = "tcp4"

var Child bool

// Prefork implements fasthttp server prefork
//
// Preforks master process (with all cores) between several child processes
// increases performance significantly, because Go doesn't have to share
// and manage memory between cores
//
// WARNING: Does not recommended for servers with in-memory cache,
// because the cache will be duplicated in each process
type Prefork struct {
	Addr string

	// The network must be "tcp", "tcp4" or "tcp6".
	//
	// By default is "tcp4"
	Network string

	// Flag to use a listener with reuseport, if not a File Listener will be used
	//
	// It's disabled by default
	Reuseport bool

	ServeFunc         func(ln net.Listener) error
	ServeTLSFunc      func(ln net.Listener, certFile, keyFile string) error
	ServeTLSEmbedFunc func(ln net.Listener, certData, keyData []byte) error

	ln    net.Listener
	files []*os.File
}

func init() { // nolint:gochecknoinits
	flag.BoolVar(&Child, preforkChildFlag, false, "is child proc")
}

// New wraps the fasthttp server to run with prefork processes
func New(s *fasthttp.Server) *Prefork {
	return &Prefork{
		Network:           defaultNetwork,
		ServeFunc:         s.Serve,
		ServeTLSFunc:      s.ServeTLS,
		ServeTLSEmbedFunc: s.ServeTLSEmbed,
	}
}

func (p *Prefork) listen() (net.Listener, error) {
	runtime.GOMAXPROCS(1)

	if p.Network == "" {
		p.Network = defaultNetwork
	}

	if p.Reuseport {
		return reuseport.Listen(p.Network, p.Addr)
	}

	return net.FileListener(os.NewFile(3, ""))
}

func (p *Prefork) setTCPListenerFiles() error {
	addr, err := net.ResolveTCPAddr(p.Network, p.Addr)
	if err != nil {
		return err
	}

	tcplistener, err := net.ListenTCP(p.Network, addr)
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

func (p *Prefork) prefork() error {
	strCmd := os.Args[0]
	chErr := make(chan error, 1)

	if !p.Reuseport {
		if err := p.setTCPListenerFiles(); err != nil {
			return err
		}

		defer p.ln.Close()
	}

	for i := 0; i < runtime.NumCPU(); i++ {
		cmd := exec.Command(strCmd, append(os.Args[1:], "-"+preforkChildFlag)...)
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
	p.Addr = addr

	if Child {
		ln, err := p.listen()
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeFunc(ln)
	}

	return p.prefork()
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr
//
// certFile and keyFile are paths to TLS certificate and key files.
func (p *Prefork) ListenAndServeTLS(addr, certKey, certFile string) error {
	p.Addr = addr

	if Child {
		ln, err := p.listen()
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeTLSFunc(ln, certFile, certKey)
	}

	return p.prefork()
}

// ListenAndServeTLSEmbed serves HTTPS requests from the given TCP addr
//
// certData and keyData must contain valid TLS certificate and key data.
func (p *Prefork) ListenAndServeTLSEmbed(addr string, certData, keyData []byte) error {
	p.Addr = addr

	if Child {
		ln, err := p.listen()
		if err != nil {
			return err
		}

		p.ln = ln

		return p.ServeTLSEmbedFunc(ln, certData, keyData)
	}

	return p.prefork()
}
