package fasthttp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServeConn serves HTTP requests from the given connection
// using the given handler.
//
// ServeConn returns nil if all requests from the c are successfully served.
// It returns non-nil error otherwise.
//
// Connection c must immediately propagate all the data passed to Write()
// to the client. Otherwise requests' processing may hang.
//
// ServeConn closes c before returning.
//
// ServeConn uses default Server settings. Use Server.ServeConn
// for custom server tuning.
func ServeConn(c net.Conn, handler RequestHandler) error {
	v := serverPool.Get()
	if v == nil {
		v = &Server{}
	}
	s := v.(*Server)
	s.Handler = handler
	err := s.ServeConn(c)
	s.Handler = nil
	serverPool.Put(v)
	return err
}

var serverPool sync.Pool

// Serve serves incoming connections from the given listener
// using the given handler.
//
// Serve blocks until the given listener returns permanent error.
//
// Serve uses default Server settings. Use Server.Serve
// for custom server tuning.
func Serve(ln net.Listener, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.Serve(ln)
}

// ListenAndServe serves HTTP requests from the given TCP addr
// using the given handler.
//
// ListenAndServe uses default Server settings. Use Server.ListenAndServe
// for custom server tuning.
func ListenAndServe(addr string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServe(addr)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr
// using the given handler.
//
// certFile and keyFile are paths to TLS certificate and key files.
//
// ListenAndServeTLS uses default Server settings. Use Server.ListenAndServeTLS
// for custom server tuning.
func ListenAndServeTLS(addr, certFile, keyFile string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}
	return s.ListenAndServeTLS(addr, certFile, keyFile)
}

// RequestHandler must process incoming requests.
//
// RequestHandler must call ctx.TimeoutError() before return
// if it keeps references to ctx and/or its' members after the return.
// Consider wrapping RequestHandler into TimeoutHandler if response time
// must be limited.
type RequestHandler func(ctx *RequestCtx)

// Server implements HTTP server.
//
// It is forbidden copying Server instances. Create new Server instances
// instead.
type Server struct {
	// Handler for processing incoming requests.
	Handler RequestHandler

	// Server name for sending in response headers.
	//
	// Default server name is used if left blank.
	Name string

	// The maximum number of concurrent connections the server may serve.
	//
	// DefaultConcurrency is used if not set.
	Concurrency int

	// Per-connection buffer size for requests' reading.
	// This also limits the maximum header size.
	//
	// Default buffer size is used if 0.
	ReadBufferSize int

	// Per-connection buffer size for responses' writing.
	//
	// Default buffer size is used if 0.
	WriteBufferSize int

	// Maximum duration for full request reading (including body).
	//
	// By default request read timeout is unlimited.
	ReadTimeout time.Duration

	// Maximum duration for full response writing (including body).
	//
	// By default response write timeout is unlimited.
	WriteTimeout time.Duration

	// Maximum number of concurrent client connections allowed per IP.
	//
	// By default unlimited number of concurrent connections
	// may be established to the server from a single IP address.
	MaxConnsPerIP int

	// Logger, which is used by RequestCtx.Logger().
	//
	// By default standard logger from log package is used.
	Logger Logger

	concurrency      uint32
	perIPConnCounter perIPConnCounter
	serverName       atomic.Value

	ctxPool    sync.Pool
	readerPool sync.Pool
	writerPool sync.Pool
}

// TimeoutHandler creates RequestHandler, which returns StatusRequestTimeout
// error with the given msg in the body to the client if h didn't return
// during the given duration.
func TimeoutHandler(h RequestHandler, timeout time.Duration, msg string) RequestHandler {
	if timeout <= 0 {
		return h
	}

	return func(ctx *RequestCtx) {
		ch := ctx.timeoutCh
		if ch == nil {
			ch = make(chan struct{}, 1)
			ctx.timeoutCh = ch
		}
		go func() {
			h(ctx)
			ch <- struct{}{}
		}()
		ctx.timeoutTimer = initTimer(ctx.timeoutTimer, timeout)
		select {
		case <-ch:
		case <-ctx.timeoutTimer.C:
			ctx.TimeoutError(msg)
		}
		stopTimer(ctx.timeoutTimer)
	}
}

// RequestCtx contains incoming request and manages outgoing response.
//
// It is forbidden copying RequestCtx instances.
//
// RequestHandler should avoid holding references to incoming RequestCtx and/or
// its' members after the return.
// If holding RequestCtx references after the return is unavoidable
// (for instance, ctx is passed to a separate goroutine and we cannot control
// ctx lifetime in this goroutine), then the RequestHandler MUST call
// ctx.TimeoutError() before return.
type RequestCtx struct {
	// Incoming request.
	Request Request

	// Outgoing response.
	Response Response

	// Unique id of the request.
	ID uint64

	// Start time for the request processing.
	Time time.Time

	logger ctxLogger
	s      *Server
	c      net.Conn
	fbr    firstByteReader

	timeoutErrMsg string
	timeoutCh     chan struct{}
	timeoutTimer  *time.Timer

	v interface{}
}

type firstByteReader struct {
	c        net.Conn
	ch       byte
	byteRead bool
}

func (r *firstByteReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	nn := 0
	if !r.byteRead {
		b[0] = r.ch
		b = b[1:]
		r.byteRead = true
		nn = 1
	}
	n, err := r.c.Read(b)
	return n + nn, err
}

// Logger is used for logging formatted messages.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...interface{})
}

var ctxLoggerLock sync.Mutex

type ctxLogger struct {
	ctx    *RequestCtx
	logger Logger
}

func (cl *ctxLogger) Printf(format string, args ...interface{}) {
	ctxLoggerLock.Lock()
	s := fmt.Sprintf(format, args...)
	ctx := cl.ctx
	req := &ctx.Request
	cl.logger.Printf("%.3f #%016X - %s<->%s - %s %s - %s",
		time.Since(ctx.Time).Seconds(), ctx.ID, ctx.LocalAddr(), ctx.RemoteAddr(), req.Header.Method(), ctx.URI().FullURI(), s)
	ctxLoggerLock.Unlock()
}

var zeroTCPAddr = &net.TCPAddr{
	IP: net.IPv4zero,
}

// SetConnectionClose sets 'Connection: close' response header and closes
// connection after the RequestHandler returns.
func (ctx *RequestCtx) SetConnectionClose() {
	ctx.Request.Header.SetConnectionClose()
}

// SetStatusCode sets response status code.
func (ctx *RequestCtx) SetStatusCode(statusCode int) {
	ctx.Response.Header.StatusCode = statusCode
}

// SetContentType sets response Content-Type.
func (ctx *RequestCtx) SetContentType(contentType string) {
	ctx.Response.Header.SetContentType(contentType)
}

// SetContentTypeBytes sets response Content-Type.
//
// It is safe modifying contentType buffer after function return.
func (ctx *RequestCtx) SetContentTypeBytes(contentType []byte) {
	ctx.Response.Header.SetContentTypeBytes(contentType)
}

// RequestURI returns RequestURI.
//
// This uri is valid until returning from RequestHandler.
func (ctx *RequestCtx) RequestURI() []byte {
	return ctx.Request.Header.RequestURI()
}

// URI returns requested uri.
func (ctx *RequestCtx) URI() *URI {
	return ctx.Request.URI()
}

// Path returns requested path.
//
// The path is valid until returning from RequestHandler.
func (ctx *RequestCtx) Path() []byte {
	return ctx.URI().Path
}

// Host returns requested host.
//
// The host is valid until returning from RequestHandler.
func (ctx *RequestCtx) Host() []byte {
	return ctx.URI().Host
}

// QueryArgs returns query arguments from RequestURI.
//
// It doesn't return POST'ed arguments - use PostArge() for this.
//
// Returned arguments are valid until returning from RequestHandler.
func (ctx *RequestCtx) QueryArgs() *Args {
	return ctx.URI().QueryArgs()
}

// PostArgs returns POST arguments.
//
// It doesn't return query arguments from RequestURI - use QueryArgs for this.
//
// Returned arguments are valid until returning from RequestHandler.
func (ctx *RequestCtx) PostArgs() *Args {
	return ctx.Request.PostArgs()
}

// IsGet returns true if request method is GET.
func (ctx *RequestCtx) IsGet() bool {
	return ctx.Request.Header.IsGet()
}

// IsPost returns true if request method is POST.
func (ctx *RequestCtx) IsPost() bool {
	return ctx.Request.Header.IsPost()
}

// Method return request method.
//
// Returned value is valid until returning from RequestHandler.
func (ctx *RequestCtx) Method() []byte {
	return ctx.Request.Header.Method()
}

// IsHead returns true if request method is HEAD.
func (ctx *RequestCtx) IsHead() bool {
	return ctx.Request.Header.IsHead()
}

// RemoteAddr returns client address for the given request.
//
// Always returns non-nil result.
func (ctx *RequestCtx) RemoteAddr() net.Addr {
	addr := ctx.c.RemoteAddr()
	if addr == nil {
		return zeroTCPAddr
	}
	return addr
}

// LocalAddr returns server address for the given request.
//
// Always returns non-nil result.
func (ctx *RequestCtx) LocalAddr() net.Addr {
	addr := ctx.c.LocalAddr()
	if addr == nil {
		return zeroTCPAddr
	}
	return addr
}

// RemoteIP returns client ip for the given request.
//
// Always returns non-nil result.
func (ctx *RequestCtx) RemoteIP() net.IP {
	x, ok := ctx.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return net.IPv4zero
	}
	return x.IP
}

// Error sets response status code to the given value and sets response body
// to the given message.
//
// Error calls are ignored after TimeoutError call.
func (ctx *RequestCtx) Error(msg string, statusCode int) {
	resp := &ctx.Response
	resp.Clear()
	resp.Header.StatusCode = statusCode
	resp.Header.SetContentTypeBytes(defaultContentType)
	resp.Body = AppendBytesStr(resp.Body[:0], msg)
}

// Success sets response Content-Type and body to the given values.
//
// It is safe modifying body buffer after the Success() call.
//
// Success calls are ignored after TimeoutError call.
func (ctx *RequestCtx) Success(contentType string, body []byte) {
	ctx.SetContentTypeBytes(contentType)
	ctx.SetResponseBody(body)
}

// SetResponseBody sets response body to the given value.
//
// It is safe modifying body buffer after the function return.
func (ctx *RequestCtx) SetResponseBody(body []byte) {
	ctx.Response.Body = append(ctx.Response.Body[:0], body...)
}

// Logger returns logger, which may be used for logging arbitrary
// request-specific messages inside RequestHandler.
//
// Each message logged via returned logger contains request-specific information
// such as request id, remote address, request method and request url.
//
// It is safe re-using returned logger for logging multiple messages.
func (ctx *RequestCtx) Logger() Logger {
	if ctx.logger.ctx == nil {
		ctx.logger.ctx = ctx
	}
	if ctx.logger.logger == nil {
		ctx.logger.logger = ctx.s.logger()
	}
	return &ctx.logger
}

// TimeoutError sets response status code to StatusRequestTimeout and sets
// body to the given msg.
//
// All response modifications after TimeoutError call are ignored.
//
// TimeoutError MUST be called before returning from RequestHandler if there are
// references to ctx and/or its members in other goroutines.
func (ctx *RequestCtx) TimeoutError(msg string) {
	ctx.timeoutErrMsg = msg
}

// ListenAndServe serves HTTP requests from the given TCP addr.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// ListenAndServeTLS serves HTTPS requests from the given TCP addr.
//
// certFile and keyFile are paths to TLS certificate and key files.
func (s *Server) ListenAndServeTLS(addr, certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Default maximum number of concurrent connections the Server may serve.
const DefaultConcurrency = 256 * 1024

// Serve serves incoming connections from the given listener.
//
// Serve may handle DefaultConcurrency concurrent connections.
// Use ServeConcurrency for handling another number of concurrent connections.
//
// Serve blocks until the given listener returns permanent error.
func (s *Server) Serve(ln net.Listener) error {
	var lastOverflowErrorTime time.Time
	var lastPerIPErrorTime time.Time
	var c net.Conn
	var err error

	maxWorkersCount := s.getConcurrency()
	wp := &workerPool{
		WorkerFunc:      s.serveConn,
		MaxWorkersCount: maxWorkersCount,
		Logger:          s.logger(),
	}
	wp.Start()

	for {
		if c, err = acceptConn(s, ln, &lastPerIPErrorTime); err != nil {
			wp.Stop()
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !wp.Serve(c) {
			c.Close()
			if time.Since(lastOverflowErrorTime) > time.Minute {
				s.logger().Printf("The incoming connection cannot be served, because %d concurrent connections are served. "+
					"Try increasing Server.Concurrency", maxWorkersCount)
				lastOverflowErrorTime = time.Now()
			}
		}
		c = nil
	}
}

func acceptConn(s *Server, ln net.Listener, lastPerIPErrorTime *time.Time) (net.Conn, error) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				s.logger().Printf("Temporary error when accepting new connections: %s", netErr)
				time.Sleep(time.Second)
				continue
			}
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				s.logger().Printf("Permanent error when accepting new connections: %s", err)
				return nil, err
			}
			return nil, io.EOF
		}
		if c == nil {
			panic("BUG: net.Listener returned (nil, nil)")
		}
		if s.MaxConnsPerIP > 0 {
			pic := wrapPerIPConn(s, c)
			if pic == nil {
				c.Close()
				if time.Since(*lastPerIPErrorTime) > time.Minute {
					s.logger().Printf("The number of connections from %s exceeds MaxConnsPerIP=%d",
						getConnIP4(c), s.MaxConnsPerIP)
					*lastPerIPErrorTime = time.Now()
				}
				continue
			}
			return pic, nil
		}
		return c, nil
	}
}

func wrapPerIPConn(s *Server, c net.Conn) net.Conn {
	ip := getUint32IP(c)
	if ip == 0 {
		return c
	}
	n := s.perIPConnCounter.Register(ip)
	if n > s.MaxConnsPerIP {
		s.perIPConnCounter.Unregister(ip)
		return nil
	}
	return acquirePerIPConn(c, ip, &s.perIPConnCounter)
}

var defaultLogger = Logger(log.New(os.Stderr, "", log.LstdFlags))

func (s *Server) logger() Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return defaultLogger
}

var (
	// ErrPerIPConnLimit may be returned from ServeConn if the number of connections
	// per ip exceeds Server.MaxConnsPerIP.
	ErrPerIPConnLimit = errors.New("too many connections per ip")

	// ErrConcurrencyLimit may be returned from ServeConn if the number
	// of concurrenty served connections exceeds Server.Concurrency.
	ErrConcurrencyLimit = errors.New("canot serve the connection because Server.Concurrency concurrent connections are served")
)

// ServeConn serves HTTP requests from the given connection.
//
// ServeConn returns nil if all requests from the c are successfully served.
// It returns non-nil error otherwise.
//
// Connection c must immediately propagate all the data passed to Write()
// to the client. Otherwise requests' processing may hang.
//
// ServeConn closes c before returning.
func (s *Server) ServeConn(c net.Conn) error {
	if s.MaxConnsPerIP > 0 {
		pic := wrapPerIPConn(s, c)
		if pic == nil {
			c.Close()
			return ErrPerIPConnLimit
		}
		c = pic
	}

	n := atomic.AddUint32(&s.concurrency, 1)
	if n > uint32(s.getConcurrency()) {
		atomic.AddUint32(&s.concurrency, ^uint32(0))
		c.Close()
		return ErrConcurrencyLimit
	}

	err := s.serveConn(c)

	atomic.AddUint32(&s.concurrency, ^uint32(0))

	err1 := c.Close()
	if err == nil {
		err = err1
	}
	return err
}

func (s *Server) getConcurrency() int {
	n := s.Concurrency
	if n <= 0 {
		n = DefaultConcurrency
	}
	return n
}

func (s *Server) serveConn(c net.Conn) error {
	readTimeout := s.ReadTimeout
	writeTimeout := s.WriteTimeout

	ctx := s.acquireCtx(c)
	var br *bufio.Reader
	var bw *bufio.Writer
	var dt time.Duration
	var prevReadTime time.Time

	var err error
	var currentTime time.Time
	var connectionClose bool
	var errMsg string
	for {
		currentTime = time.Now()
		ctx.ID++
		ctx.Time = currentTime

		if readTimeout > 0 {
			if err = c.SetReadDeadline(currentTime.Add(readTimeout)); err != nil {
				break
			}
		}
		if dt < time.Second || br != nil {
			if br == nil {
				br = acquireReader(ctx)
			}
			err = ctx.Request.Read(br)
			if br.Buffered() == 0 || err != nil {
				releaseReader(ctx, br)
				br = nil
			}
		} else {
			ctx, br, err = acquireByteReader(ctx)
			if err == nil {
				err = ctx.Request.Read(br)
				if br.Buffered() == 0 || err != nil {
					releaseReader(ctx, br)
					br = nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}

		currentTime = time.Now()
		dt = currentTime.Sub(prevReadTime)
		prevReadTime = currentTime

		ctx.Time = currentTime
		ctx.Response.Clear()
		s.Handler(ctx)
		errMsg = ctx.timeoutErrMsg
		if len(errMsg) > 0 {
			ctx = s.acquireCtx(c)
			ctx.Error(errMsg, StatusRequestTimeout)
		}

		if writeTimeout > 0 {
			if err = c.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				break
			}
		}
		if bw == nil {
			bw = acquireWriter(ctx)
		}
		if err = writeResponse(ctx, bw); err != nil {
			break
		}
		connectionClose = ctx.Request.Header.ConnectionClose() || ctx.Response.Header.ConnectionClose()

		if br == nil || connectionClose {
			err = bw.Flush()
			releaseWriter(ctx, bw)
			bw = nil
			if err != nil {
				break
			}
			if connectionClose {
				break
			}
		}
	}

	if br != nil {
		releaseReader(ctx, br)
	}
	if bw != nil {
		releaseWriter(ctx, bw)
	}
	s.releaseCtx(ctx)
	return err
}

// TimeoutErrMsg returns last error message set via TimeoutError call.
func (ctx *RequestCtx) TimeoutErrMsg() string {
	return ctx.timeoutErrMsg
}

func writeResponse(ctx *RequestCtx, w *bufio.Writer) error {
	if len(ctx.timeoutErrMsg) > 0 {
		panic("BUG: cannot write timed out response")
	}
	h := &ctx.Response.Header
	serverOld := h.Server()
	if len(serverOld) == 0 {
		h.server = ctx.s.getServerName()
	}
	err := ctx.Response.Write(w)
	if len(serverOld) == 0 {
		h.server = serverOld
	}
	return err
}

const (
	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 4096
)

var bytePool sync.Pool

func acquireByteReader(ctx *RequestCtx) (*RequestCtx, *bufio.Reader, error) {
	s := ctx.s
	c := ctx.c
	s.releaseCtx(ctx)

	v := bytePool.Get()
	if v == nil {
		v = make([]byte, 1)
	}
	b := v.([]byte)
	n, err := c.Read(b)
	ch := b[0]
	bytePool.Put(v)
	ctx = s.acquireCtx(c)
	if err != nil {
		return ctx, nil, err
	}
	if n != 1 {
		panic("BUG: Reader must return at least one byte")
	}

	ctx.fbr.c = c
	ctx.fbr.ch = ch
	ctx.fbr.byteRead = false
	r := acquireReader(ctx)
	r.Reset(&ctx.fbr)
	return ctx, r, nil
}

func acquireReader(ctx *RequestCtx) *bufio.Reader {
	v := ctx.s.readerPool.Get()
	if v == nil {
		n := ctx.s.ReadBufferSize
		if n <= 0 {
			n = defaultReadBufferSize
		}
		return bufio.NewReaderSize(ctx.c, n)
	}
	r := v.(*bufio.Reader)
	r.Reset(ctx.c)
	return r
}

func releaseReader(ctx *RequestCtx, r *bufio.Reader) {
	r.Reset(nil)
	ctx.s.readerPool.Put(r)
}

func acquireWriter(ctx *RequestCtx) *bufio.Writer {
	v := ctx.s.writerPool.Get()
	if v == nil {
		n := ctx.s.WriteBufferSize
		if n <= 0 {
			n = defaultWriteBufferSize
		}
		return bufio.NewWriterSize(ctx.c, n)
	}
	w := v.(*bufio.Writer)
	w.Reset(ctx.c)
	return w
}

func releaseWriter(ctx *RequestCtx, w *bufio.Writer) {
	w.Reset(nil)
	ctx.s.writerPool.Put(w)
}

var globalCtxID uint64

func (s *Server) acquireCtx(c net.Conn) *RequestCtx {
	v := s.ctxPool.Get()
	var ctx *RequestCtx
	if v == nil {
		ctx = &RequestCtx{
			s: s,
		}
		ctx.v = ctx
		v = ctx
	} else {
		ctx = v.(*RequestCtx)
	}
	ctx.initID()
	ctx.c = c
	return ctx
}

// Init prepares ctx for passing to RequestHandler.
//
// remoteAddr and logger are optional. They are used by RequestCtx.Logger().
//
// This function is intended for custom Server implementations.
func (ctx *RequestCtx) Init(req *Request, remoteAddr net.Addr, logger Logger) {
	if remoteAddr == nil {
		remoteAddr = zeroTCPAddr
	}
	ctx.c = &fakeAddrer{
		addr: remoteAddr,
	}
	if logger != nil {
		ctx.logger.logger = logger
	}
	ctx.s = &fakeServer
	ctx.initID()
	req.CopyTo(&ctx.Request)
	ctx.Response.Clear()
	ctx.Time = time.Now()
}

var fakeServer Server

type fakeAddrer struct {
	net.Conn
	addr net.Addr
}

func (fa *fakeAddrer) RemoteAddr() net.Addr {
	return fa.addr
}

func (fa *fakeAddrer) LocalAddr() net.Addr {
	return fa.addr
}

func (fa *fakeAddrer) Read(p []byte) (int, error) {
	panic("BUG: unexpected Read call")
}

func (fa *fakeAddrer) Write(p []byte) (int, error) {
	panic("BUG: unexpected Write call")
}

func (fa *fakeAddrer) Close() error {
	panic("BUG: unexpected Close call")
}

func (ctx *RequestCtx) initID() {
	ctx.ID = (atomic.AddUint64(&globalCtxID, 1)) << 32
}

func (s *Server) releaseCtx(ctx *RequestCtx) {
	if len(ctx.timeoutErrMsg) > 0 {
		panic("BUG: cannot release timed out RequestCtx")
	}
	ctx.c = nil
	ctx.fbr.c = nil
	s.ctxPool.Put(ctx.v)
}

func (s *Server) getServerName() []byte {
	v := s.serverName.Load()
	var serverName []byte
	if v == nil {
		serverName = []byte(s.Name)
		if len(serverName) == 0 {
			serverName = defaultServerName
		}
		s.serverName.Store(serverName)
	} else {
		serverName = v.([]byte)
	}
	return serverName
}
