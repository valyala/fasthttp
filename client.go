package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Do performs the given http request and fills the given http response.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// ErrNoFreeConns is returned if all Client.MaxConnsPerHost connections
// to the requested host are busy.
func Do(req *Request, resp *Response) error {
	return defaultClient.Do(req, resp)
}

var defaultClient Client

// Client implements http client.
type Client struct {
	// Maximum number of connections per each host which may be established.
	//
	// DefaultMaxConnsPerHost is used if not set.
	MaxConnsPerHost int

	// Per-connection buffer size for responses' reading.
	// This also limits the maximum header size.
	//
	// Default buffer size is used if 0.
	ReadBufferSize int

	// Per-connection buffer size for requests' writing.
	//
	// Default buffer size is used if 0.
	WriteBufferSize int

	// Logger is used for error logging.
	//
	// Default logger from log package is used if not set.
	Logger Logger

	mLock sync.Mutex
	m     map[string]*HostClient
}

// Do performs the given http request and fills the given http response.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// ErrNoFreeConns is returned if all Client.MaxConnsPerHost connections
// to the requested host are busy.
func (c *Client) Do(req *Request, resp *Response) error {
	req.ParseURI()
	host := req.URI.Host
	if len(req.Header.PeekBytes(strHost)) == 0 {
		req.Header.SetCanonical(strHost, host)
	}
	if len(host) == 0 {
		return fmt.Errorf("Host must be non-empty. Set it via RequestHeader.Set() or via RequestHeader.RequestURI")
	}
	if !bytes.Equal(req.URI.Scheme, strHTTP) {
		return fmt.Errorf("unsupported protocol %q. Currently only http is supported", req.URI.Scheme)
	}
	req.Header.RequestURI = req.URI.AppendRequestURI(req.Header.RequestURI[:0])

	startCleaner := false

	c.mLock.Lock()
	if c.m == nil {
		c.m = make(map[string]*HostClient)
	}
	hc := c.m[string(host)]
	if hc == nil {
		hc = &HostClient{
			Addr:            string(host),
			MaxConns:        c.MaxConnsPerHost,
			ReadBufferSize:  c.ReadBufferSize,
			WriteBufferSize: c.WriteBufferSize,
			Logger:          c.Logger,
		}
		c.m[hc.Addr] = hc
		if len(c.m) == 1 {
			startCleaner = true
		}
	}
	c.mLock.Unlock()

	if startCleaner {
		go func() {
			mustStop := false
			for {
				t := time.Now()
				c.mLock.Lock()
				for k, v := range c.m {
					if t.Sub(v.LastUseTime) > time.Minute {
						delete(c.m, k)
					}
				}
				if len(c.m) == 0 {
					mustStop = true
				}
				c.mLock.Unlock()

				if mustStop {
					break
				}
				time.Sleep(10 * time.Second)
			}
		}()
	}

	return hc.Do(req, resp)
}

// Maximum number of concurrent connections http client can establish per host
// by default.
const DefaultMaxConnsPerHost = 100

// DialFunc must establish connection to addr.
//
// HostClient.Addr is passed as addr to DialFunc.
type DialFunc func(addr string) (net.Conn, error)

// HostClient is a single-host http client. It can make http requests
// to the given Addr only.
//
// It is forbidden copying HostClient instances.
type HostClient struct {
	// HTTP server host address.
	Addr string

	// Callback for establishing new connection to the host.
	//
	// TCP dialer is used if not set.
	Dial DialFunc

	// Maximum number of connections to the host which may be established.
	//
	// DefaultMaxConnsPerHost is used if not set.
	MaxConns int

	// Per-connection buffer size for responses' reading.
	// This also limits the maximum header size.
	//
	// Default buffer size is used if 0.
	ReadBufferSize int

	// Per-connection buffer size for requests' writing.
	//
	// Default buffer size is used if 0.
	WriteBufferSize int

	// Logger is used for error logging.
	//
	// Default logger from log package is used if not set.
	Logger Logger

	// Last time the client was used.
	LastUseTime time.Time

	connsLock  sync.Mutex
	connsCount int
	conns      []*clientConn

	// dns caching stuff for default dialer.
	tcpAddrsLock        sync.Mutex
	tcpAddrs            []net.TCPAddr
	tcpAddrsPending     bool
	tcpAddrsResolveTime time.Time
	tcpAddrsIdx         uint32

	readerPool sync.Pool
	writerPool sync.Pool
}

type clientConn struct {
	t time.Time
	c net.Conn
	v interface{}
}

// Do performs the given http request and sets the corresponding response.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// ErrNoFreeConns is returned if all HostClient.MaxConns connections
// to the host are busy.
func (c *HostClient) Do(req *Request, resp *Response) error {
	c.LastUseTime = time.Now()

	cc, err := c.acquireConn()
	if err != nil {
		return err
	}
	conn := cc.c

	bw := c.acquireWriter(conn)
	if err = req.Write(bw); err != nil {
		c.releaseWriter(bw)
		c.closeConn(cc)
		return err
	}
	if err = bw.Flush(); err != nil {
		c.releaseWriter(bw)
		c.closeConn(cc)
		return err
	}
	c.releaseWriter(bw)

	br := c.acquireReader(conn)
	if err = resp.Read(br); err != nil {
		c.releaseReader(br)
		c.closeConn(cc)
		return err
	}
	c.releaseReader(br)
	c.releaseConn(cc)
	return err
}

// ErrNoFreeConns is returned when no free connections available
// to the given host.
var ErrNoFreeConns = errors.New("no free connections to host")

func (c *HostClient) acquireConn() (*clientConn, error) {
	var cc *clientConn
	createConn := false
	startCleaner := false

	c.connsLock.Lock()
	n := len(c.conns)
	if n == 0 {
		if c.MaxConns <= 0 {
			c.MaxConns = DefaultMaxConnsPerHost
		}
		if c.connsCount < c.MaxConns {
			c.connsCount++
			createConn = true
		}
		if createConn && c.connsCount == 1 {
			startCleaner = true
		}
	} else {
		n--
		cc = c.conns[n]
		c.conns = c.conns[:n]
	}
	c.connsLock.Unlock()

	if cc != nil {
		return cc, nil
	}
	if !createConn {
		return nil, ErrNoFreeConns
	}

	if startCleaner {
		go func() {
			mustStop := false
			for {
				t := time.Now()
				c.connsLock.Lock()
				for len(c.conns) > 0 && t.Sub(c.conns[0].t) > 10*time.Second {
					cc := c.conns[0]
					c.connsCount--
					cc.c.Close()
					releaseClientConn(cc)

					copy(c.conns, c.conns[1:])
					c.conns = c.conns[:len(c.conns)-1]
				}
				if len(c.conns) == 0 {
					mustStop = true
				}
				c.connsLock.Unlock()

				if mustStop {
					break
				}
				time.Sleep(time.Second)
			}
		}()
	}

	if c.Dial == nil {
		c.Dial = c.defaultDial
	}
	conn, err := c.Dial(c.Addr)
	if err != nil {
		c.decConnsCount()
		return nil, err
	}
	cc = acquireClientConn(conn)
	return cc, nil
}

func (c *HostClient) closeConn(cc *clientConn) {
	c.decConnsCount()
	cc.c.Close()
	releaseClientConn(cc)
}

func (c *HostClient) decConnsCount() {
	c.connsLock.Lock()
	c.connsCount--
	c.connsLock.Unlock()
}

func acquireClientConn(conn net.Conn) *clientConn {
	v := clientConnPool.Get()
	if v == nil {
		cc := &clientConn{
			c: conn,
		}
		cc.v = cc
		return cc
	}
	return v.(*clientConn)
}

func releaseClientConn(cc *clientConn) {
	cc.c = nil
	clientConnPool.Put(cc.v)
}

var clientConnPool sync.Pool

func (c *HostClient) releaseConn(cc *clientConn) {
	cc.t = time.Now()
	c.connsLock.Lock()
	c.conns = append(c.conns, cc)
	c.connsLock.Unlock()
}

func (c *HostClient) acquireWriter(conn net.Conn) *bufio.Writer {
	v := c.writerPool.Get()
	if v == nil {
		n := c.WriteBufferSize
		if n <= 0 {
			n = defaultWriteBufferSize
		}
		return bufio.NewWriterSize(conn, n)
	}
	bw := v.(*bufio.Writer)
	bw.Reset(conn)
	return bw
}

func (c *HostClient) releaseWriter(bw *bufio.Writer) {
	bw.Reset(nil)
	c.writerPool.Put(bw)
}

func (c *HostClient) acquireReader(conn net.Conn) *bufio.Reader {
	v := c.readerPool.Get()
	if v == nil {
		n := c.ReadBufferSize
		if n <= 0 {
			n = defaultReadBufferSize
		}
		return bufio.NewReaderSize(conn, n)
	}
	br := v.(*bufio.Reader)
	br.Reset(conn)
	return br
}

func (c *HostClient) releaseReader(br *bufio.Reader) {
	br.Reset(nil)
	c.readerPool.Put(br)
}

var dnsCacheDuration = time.Minute

func (c *HostClient) defaultDial(addr string) (net.Conn, error) {
	c.tcpAddrsLock.Lock()
	tcpAddrs := c.tcpAddrs
	if tcpAddrs != nil && !c.tcpAddrsPending && time.Since(c.tcpAddrsResolveTime) > dnsCacheDuration {
		c.tcpAddrsPending = true
		tcpAddrs = nil
	}
	c.tcpAddrsLock.Unlock()

	if tcpAddrs == nil {
		var err error
		if tcpAddrs, err = resolveTCPAddrs(addr); err != nil {
			c.tcpAddrsLock.Lock()
			c.tcpAddrsPending = false
			c.tcpAddrsLock.Unlock()
			return nil, err
		}

		c.tcpAddrsLock.Lock()
		c.tcpAddrs = tcpAddrs
		c.tcpAddrsResolveTime = time.Now()
		c.tcpAddrsPending = false
		c.tcpAddrsLock.Unlock()
	}

	tcpAddr := &tcpAddrs[0]
	n := len(tcpAddrs)
	if n > 1 {
		n := atomic.AddUint32(&c.tcpAddrsIdx, 1)
		tcpAddr = &tcpAddrs[n%uint32(n)]
	}
	return net.DialTCP("tcp4", nil, tcpAddr)
}

func resolveTCPAddrs(addr string) ([]net.TCPAddr, error) {
	host := addr
	port := 80
	n := strings.Index(addr, ":")
	if n >= 0 {
		h, portS, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = h
		if port, err = strconv.Atoi(portS); err != nil {
			return nil, err
		}
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	n = len(ips)
	addrs := make([]net.TCPAddr, n)
	for i := 0; i < n; i++ {
		addrs[i].IP = ips[i]
		addrs[i].Port = port
	}
	return addrs, nil
}
