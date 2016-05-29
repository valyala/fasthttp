package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AcquireRequest returns an empty Request instance from request pool.
//
// The returned Request instance may be passed to ReleaseRequest when it is
// no longer needed. This allows Request recycling, reduces GC pressure
// and usually improves performance.
func AcquireRequest() *Request {
	v := requestPool.Get()
	if v == nil {
		return &Request{}
	}
	return v.(*Request)
}

// AcquireResponse returns an empty Response instance from response pool.
//
// The returned Response instance may be passed to ReleaseResponse when it is
// no longer needed. This allows Response recycling, reduces GC pressure
// and usually improves performance.
func AcquireResponse() *Response {
	v := responsePool.Get()
	if v == nil {
		return &Response{}
	}
	return v.(*Response)
}

// ReleaseResponse return resp acquired via AcquireResponse to response pool.
//
// It is forbidden accessing resp and/or its' members after returning
// it to response pool.
func ReleaseResponse(resp *Response) {
	resp.Reset()
	responsePool.Put(resp)
}

var (
	requestPool  sync.Pool
	responsePool sync.Pool
)

type clientConn struct {
	c net.Conn

	createdTime time.Time
	lastUseTime time.Time

	lastReadDeadlineTime  time.Time
	lastWriteDeadlineTime time.Time
}

var startTimeUnix = time.Now().Unix()

// DefaultMaxConnsPerHost is the maximum number of concurrent connections
// http client may establish per host by default (i.e. if
// Client.MaxConnsPerHost isn't set).
const DefaultMaxConnsPerHost = 512

// DefaultMaxIdleConnDuration is the default duration before idle keep-alive
// connection is closed.
const DefaultMaxIdleConnDuration = 10 * time.Second

// DialFunc must establish connection to addr.
//
// There is no need in establishing TLS (SSL) connection for https.
// The client automatically converts connection to TLS
// if HostClient.IsTLS is set.
//
// TCP address passed to DialFunc always contains host and port.
// Example TCP addr values:
//
//   - foobar.com:80
//   - foobar.com:443
//   - foobar.com:8080
type DialFunc func(addr string) (net.Conn, error)

// ProxyClient is a client used in revese proxies.
//
// ProxyClint provides methods for sending requests, reading response headers
// and reading partial response body.
//
// It is forbidden copying ProxyClient instances. Create new instances instead.
//
// It is safe calling ProxyClient methods from concurrently running goroutines.
type ProxyClient struct {
	// Comma-separated list of upstream HTTP server host addresses,
	// which are passed to Dial in round-robin manner.
	//
	// Each address may contain port if default dialer is used.
	// For example,
	//
	//    - foobar.com:80
	//    - foobar.com:443
	//    - foobar.com:8080
	Addr string

	// Client name. Used in User-Agent request header.
	Name string

	// Callback for establishing new connection to the host.
	//
	// Default Dial is used if not set.
	Dial DialFunc

	// Attempt to connect to both ipv4 and ipv6 host addresses
	// if set to true.
	//
	// This option is used only if default TCP dialer is used,
	// i.e. if Dial is blank.
	//
	// By default client connects only to ipv4 addresses,
	// since unfortunately ipv6 remains broken in many networks worldwide :)
	DialDualStack bool

	// Whether to use TLS (aka SSL or HTTPS) for host connections.
	IsTLS bool

	// Optional TLS config.
	TLSConfig *tls.Config

	// Maximum number of connections which may be established to all hosts
	// listed in Addr.
	//
	// DefaultMaxConnsPerHost is used if not set.
	MaxConns int

	// Keep-alive connections are closed after this duration.
	//
	// By default connection duration is unlimited.
	MaxConnDuration time.Duration

	// Idle keep-alive connections are closed after this duration.
	//
	// By default idle connections are closed
	// after DefaultMaxIdleConnDuration.
	MaxIdleConnDuration time.Duration

	// Per-connection buffer size for responses' reading.
	// This also limits the maximum header size.
	//
	// Default buffer size is used if 0.
	ReadBufferSize int

	// Per-connection buffer size for requests' writing.
	//
	// Default buffer size is used if 0.
	WriteBufferSize int

	// Maximum duration for full response reading (including body).
	//
	// By default response read timeout is unlimited.
	ReadTimeout time.Duration

	// Maximum duration for full request writing (including body).
	//
	// By default request write timeout is unlimited.
	WriteTimeout time.Duration

	// Maximum response body size.
	//
	// The client returns ErrBodyTooLarge if this limit is greater than 0
	// and response body is greater than the limit.
	//
	// By default response body size is unlimited.
	MaxResponseBodySize int

	// Header names are passed as-is without normalization
	// if this option is set.
	//
	// Disabled header names' normalization may be useful only for proxying
	// responses to other clients expecting case-sensitive
	// header names. See https://github.com/valyala/fasthttp/issues/57
	// for details.
	//
	// By default request and response header names are normalized, i.e.
	// The first letter and the first letters following dashes
	// are uppercased, while all the other letters are lowercased.
	// Examples:
	//
	//     * HOST -> Host
	//     * content-type -> Content-Type
	//     * cONTENT-lenGTH -> Content-Length
	DisableHeaderNamesNormalizing bool

	clientName  atomic.Value
	lastUseTime uint32

	connsLock  sync.Mutex
	connsCount int
	conns      []*clientConn

	addrsLock sync.Mutex
	addrs     []string
	addrIdx   uint32

	readerPool sync.Pool
	writerPool sync.Pool
}

// Do sends a request and read response headers.
// The caller must pass valid request and response.
// The response body can be read from the resp.BodyStream().
// The user must call ProxyClient.CleanupResponse() for clean up.
func (c *ProxyClient) Do(req *Request, resp *Response) error {
	if req == nil {
		panic("BUG: req cannot be nil")
	}
	if resp == nil {
		panic("BUG: resp cannot be nil")
	}

	atomic.StoreUint32(&c.lastUseTime, uint32(time.Now().Unix()-startTimeUnix))

	cc, err := c.acquireConn()
	if err != nil {
		return err
	}
	conn := cc.c

	if c.WriteTimeout > 0 {
		// Optimization: update write deadline only if more than 25%
		// of the last write deadline exceeded.
		// See https://github.com/golang/go/issues/15133 for details.
		currentTime := time.Now()
		if currentTime.Sub(cc.lastWriteDeadlineTime) > (c.WriteTimeout >> 2) {
			if err = conn.SetWriteDeadline(currentTime.Add(c.WriteTimeout)); err != nil {
				c.closeConn(cc)
				return err
			}
			cc.lastWriteDeadlineTime = currentTime
		}
	}

	resetConnection := false
	if c.MaxConnDuration > 0 && time.Since(cc.createdTime) > c.MaxConnDuration && !req.ConnectionClose() {
		req.SetConnectionClose()
		resetConnection = true
	}

	userAgentOld := req.Header.UserAgent()
	if len(userAgentOld) == 0 {
		req.Header.userAgent = c.getClientName()
	}
	bw := c.acquireWriter(conn)
	err = req.Write(bw)
	if len(userAgentOld) == 0 {
		req.Header.userAgent = userAgentOld
	}

	if resetConnection {
		req.Header.ResetConnectionClose()
	}

	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		c.releaseWriter(bw)
		c.closeConn(cc)
		return err
	}
	c.releaseWriter(bw)

	if c.ReadTimeout > 0 {
		// Optimization: update read deadline only if more than 25%
		// of the last read deadline exceeded.
		// See https://github.com/golang/go/issues/15133 for details.
		currentTime := time.Now()
		if currentTime.Sub(cc.lastReadDeadlineTime) > (c.ReadTimeout >> 2) {
			if err = conn.SetReadDeadline(currentTime.Add(c.ReadTimeout)); err != nil {
				c.closeConn(cc)
				return err
			}
			cc.lastReadDeadlineTime = currentTime
		}
	}

	if !req.Header.IsGet() && req.Header.IsHead() {
		resp.SkipBody = true
	}
	if c.DisableHeaderNamesNormalizing {
		resp.Header.DisableNormalizing()
	}

	br := c.acquireReader(conn)
	if err = resp.ReadHeader(br); err != nil {
		c.releaseReader(br)
		c.closeConn(cc)
		return err
	}

	if err = readTransferResponse(req, resp, br); err != nil {
		c.releaseReader(br)
		c.closeConn(cc)
		return err
	}

	resp.cc = cc
	resp.resetConnection = resetConnection
	resp.br = br
	return nil
}

// CleanupResponse releases the reader and closes or releases the connection used.
func (c *ProxyClient) CleanupResponse(req *Request, resp *Response) {
	if resp.br != nil {
		c.releaseReader(resp.br)
		resp.br = nil
	}

	if resp.cc != nil {
		if !resp.sawEOF || resp.resetConnection || req.ConnectionClose() || resp.ConnectionClose() {
			c.closeConn(resp.cc)
		} else {
			c.releaseConn(resp.cc)
		}
		resp.cc = nil
	}
	resp.sawEOF = false
	resp.resetConnection = false
}

var (
	// ErrNoFreeConns is returned when no free connections available
	// to the given host.
	ErrNoFreeConns = errors.New("no free connections available to host")

	//// ErrTimeout is returned from timed out calls.
	//ErrTimeout = errors.New("timeout")

	// ErrConnectionClosed may be returned from client methods if the server
	// closes connection before returning the first response byte.
	//
	// If you see this error, then either fix the server by returning
	// 'Connection: close' response header before closing the connection
	// or add 'Connection: close' request header before sending requests
	// to broken server.
	ErrConnectionClosed = errors.New("the server closed connection before returning the first response byte. " +
		"Make sure the server returns 'Connection: close' response header before closing the connection")
)

func (c *ProxyClient) acquireConn() (*clientConn, error) {
	var cc *clientConn
	createConn := false
	startCleaner := false

	var n int
	c.connsLock.Lock()
	n = len(c.conns)
	if n == 0 {
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

	conn, err := c.dialHostHard()
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

func (c *ProxyClient) connsCleaner() {
	var (
		scratch             []*clientConn
		mustStop            bool
		maxIdleConnDuration = c.MaxIdleConnDuration
	)
	if maxIdleConnDuration <= 0 {
		maxIdleConnDuration = DefaultMaxIdleConnDuration
	}
	for {
		currentTime := time.Now()

		c.connsLock.Lock()
		conns := c.conns
		n := len(conns)
		i := 0
		for i < n && currentTime.Sub(conns[i].lastUseTime) > maxIdleConnDuration {
			i++
		}
		mustStop = (c.connsCount == i)
		scratch = append(scratch[:0], conns[:i]...)
		if i > 0 {
			m := copy(conns, conns[i:])
			for i = m; i < n; i++ {
				conns[i] = nil
			}
			c.conns = conns[:m]
		}
		c.connsLock.Unlock()

		for i, cc := range scratch {
			c.closeConn(cc)
			scratch[i] = nil
		}
		if mustStop {
			break
		}
		time.Sleep(maxIdleConnDuration)
	}
}

func (c *ProxyClient) closeConn(cc *clientConn) {
	c.decConnsCount()
	cc.c.Close()
	releaseClientConn(cc)
}

func (c *ProxyClient) decConnsCount() {
	c.connsLock.Lock()
	c.connsCount--
	c.connsLock.Unlock()
}

func acquireClientConn(conn net.Conn) *clientConn {
	v := clientConnPool.Get()
	if v == nil {
		v = &clientConn{}
	}
	cc := v.(*clientConn)
	cc.c = conn
	cc.createdTime = time.Now()
	return cc
}

func releaseClientConn(cc *clientConn) {
	cc.c = nil
	clientConnPool.Put(cc)
}

var clientConnPool sync.Pool

func (c *ProxyClient) releaseConn(cc *clientConn) {
	cc.lastUseTime = time.Now()
	c.connsLock.Lock()
	c.conns = append(c.conns, cc)
	c.connsLock.Unlock()
}

func (c *ProxyClient) acquireWriter(conn net.Conn) *bufio.Writer {
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

func (c *ProxyClient) getClientName() []byte {
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

func (c *ProxyClient) releaseWriter(bw *bufio.Writer) {
	c.writerPool.Put(bw)
}

func (c *ProxyClient) acquireReader(conn net.Conn) *bufio.Reader {
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

func (c *ProxyClient) releaseReader(br *bufio.Reader) {
	c.readerPool.Put(br)
}

func newDefaultTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
	}
}

func (c *ProxyClient) nextAddr() string {
	c.addrsLock.Lock()
	if c.addrs == nil {
		c.addrs = strings.Split(c.Addr, ",")
	}
	addr := c.addrs[0]
	if len(c.addrs) > 1 {
		addr = c.addrs[c.addrIdx%uint32(len(c.addrs))]
		c.addrIdx++
	}
	c.addrsLock.Unlock()
	return addr
}

func (c *ProxyClient) dialHostHard() (conn net.Conn, err error) {
	// attempt to dial all the available hosts before giving up.

	c.addrsLock.Lock()
	n := len(c.addrs)
	c.addrsLock.Unlock()

	if n == 0 {
		// It looks like c.addrs isn't initialized yet.
		n = 1
	}

	timeout := c.ReadTimeout + c.WriteTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	deadline := time.Now().Add(timeout)
	for n > 0 {
		addr := c.nextAddr()
		conn, err = dialAddr(addr, c.Dial, c.DialDualStack, c.IsTLS, c.TLSConfig)
		if err == nil {
			return conn, nil
		}
		if time.Since(deadline) >= 0 {
			break
		}
		n--
	}
	return nil, err
}

func dialAddr(addr string, dial DialFunc, dialDualStack, isTLS bool, tlsConfig *tls.Config) (net.Conn, error) {
	if dial == nil {
		if dialDualStack {
			dial = DialDualStack
		} else {
			dial = Dial
		}
		addr = addMissingPort(addr, isTLS)
	}
	conn, err := dial(addr)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		panic("BUG: DialFunc returned (nil, nil)")
	}
	if isTLS {
		if tlsConfig == nil {
			tlsConfig = newDefaultTLSConfig()
		}
		conn = tls.Client(conn, tlsConfig)
	}
	return conn, nil
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
