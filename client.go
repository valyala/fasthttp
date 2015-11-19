package fasthttp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
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
// Response is ignored if resp is nil.
//
// Client determines the server to be requested in the following order:
//
//   - from RequestURI if it contains full url with scheme and host;
//   - from Host header otherwise.
//
// ErrNoFreeConns is returned if all Client.MaxConnsPerHost connections
// to the requested host are busy.
func Do(req *Request, resp *Response) error {
	return defaultClient.Do(req, resp)
}

// DoTimeout performs the given request and waits for response during
// the given timeout duration.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// Client determines the server to be requested in the following order:
//
//   - from RequestURI if it contains full url with scheme and host;
//   - from Host header otherwise.
//
// ErrTimeout is returned if the response wasn't returned during
// the given timeout.
func DoTimeout(req *Request, resp *Response, timeout time.Duration) error {
	return defaultClient.DoTimeout(req, resp, timeout)
}

// Get fetches url contents into dst.
//
// Use Do for request customization.
func Get(dst []byte, url string) (statusCode int, body []byte, err error) {
	return defaultClient.Get(dst, url)
}

// Post sends POST request to the given url with the given POST arguments.
//
// Empty POST body is sent if postArgs is nil.
//
// Use Do for request customization.
func Post(dst []byte, url string, postArgs *Args) (statusCode int, body []byte, err error) {
	return defaultClient.Post(dst, url, postArgs)
}

var defaultClient Client

// Client implements http client.
type Client struct {
	// Client name. Used in User-Agent request header.
	//
	// Default client name is used if not set.
	Name string

	// Callback for establishing new connections to hosts.
	//
	// Default TCP dialer is used if not set.
	Dial DialFunc

	// TLS config for https connections.
	//
	// Default TLS config is used if not set.
	TLSConfig *tls.Config

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
	ms    map[string]*HostClient
}

// Get fetches url contents into dst.
//
// Use Do for request customization.
func (c *Client) Get(dst []byte, url string) (statusCode int, body []byte, err error) {
	return clientGetURL(dst, url, c)
}

// Post sends POST request to the given url with the given POST arguments.
//
// Empty POST body is sent if postArgs is nil.
//
// Use Do for request customization.
func (c *Client) Post(dst []byte, url string, postArgs *Args) (statusCode int, body []byte, err error) {
	return clientPostURL(dst, url, postArgs, c)
}

// DoTimeout performs the given request and waits for response during
// the given timeout duration.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// Client determines the server to be requested in the following order:
//
//   - from RequestURI if it contains full url with scheme and host;
//   - from Host header otherwise.
//
// ErrTimeout is returned if the response wasn't returned during
// the given timeout.
func (c *Client) DoTimeout(req *Request, resp *Response, timeout time.Duration) error {
	return clientDoTimeout(req, resp, timeout, c)
}

// Do performs the given http request and fills the given http response.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// Response is ignored if resp is nil.
//
// Client determines the server to be requested in the following order:
//
//   - from RequestURI if it contains full url with scheme and host;
//   - from Host header otherwise.
//
// ErrNoFreeConns is returned if all Client.MaxConnsPerHost connections
// to the requested host are busy.
func (c *Client) Do(req *Request, resp *Response) error {
	uri := req.URI()
	host := uri.Host()

	isTLS := false
	if bytes.Equal(uri.Scheme, strHTTPS) {
		isTLS = true
	} else if !bytes.Equal(uri.Scheme, strHTTP) {
		return fmt.Errorf("unsupported protocol %q. http and https are supported", uri.Scheme)
	}

	startCleaner := false

	c.mLock.Lock()
	m := c.m
	if isTLS {
		m = c.ms
	}
	if m == nil {
		m = make(map[string]*HostClient)
		if isTLS {
			c.ms = m
		} else {
			c.m = m
		}
	}
	hc := m[string(host)]
	if hc == nil {
		hc = &HostClient{
			Addr:            addMissingPort(string(host), isTLS),
			Name:            c.Name,
			Dial:            c.Dial,
			TLSConfig:       c.TLSConfig,
			MaxConns:        c.MaxConnsPerHost,
			ReadBufferSize:  c.ReadBufferSize,
			WriteBufferSize: c.WriteBufferSize,
			Logger:          c.Logger,
		}
		if isTLS {
			hc.IsTLS = true
		}
		m[hc.Addr] = hc
		if len(m) == 1 {
			startCleaner = true
		}
	}
	c.mLock.Unlock()

	if startCleaner {
		go c.mCleaner(m)
	}

	return hc.Do(req, resp)
}

func (c *Client) mCleaner(m map[string]*HostClient) {
	mustStop := false
	for {
		t := time.Now()
		c.mLock.Lock()
		for k, v := range m {
			if t.Sub(v.LastUseTime()) > time.Minute {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			mustStop = true
		}
		c.mLock.Unlock()

		if mustStop {
			break
		}
		time.Sleep(10 * time.Second)
	}
}

// Maximum number of concurrent connections http client can establish per host
// by default.
const DefaultMaxConnsPerHost = 100

// DialFunc must establish connection to addr.
//
// There is no need in establishing TLS (SSL) connection for https.
// The client automatically converts connection to TLS
// if HostClient.IsTLS is set.
//
// TCP address passed to DialFunc always contain host and port.
// Example TCP addr values:
//
//   - foobar.com:80
//   - foobar.com:443
//   - foobar.com:8080
type DialFunc func(addr string) (net.Conn, error)

// HostClient is a single-host http client. It can make http requests
// to the given Addr only.
//
// It is forbidden copying HostClient instances.
type HostClient struct {
	// HTTP server host address, which is passed to Dial.
	//
	// The address MUST contain port if it is TCP. For example,
	//
	//    - foobar.com:80
	//    - foobar.com:443
	//    - foobar.com:8080
	Addr string

	// Client name. Used in User-Agent request header.
	Name string

	// Callback for establishing new connection to the host.
	//
	// Default TCP dialer is used if not set.
	Dial DialFunc

	// Whether to use TLS (aka SSL or HTTPS) for host connections.
	IsTLS bool

	// Optional TLS config.
	TLSConfig *tls.Config

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

	clientName  atomic.Value
	lastUseTime uint64

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

// LastUseTime returns time the client was last used
func (c *HostClient) LastUseTime() time.Time {
	n := atomic.LoadUint64(&c.lastUseTime)
	return time.Unix(int64(n), 0)
}

// Get fetches url contents into dst.
//
// Use Do for request customization.
func (c *HostClient) Get(dst []byte, url string) (statusCode int, body []byte, err error) {
	return clientGetURL(dst, url, c)
}

// Post sends POST request to the given url with the given POST arguments.
//
// Empty POST body is sent if postArgs is nil.
//
// Use Do for request customization.
func (c *HostClient) Post(dst []byte, url string, postArgs *Args) (statusCode int, body []byte, err error) {
	return clientPostURL(dst, url, postArgs, c)
}

type clientDoer interface {
	Do(req *Request, resp *Response) error
}

func clientGetURL(dst []byte, url string, c clientDoer) (statusCode int, body []byte, err error) {
	req := acquireRequest()

	statusCode, body, err = doRequest(req, dst, url, c)

	releaseRequest(req)
	return statusCode, body, err
}

func clientPostURL(dst []byte, url string, postArgs *Args, c clientDoer) (statusCode int, body []byte, err error) {
	req := acquireRequest()
	req.Header.SetMethodBytes(strPost)
	req.Header.SetContentTypeBytes(strPostArgsContentType)
	if postArgs != nil {
		postArgs.WriteTo(req.BodyWriter())
	}

	statusCode, body, err = doRequest(req, dst, url, c)

	releaseRequest(req)
	return statusCode, body, err
}

func doRequest(req *Request, dst []byte, url string, c clientDoer) (statusCode int, body []byte, err error) {
	req.SetRequestURI(url)

	resp := acquireResponse()
	oldBody := resp.body
	resp.body = dst
	if err = c.Do(req, resp); err != nil {
		return 0, nil, err
	}
	statusCode = resp.Header.StatusCode()
	body = resp.body
	resp.body = oldBody
	releaseResponse(resp)

	return statusCode, body, err
}

var (
	requestPool  sync.Pool
	responsePool sync.Pool
)

func acquireRequest() *Request {
	v := requestPool.Get()
	if v == nil {
		return &Request{}
	}
	return v.(*Request)
}

func releaseRequest(req *Request) {
	req.Reset()
	requestPool.Put(req)
}

func acquireResponse() *Response {
	v := responsePool.Get()
	if v == nil {
		return &Response{}
	}
	return v.(*Response)
}

func releaseResponse(resp *Response) {
	resp.Reset()
	responsePool.Put(resp)
}

// DoTimeout performs the given request and waits for response during
// the given timeout duration.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// ErrTimeout is returned if the response wasn't returned during
// the given timeout.
func (c *HostClient) DoTimeout(req *Request, resp *Response, timeout time.Duration) error {
	return clientDoTimeout(req, resp, timeout, c)
}

func clientDoTimeout(req *Request, resp *Response, timeout time.Duration, c clientDoer) error {
	var ch chan error
	chv := errorChPool.Get()
	if chv == nil {
		ch = make(chan error, 1)
	} else {
		ch = chv.(chan error)
	}

	// make req and resp copies, since on timeout they no longer
	// may accessed.
	reqCopy := acquireRequest()
	req.CopyTo(reqCopy)
	respCopy := acquireResponse()

	go func() {
		ch <- c.Do(reqCopy, respCopy)
	}()

	var tc *time.Timer
	tcv := timerPool.Get()
	if tcv == nil {
		tc = time.NewTimer(timeout)
	} else {
		tc = tcv.(*time.Timer)
		initTimer(tc, timeout)
	}

	var err error
	select {
	case err = <-ch:
		resp.CopyTo(respCopy)
		releaseResponse(respCopy)
		releaseRequest(reqCopy)
		errorChPool.Put(chv)
	case <-tc.C:
		err = ErrTimeout
	}

	stopTimer(tc)
	timerPool.Put(tcv)

	return err
}

var (
	errorChPool sync.Pool
	timerPool   sync.Pool
)

// Do performs the given http request and sets the corresponding response.
//
// Request must contain at least non-zero RequestURI with full url (including
// scheme and host) or non-zero Host header + RequestURI.
//
// Response is ignored if resp is nil.
//
// ErrNoFreeConns is returned if all HostClient.MaxConns connections
// to the host are busy.
func (c *HostClient) Do(req *Request, resp *Response) error {
	retry, err := c.do(req, resp, false)
	if err != nil && retry && (req.Header.IsGet() || req.Header.IsHead()) {
		_, err = c.do(req, resp, true)
	}
	return err
}

func (c *HostClient) do(req *Request, resp *Response, newConn bool) (bool, error) {
	if req == nil {
		panic("BUG: req cannot be nil")
	}

	atomic.StoreUint64(&c.lastUseTime, uint64(time.Now().Unix()))

	cc, err := c.acquireConn(newConn)
	if err != nil {
		return false, err
	}
	conn := cc.c

	userAgentOld := req.Header.UserAgent()
	if len(userAgentOld) == 0 {
		req.Header.userAgent = c.getClientName()
	}
	bw := c.acquireWriter(conn)
	err = req.Write(bw)
	if len(userAgentOld) == 0 {
		req.Header.userAgent = userAgentOld
	}

	if err != nil {
		c.releaseWriter(bw)
		c.closeConn(cc)
		return false, err
	}
	if err = bw.Flush(); err != nil {
		c.releaseWriter(bw)
		c.closeConn(cc)
		return true, err
	}
	c.releaseWriter(bw)

	nilResp := false
	if resp == nil {
		nilResp = true
		resp = acquireResponse()
	}

	br := c.acquireReader(conn)
	if err = resp.Read(br); err != nil {
		if nilResp {
			releaseResponse(resp)
		}
		c.releaseReader(br)
		c.closeConn(cc)
		if err == io.EOF {
			return true, err
		}
		return false, err
	}
	c.releaseReader(br)

	if req.Header.ConnectionClose() || resp.Header.ConnectionClose() {
		c.closeConn(cc)
	} else {
		c.releaseConn(cc)
	}

	if nilResp {
		releaseResponse(resp)
	}
	return false, err
}

var (
	// ErrNoFreeConns is returned when no free connections available
	// to the given host.
	ErrNoFreeConns = errors.New("no free connections available to host")

	// ErrTimeout is returned from timed out calls.
	ErrTimeout = errors.New("timeout")
)

func (c *HostClient) acquireConn(newConn bool) (*clientConn, error) {
	var cc *clientConn
	createConn := false
	startCleaner := false

	c.connsLock.Lock()
	n := len(c.conns)
	if n == 0 || newConn {
		maxConns := c.MaxConns
		if maxConns <= 0 {
			maxConns = DefaultMaxConnsPerHost
		}
		if c.connsCount < maxConns {
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

	conn, err := c.dialHost()
	if err != nil {
		c.decConnsCount()
		return nil, err
	}
	cc = acquireClientConn(conn)

	if startCleaner {
		go c.connsCleaner()
	}
	return cc, nil
}

func (c *HostClient) connsCleaner() {
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
		if c.connsCount == 0 {
			mustStop = true
		}
		c.connsLock.Unlock()

		if mustStop {
			break
		}
		time.Sleep(time.Second)
	}
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
	cc := v.(*clientConn)
	cc.c = conn
	return cc
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

var defaultTLSConfig = &tls.Config{
	InsecureSkipVerify: true,
}

func (c *HostClient) dialHost() (net.Conn, error) {
	dial := c.Dial
	if dial == nil {
		dial = c.defaultDialFunc
	}
	conn, err := dial(c.Addr)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		panic("BUG: DialFunc returned (nil, nil)")
	}
	if c.IsTLS {
		tlsConfig := c.TLSConfig
		if tlsConfig == nil {
			tlsConfig = defaultTLSConfig
		}
		conn = tls.Client(conn, tlsConfig)
	}
	return conn, nil
}

func (c *HostClient) defaultDialFunc(addr string) (net.Conn, error) {
	tcpAddr, err := c.getTCPAddr(addr)
	if err != nil {
		return nil, err
	}
	return net.DialTCP("tcp4", nil, tcpAddr)
}

func (c *HostClient) getTCPAddr(addr string) (*net.TCPAddr, error) {
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
	return tcpAddr, nil
}

func addMissingPort(addr string, isTLS bool) string {
	n := strings.Index(addr, ":")
	if n >= 0 {
		return addr
	}
	port := 80
	if isTLS {
		port = 443
	}
	return fmt.Sprintf("%s:%d", addr, port)
}

func resolveTCPAddrs(addr string) ([]net.TCPAddr, error) {
	host, portS, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portS)
	if err != nil {
		return nil, err
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	n := len(ips)
	addrs := make([]net.TCPAddr, n)
	for i := 0; i < n; i++ {
		addrs[i].IP = ips[i]
		addrs[i].Port = port
	}
	return addrs, nil
}

func (c *HostClient) getClientName() []byte {
	v := c.clientName.Load()
	var clientName []byte
	if v == nil {
		clientName = []byte(c.Name)
		if len(clientName) == 0 {
			clientName = defaultUserAgent
		}
		c.clientName.Store(clientName)
	} else {
		clientName = v.([]byte)
	}
	return clientName
}
