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
	"unsafe"
)

type Server struct {
	// Request handler
	Handler RequestHandler

	// Server name for sending in response headers.
	Name string

	// Per-connection buffer size for requests' reading.
	// This also limits the maximum header size.
	ReadBufferSize int

	// Per-connection buffer size for responses' writing.
	WriteBufferSize int

	// Logger.
	Logger Logger

	serverName atomic.Value
	ctxPool    sync.Pool
}

type RequestHandler func(ctx *ServerCtx)

type ServerCtx struct {
	Request  Request
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
	c      remoteAddrer
	r      *bufio.Reader
	w      *bufio.Writer
	shadow unsafe.Pointer

	v interface{}
}

type remoteAddrer interface {
	RemoteAddr() net.Addr
}

type Logger interface {
	Printf(format string, args ...interface{})
}

var ctxLoggerLock sync.Mutex

type ctxLogger struct {
	ctx *ServerCtx
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

func (ctx *ServerCtx) RemoteAddr() string {
	if ctx.c == nil {
		return "unknown remote addr"
	}
	return ctx.c.RemoteAddr().String()
}

func (ctx *ServerCtx) RemoteIP() string {
	addr := ctx.RemoteAddr()
	n := strings.LastIndexByte(addr, ':')
	if n < 0 {
		return addr
	}
	return addr[:n]
}

func (ctx *ServerCtx) Error(msg string, statusCode int) {
	resp := &ctx.Response
	resp.Clear()
	resp.Header.StatusCode = statusCode
	resp.Header.set(strContentType, defaultContentType)
	resp.Body = append(resp.Body, []byte(msg)...)
}

func (ctx *ServerCtx) Success(contentType string, body []byte) {
	resp := &ctx.Response
	resp.Header.setStr(strContentType, contentType)
	resp.Body = append(resp.Body, body...)
}

func (ctx *ServerCtx) Logger() Logger {
	return &ctx.logger
}

func (ctx *ServerCtx) TimeoutError(msg string) {
	var shadow ServerCtx
	shadow.Request = Request{}
	shadow.Response = Response{}
	shadow.logger.ctx = &shadow
	shadow.v = &shadow

	shadow.s = ctx.s
	shadow.c = ctx.c
	shadow.r = ctx.r
	shadow.w = ctx.w

	if atomic.CompareAndSwapPointer(&ctx.shadow, nil, unsafe.Pointer(&shadow)) {
		shadow.Error(msg, StatusRequestTimeout)
	}
}

func (ctx *ServerCtx) writeResponse() error {
	if atomic.LoadPointer(&ctx.shadow) != nil {
		panic("BUG: cannot write response with shadow")
	}
	h := &ctx.Response.Header
	serverOld := h.peek(strServer)
	if len(serverOld) == 0 {
		h.set(strServer, ctx.s.getServerName())
	}
	err := ctx.Response.Write(ctx.w)
	if len(serverOld) == 0 {
		h.set(strServer, serverOld)
	}
	return err
}

const defaultConcurrency = 64 * 1024

func (s *Server) Serve(ln net.Listener) error {
	return s.ServeConcurrency(ln, defaultConcurrency)
}

func (s *Server) ServeConcurrency(ln net.Listener, concurrency int) error {
	ch := make(chan net.Conn, 16*concurrency)
	stopCh := make(chan struct{})
	go connWorkersMonitor(s, ch, concurrency, stopCh)
	lastOverflowErrorTime := time.Now()
	for {
		c, err := acceptConn(s, ln)
		if err != nil {
			close(stopCh)
			return err
		}
		select {
		case ch <- c:
		default:
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
			tc := acquireTimer(100 * time.Millisecond)
			select {
			case <-stopCh:
				return
			case <-tc.C:
			}
			releaseTimer(tc)
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

	for {
		select {
		case c = <-ch:
		default:
			tc := acquireTimer(time.Second)
			select {
			case c = <-ch:
			case <-tc.C:
				s.releaseCtx(ctx)
				return
			}
			releaseTimer(tc)
		}
		serveConn(s, c, &ctx)
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
			s.logger().Printf("Permanent error: %s", err)
			return nil, err
		}
		return c, nil
	}
}

func serveConn(s *Server, c io.ReadWriter, ctx **ServerCtx) {
	if err := s.serveConn(c, ctx); err != nil {
		if !strings.Contains(err.Error(), "connection reset by peer") {
			s.logger().Printf("Error when serving network connection: %s", err)
		}
	}
}

var defaultLogger = log.New(os.Stderr, "", log.LstdFlags)

func (s *Server) logger() Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return defaultLogger
}

func (s *Server) ServeConn(c io.ReadWriter) error {
	ctx := s.acquireCtx()
	err := s.serveConn(c, &ctx)
	s.releaseCtx(ctx)
	return err
}

func (s *Server) serveConn(c io.ReadWriter, ctxP **ServerCtx) error {
	ctx := *ctxP
	initServerCtx(ctx, c)
	var err error
	for {
		if err = ctx.Request.Read(ctx.r); err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		ctx.ID++
		ctx.Time = time.Now()
		s.Handler(ctx)
		shadow := atomic.LoadPointer(&ctx.shadow)
		if shadow != nil {
			ctx = (*ServerCtx)(shadow)
			*ctxP = ctx
		}
		if err = ctx.writeResponse(); err != nil {
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
	return err
}

const bigBufferLimit = 16 * 1024

func trimBigBuffers(ctx *ServerCtx) {
	if cap(ctx.Request.Body) > bigBufferLimit {
		ctx.Request.Body = nil
	}
	if cap(ctx.Response.Body) > bigBufferLimit {
		ctx.Response.Body = nil
	}
}

func initServerCtx(ctx *ServerCtx, c io.ReadWriter) {
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
	if conn, ok := c.(remoteAddrer); ok {
		ctx.c = conn
	}
}

var globalCtxID uint64

func (s *Server) acquireCtx() *ServerCtx {
	v := s.ctxPool.Get()
	var ctx *ServerCtx
	if v == nil {
		ctx = &ServerCtx{
			s: s,
		}
		ctx.logger.ctx = ctx
		ctx.v = ctx
		v = ctx
	} else {
		ctx = v.(*ServerCtx)
	}
	ctx.ID = (atomic.AddUint64(&globalCtxID, 1)) << 32
	return ctx
}

func (s *Server) releaseCtx(ctx *ServerCtx) {
	if atomic.LoadPointer(&ctx.shadow) != nil {
		panic("BUG: cannot release ServerCtx with shadow")
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
