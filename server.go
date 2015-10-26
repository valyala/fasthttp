package fasthttp

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RequestHandler must process incoming requests.
//
// ResponseHandler must call ctx.TimeoutError() before return
// if it keeps references to ctx and/or its' members after the return.
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

	// Logger, which is used by ServerCtx.Logger().
	//
	// By default standard logger from log package is used.
	Logger Logger

	serverName atomic.Value
	ctxPool    sync.Pool
}

// TimeoutHandler returns StatusRequestTimeout error with the given msg
// in the body to the client if h didn't return during the given duration.
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

	// Cache for arbitrary data, which may be used by RequestHandler.
	// Cache contents may survive across requests.
	Cache interface{}

	logger ctxLogger
	s      *Server
	c      io.ReadWriter
	r      *bufio.Reader
	w      *bufio.Writer

	// shadow is set by TimeoutError().
	shadow *RequestCtx

	timeoutCh    chan struct{}
	timeoutTimer *time.Timer

	v interface{}
}

type remoteAddrer interface {
	RemoteAddr() net.Addr
}

type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

// Logger is used for logging formatted messages.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...interface{})
}

var ctxLoggerLock sync.Mutex

type ctxLogger struct {
	ctx *RequestCtx
}

func (cl *ctxLogger) Printf(format string, args ...interface{}) {
	ctxLoggerLock.Lock()
	s := fmt.Sprintf(format, args...)
	ctx := cl.ctx
	req := &ctx.Request
	req.ParseURI()
	ctx.s.logger().Printf("#%016X - %s - %s %s - %s", ctx.ID, ctx.RemoteAddr(), req.Header.Method, req.URI.URI, s)
	ctxLoggerLock.Unlock()
}

// RemoteAddr returns client address for the given request.
func (ctx *RequestCtx) RemoteAddr() string {
	x, ok := ctx.c.(remoteAddrer)
	if !ok {
		return "unknown remote addr"
	}
	return x.RemoteAddr().String()
}

// RemoteIP returns client ip for the given request.
func (ctx *RequestCtx) RemoteIP() string {
	addr := ctx.RemoteAddr()
	n := strings.LastIndexByte(addr, ':')
	if n < 0 {
		return addr
	}
	return addr[:n]
}

// Error sets response status code to the given value and sets response body
// to the given message.
//
// Error calls are ignored after TimeoutError call.
func (ctx *RequestCtx) Error(msg string, statusCode int) {
	resp := &ctx.Response
	resp.Clear()
	resp.Header.StatusCode = statusCode
	resp.Header.set(strContentType, defaultContentType)
	resp.Body = append(resp.Body, []byte(msg)...)
}

// Success sets response Content-Type and body to the given values.
//
// It is safe modifying body buffer after the Success() call.
//
// Success calls are ignored after TimeoutError call.
func (ctx *RequestCtx) Success(contentType string, body []byte) {
	resp := &ctx.Response
	resp.Header.setStr(strContentType, contentType)
	resp.Body = append(resp.Body, body...)
}

// Logger returns logger, which may be used for logging arbitrary
// request-specific messages inside RequestHandler.
//
// Each message logged via returned logger contains request-specific information
// such as request id, remote address, request method and request url.
//
// It is safe re-using returned logger for logging multiple messages.
func (ctx *RequestCtx) Logger() Logger {
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
	shadow := makeShadow(ctx)
	if ctx.shadow == nil {
		ctx.shadow = shadow
		shadow.Error(msg, StatusRequestTimeout)
	}
}

const defaultConcurrency = 64 * 1024

// Serve serves incoming connections from the given listener.
//
// Serve blocks until the given listener returns permanent error.
// This error is returned from Serve.
func (s *Server) Serve(ln net.Listener) error {
	return s.ServeConcurrency(ln, defaultConcurrency)
}

// ServeConcurrency serves incoming connections from the given listener.
// It may serve maximum concurrency simultaneous connections.
//
// ServeConcurrency blocks until the given listener returns permanent error.
// This error is returned from ServeConcurrency.
func (s *Server) ServeConcurrency(ln net.Listener, concurrency int) error {
	ch := make(chan net.Conn, 16*concurrency)
	stopCh := make(chan struct{})
	go connWorkersMonitor(s, ch, concurrency, stopCh)
	var lastOverflowErrorTime time.Time
	for {
		c, err := acceptConn(s, ln)
		if err != nil {
			close(stopCh)
			return err
		}
		select {
		case ch <- c:
		default:
			c.Close()
			if time.Since(lastOverflowErrorTime) > time.Second*10 {
				s.logger().Printf("The incoming connection cannot be served, because all %d workers are busy. "+
					"Try increasing concurrency in Server.ServeWorkers()", concurrency)
				lastOverflowErrorTime = time.Now()
			}
		}
	}
}

func connWorkersMonitor(s *Server, ch <-chan net.Conn, maxWorkers int, stopCh <-chan struct{}) {
	workersCount := uint32(0)
	var tc *time.Timer
	for {
		n := int(atomic.LoadUint32(&workersCount))
		pendingConns := len(ch)
		if n < maxWorkers && pendingConns > 0 {
			for i := 0; i < pendingConns; i++ {
				atomic.AddUint32(&workersCount, 1)
				go func() {
					connWorker(s, ch)
					atomic.AddUint32(&workersCount, ^uint32(0))
				}()
			}
			runtime.Gosched()
		} else {
			tc = initTimer(tc, 100*time.Millisecond)
			select {
			case <-stopCh:
				stopTimer(tc)
				return
			case <-tc.C:
				stopTimer(tc)
			}
		}
	}
}

func connWorker(s *Server, ch <-chan net.Conn) {
	ctx := s.acquireCtx()

	var c net.Conn
	defer func() {
		if r := recover(); r != nil {
			s.logger().Printf("panic: %s\nStack trace:\n%s", r, debug.Stack())
		}
		if c != nil {
			c.Close()
		}
	}()

	var tc *time.Timer
	for {
		select {
		case c = <-ch:
		default:
			tc = initTimer(tc, time.Second)
			select {
			case c = <-ch:
				stopTimer(tc)
			case <-tc.C:
				stopTimer(tc)
				s.releaseCtx(ctx)
				return
			}
		}
		s.serveConn(c, &ctx)
		c.Close()
		c = nil
	}
}

func acceptConn(s *Server, ln net.Listener) (net.Conn, error) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				s.logger().Printf("Temporary error when accepting new connections: %s", netErr)
				time.Sleep(time.Second)
				continue
			}
			if err != io.EOF {
				s.logger().Printf("Permanent error: %s", err)
			}
			return nil, err
		}
		return c, nil
	}
}

var defaultLogger = log.New(os.Stderr, "", log.LstdFlags)

func (s *Server) logger() Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return defaultLogger
}

// ServeConn serves HTTP requests from the given connection.
//
// ServeConn returns nil if all requests from the c are successfully served.
// It returns non-nil error otherwise.
func (s *Server) ServeConn(c io.ReadWriter) error {
	ctx := s.acquireCtx()
	err := s.serveConn(c, &ctx)
	s.releaseCtx(ctx)
	return err
}

func (s *Server) serveConn(c io.ReadWriter, ctxP **RequestCtx) error {
	ctx := *ctxP
	initRequestCtx(ctx, c)

	var rd readDeadliner
	readTimeout := s.ReadTimeout
	if readTimeout > 0 {
		rd, _ = c.(readDeadliner)
	}

	var wd writeDeadliner
	writeTimeout := s.WriteTimeout
	if writeTimeout > 0 {
		wd, _ = c.(writeDeadliner)
	}

	var err error
	for {
		if rd != nil {
			if err = rd.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				break
			}
			err = ctx.Request.Read(ctx.r)
		} else {
			if err = ctx.Request.ReadTimeout(ctx.r, readTimeout); err == ErrReadTimeout {
				// ctx.Requests cannot be used after ErrReadTimeout, so create ctx shadow.
				*ctxP = makeShadow(ctx)
				break
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		ctx.ID++
		ctx.Time = time.Now()
		s.Handler(ctx)
		shadow := ctx.shadow
		if shadow != nil {
			ctx = shadow
			*ctxP = ctx
		}

		if wd != nil {
			if err = wd.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				break
			}
		}
		if err = writeResponse(ctx); err != nil {
			break
		}
		connectionClose := ctx.Response.Header.ConnectionClose

		ctx.Response.Clear()
		trimBigBuffers(ctx)

		if ctx.r.Buffered() == 0 || connectionClose {
			if err = ctx.w.Flush(); err != nil {
				break
			}
			if connectionClose {
				break
			}
		}
	}

	if err != nil && !strings.Contains(err.Error(), "connection reset by peer") {
		ctx.Logger().Printf("Error when serving network connection: %s", err)
	}
	return err
}

func makeShadow(ctx *RequestCtx) *RequestCtx {
	var shadow RequestCtx
	shadow.Request = Request{}
	shadow.Response = Response{}
	shadow.logger.ctx = &shadow
	shadow.v = &shadow

	shadow.s = ctx.s
	shadow.c = ctx.c
	shadow.r = ctx.r
	shadow.w = ctx.w
	return &shadow
}

func writeResponse(ctx *RequestCtx) error {
	if ctx.shadow != nil {
		panic("BUG: cannot write response with shadow")
	}
	h := &ctx.Response.Header
	serverOld := h.server
	if len(serverOld) == 0 {
		h.server = ctx.s.getServerName()
	}
	err := ctx.Response.Write(ctx.w)
	if len(serverOld) == 0 {
		h.server = serverOld
	}
	return err
}

const bigBufferLimit = 16 * 1024

func trimBigBuffers(ctx *RequestCtx) {
	if cap(ctx.Request.Body) > bigBufferLimit {
		ctx.Request.Body = nil
	}
	if cap(ctx.Response.Body) > bigBufferLimit {
		ctx.Response.Body = nil
	}
}

func initRequestCtx(ctx *RequestCtx, c io.ReadWriter) {
	if ctx.r == nil {
		readBufferSize := ctx.s.ReadBufferSize
		if readBufferSize <= 0 {
			readBufferSize = 4096
		}
		writeBufferSize := ctx.s.WriteBufferSize
		if writeBufferSize <= 0 {
			writeBufferSize = 4096
		}
		ctx.r = bufio.NewReaderSize(c, readBufferSize)
		ctx.w = bufio.NewWriterSize(c, writeBufferSize)
	} else {
		ctx.r.Reset(c)
		ctx.w.Reset(c)
	}
	ctx.c = c
}

var globalCtxID uint64

func (s *Server) acquireCtx() *RequestCtx {
	v := s.ctxPool.Get()
	var ctx *RequestCtx
	if v == nil {
		ctx = &RequestCtx{
			s: s,
		}
		ctx.logger.ctx = ctx
		ctx.v = ctx
		v = ctx
	} else {
		ctx = v.(*RequestCtx)
	}
	ctx.ID = (atomic.AddUint64(&globalCtxID, 1)) << 32
	return ctx
}

func (s *Server) releaseCtx(ctx *RequestCtx) {
	if ctx.shadow != nil {
		panic("BUG: cannot release RequestCtx with shadow")
	}
	ctx.c = nil
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
