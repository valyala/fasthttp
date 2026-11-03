package fasthttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type protocolHandlerFunc func(*ProtocolServerContext, net.Conn) error

func (f protocolHandlerFunc) ServeConn(ctx *ProtocolServerContext, c net.Conn) error {
	return f(ctx, c)
}

type upgraderHandlerFunc struct{ protocolHandlerFunc }

func (upgraderHandlerFunc) UpgradeConn(*ProtocolServerContext, net.Conn, *Request, RequestConfig) (bool, error) {
	return false, nil
}

type protocolDeadlineConn struct {
	net.Conn

	mu                   sync.Mutex
	nonzeroReadDeadlines int
}

func (c *protocolDeadlineConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.mu.Lock()
		c.nonzeroReadDeadlines++
		c.mu.Unlock()
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *protocolDeadlineConn) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

func (c *protocolDeadlineConn) readDeadlineCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonzeroReadDeadlines
}

type emptyReadConn struct {
	net.Conn

	reads int
}

type readCountingConn struct {
	net.Conn

	reads int
}

func (c *readCountingConn) Read(p []byte) (int, error) {
	c.reads++
	return c.Conn.Read(p)
}

func (c *emptyReadConn) Read([]byte) (int, error) {
	c.reads++
	return 0, nil
}

type testProtocolStream struct {
	context.Context //nolint:containedctx

	mu                  sync.Mutex
	informationalStatus int
	pushTarget          string
	accepted            bool
	hijackRejected      bool
	hijackNoResponse    bool
}

func (s *testProtocolStream) HijackRejected(noResponse bool) {
	s.mu.Lock()
	s.hijackRejected = true
	s.hijackNoResponse = noResponse
	s.mu.Unlock()
}

func (s *testProtocolStream) WriteInformational(statusCode int, _ *ResponseHeader) error {
	s.mu.Lock()
	s.informationalStatus = statusCode
	s.mu.Unlock()
	return nil
}

func (s *testProtocolStream) Push(target string, _ *PushOptions) error {
	s.mu.Lock()
	s.pushTarget = target
	s.mu.Unlock()
	return nil
}

func (s *testProtocolStream) AcceptStream(_ StreamHandler) error {
	s.mu.Lock()
	s.accepted = true
	s.mu.Unlock()
	return nil
}

func TestServerRegisterProtocol(t *testing.T) {
	validHandler := protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil })
	tests := []struct {
		name          string
		registrations []ProtocolRegistration
		wantError     bool
	}{
		{
			name: "missing handler",
			registrations: []ProtocolRegistration{{
				ALPN: []string{"example"},
			}},
			wantError: true,
		},
		{
			name: "missing selector",
			registrations: []ProtocolRegistration{{
				Handler: validHandler,
			}},
			wantError: true,
		},
		{
			name: "duplicate alpn in registration",
			registrations: []ProtocolRegistration{{
				ALPN:    []string{"example", "example"},
				Handler: validHandler,
			}},
			wantError: true,
		},
		{
			name: "two protocols",
			registrations: []ProtocolRegistration{
				{ALPN: []string{"example"}, Handler: validHandler},
				{ALPN: []string{"other"}, Handler: validHandler},
			},
		},
		{
			name: "duplicate registered alpn",
			registrations: []ProtocolRegistration{
				{ALPN: []string{"example"}, Handler: validHandler},
				{ALPN: []string{"example"}, Handler: validHandler},
			},
			wantError: true,
		},
		{
			name: "conflicting prefaces",
			registrations: []ProtocolRegistration{
				{CleartextPreface: []byte("PROTO"), Handler: validHandler},
				{CleartextPreface: []byte("PROTOCOL"), Handler: validHandler},
			},
			wantError: true,
		},
		{
			name: "duplicate upgrade token",
			registrations: []ProtocolRegistration{
				{ALPN: []string{"a"}, CleartextUpgradeToken: "example", Handler: upgraderHandlerFunc{validHandler}},
				{ALPN: []string{"b"}, CleartextUpgradeToken: "example", Handler: upgraderHandlerFunc{validHandler}},
			},
			wantError: true,
		},
		{
			name: "alpn and preface",
			registrations: []ProtocolRegistration{{
				ALPN:             []string{"example"},
				CleartextPreface: []byte("PROTOCOL"),
				Handler:          validHandler,
			}},
		},
		{
			name: "upgrade token without upgrader",
			registrations: []ProtocolRegistration{{
				ALPN:                  []string{"example"},
				CleartextUpgradeToken: "example",
				Handler:               validHandler,
			}},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			var err error
			for _, registration := range tt.registrations {
				err = s.RegisterProtocol(registration)
				if err != nil {
					break
				}
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("RegisterProtocol() error = %v, wantError = %v", err, tt.wantError)
			}
			if err == nil && !slices.Contains(s.TLSConfig.NextProtos, "example") {
				t.Fatal("RegisterProtocol() didn't add the ALPN protocol")
			}
		})
	}
}

func TestServerRegisterProtocolCopiesSelectors(t *testing.T) {
	alpn := []string{"example"}
	preface := []byte("PROTOCOL")
	s := &Server{}
	err := s.RegisterProtocol(ProtocolRegistration{
		ALPN:             alpn,
		CleartextPreface: preface,
		Handler:          protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil }),
	})
	if err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}

	alpn[0] = "changed"
	preface[0] = 'X'
	if got := s.protocols[0].alpn[0]; got != "example" {
		t.Fatalf("registered ALPN = %q, want %q", got, "example")
	}
	if got := string(s.protocols[0].cleartextPreface); got != "PROTOCOL" {
		t.Fatalf("registered preface = %q, want %q", got, "PROTOCOL")
	}
}

func TestServerRegisterProtocolPreservesAndOrdersTLSConfig(t *testing.T) {
	original := &tls.Config{NextProtos: []string{"custom", "http/1.1"}}
	server := &Server{TLSConfig: original}
	err := server.RegisterProtocol(ProtocolRegistration{
		ALPN:         []string{"h2"},
		FallbackALPN: []string{"http/1.1"},
		Handler:      protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil }),
	})
	if err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if server.TLSConfig != original {
		t.Fatal("RegisterProtocol() replaced the caller's TLS config")
	}
	if got := server.TLSConfig.NextProtos; !slices.Equal(got, []string{"custom", "h2", "http/1.1"}) {
		t.Fatalf("NextProtos = %v, want [custom h2 http/1.1]", got)
	}
	if got := original.NextProtos; !slices.Equal(got, []string{"custom", "h2", "http/1.1"}) {
		t.Fatalf("caller's NextProtos = %v, want [custom h2 http/1.1]", got)
	}
}

func TestServerRegisterProtocolTLSFailureDoesNotMutateServer(t *testing.T) {
	original := &tls.Config{
		MinVersion: tls.VersionTLS10,
		MaxVersion: tls.VersionTLS11,
		NextProtos: []string{"custom"},
	}
	server := &Server{TLSConfig: original}
	err := server.RegisterProtocol(ProtocolRegistration{
		ALPN:          []string{"h2"},
		FallbackALPN:  []string{"http/1.1"},
		MinTLSVersion: tls.VersionTLS12,
		Handler:       protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil }),
	})
	if err == nil {
		t.Fatal("RegisterProtocol() succeeded with an incompatible TLS maximum version")
	}
	if server.TLSConfig != original {
		t.Fatal("RegisterProtocol() replaced TLSConfig after a failed registration")
	}
	if len(server.protocols) != 0 {
		t.Fatal("a failed registration left a protocol registered")
	}
	if original.MinVersion != tls.VersionTLS10 || !slices.Equal(original.NextProtos, []string{"custom"}) {
		t.Fatalf("caller's TLS config was modified: min=%#x next=%v", original.MinVersion, original.NextProtos)
	}
}

func TestServerTLSMutationPreservesCallerConfig(t *testing.T) {
	original := &tls.Config{NextProtos: []string{"custom"}, MinVersion: tls.VersionTLS12}
	server := &Server{TLSConfig: original}
	server.NextProto("example", func(net.Conn) error { return nil })
	if server.TLSConfig != original {
		t.Fatal("NextProto replaced the caller's TLS config")
	}
	if !slices.Equal(original.NextProtos, []string{"custom", "example"}) {
		t.Fatalf("caller's NextProtos = %v, want [custom example]", original.NextProtos)
	}
	if !slices.Equal(server.TLSConfig.NextProtos, []string{"custom", "example"}) {
		t.Fatalf("server NextProtos = %v", server.TLSConfig.NextProtos)
	}

	replacement := &tls.Config{NextProtos: []string{"replacement"}}
	server.TLSConfig = replacement
	server.NextProto("second", func(net.Conn) error { return nil })
	if server.TLSConfig != replacement {
		t.Fatal("NextProto replaced a caller-supplied TLS config")
	}
	if !slices.Equal(replacement.NextProtos, []string{"replacement", "second"}) {
		t.Fatalf("replacement NextProtos = %v, want [replacement second]", replacement.NextProtos)
	}
}

func TestServerCleartextProtocolDispatch(t *testing.T) {
	const preface = "PROTOCOL-PREFACE"
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	requestHandled := make(chan struct{})
	states := make(chan ConnState, 2)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if got := string(ctx.Request.Header.Protocol()); got != "EXAMPLE/1" {
				t.Errorf("request protocol = %q, want %q", got, "EXAMPLE/1")
			}
			close(requestHandled)
		},
		ConnState: func(_ net.Conn, state ConnState) {
			if state == StateActive || state == StateIdle {
				states <- state
			}
		},
	}
	err := s.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte(preface),
		Handler: protocolHandlerFunc(func(ctx *ProtocolServerContext, c net.Conn) error {
			if !ctx.CleartextPrefaceConsumed() {
				return errors.New("protocol preface wasn't marked consumed")
			}

			stream := &testProtocolStream{Context: context.Background()}
			requestCtx := ctx.AcquireRequestCtx(c, stream)
			requestCtx.Request.Header.SetProtocol("EXAMPLE/1")
			ctx.Server().Handler(requestCtx)
			ctx.ReleaseRequestCtx(requestCtx)
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}

	serveError := make(chan error, 1)
	go func() {
		serveError <- s.ServeConn(serverConn)
	}()
	if _, err := io.WriteString(clientConn, preface); err != nil {
		t.Fatalf("writing preface: %v", err)
	}

	select {
	case <-requestHandled:
	case <-time.After(time.Second):
		t.Fatal("protocol handler didn't run")
	}
	if err := <-serveError; err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
	if got := []ConnState{<-states, <-states}; !slices.Equal(got, []ConnState{StateActive, StateIdle}) {
		t.Fatalf("connection states = %v, want [active idle]", got)
	}
}

func TestDetectCleartextProtocolReadsAvailablePrefixInOneCall(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	counted := &readCountingConn{Conn: serverConn}
	server := &Server{protocols: []*registeredProtocol{{
		cleartextPreface: []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
	}}}
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(clientConn, "PRI * HTTP/2.0\r\n\r\nSX\r\n\r\n")
		writeDone <- err
	}()
	detection, err := server.detectCleartextProtocol(counted, new(atomic.Int64))
	if err != nil {
		t.Fatalf("detectCleartextProtocol() error: %v", err)
	}
	if detection.protocol != nil || len(detection.prefix) != len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n") {
		t.Fatalf("detection = protocol:%v prefix:%q", detection.protocol, detection.prefix)
	}
	if counted.reads != 1 {
		t.Fatalf("connection reads = %d, want 1 for an available 24-byte prefix", counted.reads)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("writing prefix: %v", err)
	}
}

func TestProtocolReleaseAbandonsTimedOutRequestCtx(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server := &Server{}
	protocolContext := &ProtocolServerContext{
		server:       server,
		conn:         serverConn,
		idleConnTime: new(atomic.Int64),
	}
	stream := &testProtocolStream{Context: context.Background()}
	requestCtx := protocolContext.AcquireRequestCtx(serverConn, stream)
	requestCtx.TimeoutError("timeout")

	protocolContext.ReleaseRequestCtx(requestCtx)
	if protocolContext.active != 0 {
		t.Fatalf("active requests = %d, want 0", protocolContext.active)
	}
	if requestCtx.LastTimeoutErrorResponse() == nil {
		t.Fatal("timed out RequestCtx was reset and returned to the pool")
	}
}

func TestProtocolRequestCtxCacheRetainsBodyCapacity(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server := &Server{}
	protocolContext := &ProtocolServerContext{
		server:       server,
		conn:         serverConn,
		idleConnTime: new(atomic.Int64),
	}
	stream := &testProtocolStream{Context: context.Background()}
	first := protocolContext.AcquireRequestCtx(serverConn, stream)
	first.Request.SetBody(make([]byte, 1<<20))
	ctxCap := cap(first.Request.Body())
	protocolContext.ReleaseRequestCtx(first)
	if protocolContext.requestBytes != ctxCap {
		t.Fatalf("cached request bytes = %d, want %d", protocolContext.requestBytes, ctxCap)
	}

	second := protocolContext.AcquireRequestCtx(serverConn, stream)
	if second != first {
		t.Fatal("connection-local protocol cache did not reuse RequestCtx")
	}
	if got := cap(second.Request.Body()); got != ctxCap {
		t.Fatalf("reused request body capacity = %d, want %d", got, ctxCap)
	}
	if protocolContext.requestBytes != 0 {
		t.Fatalf("cached request bytes after acquire = %d, want 0", protocolContext.requestBytes)
	}
	protocolContext.ReleaseRequestCtx(second)
	protocolContext.releaseCachedRequestCtxs()
	if protocolContext.requestBytes != 0 || len(protocolContext.requestCache) != 0 {
		t.Fatal("connection-local request cache was not drained")
	}
	if got := cap(first.Request.Body()); got != 0 {
		t.Fatalf("drained cache kept %d bytes of body in the server-wide pool", got)
	}
}

func TestProtocolRequestCtxCacheAccountsForHeaderArenas(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server := &Server{MaxProtocolRequestCtxCacheBytes: 1024}
	protocolContext := &ProtocolServerContext{
		server:       server,
		conn:         serverConn,
		idleConnTime: new(atomic.Int64),
	}
	stream := &testProtocolStream{Context: context.Background()}
	requestCtx := protocolContext.AcquireRequestCtx(serverConn, stream)
	requestCtx.Request.Header.Set("X-Large", string(make([]byte, 4<<10)))
	protocolContext.ReleaseRequestCtx(requestCtx)
	if len(protocolContext.requestCache) != 0 || protocolContext.requestBytes != 0 {
		t.Fatalf("oversize header arena was cached: contexts=%d bytes=%d",
			len(protocolContext.requestCache), protocolContext.requestBytes)
	}
}

func TestRequestCtxInit2ClearsProtocolState(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	requestCtx := &RequestCtx{
		protocolStream: &testProtocolStream{Context: context.Background()},
		protocolOwner:  &ProtocolServerContext{},
	}
	requestCtx.Init2(serverConn, nil, false)
	if requestCtx.protocolStream != nil || requestCtx.protocolOwner != nil {
		t.Fatal("Init2 retained protocol state from the previous request")
	}
}

func TestServerCleartextProtocolFallsBackToHTTP1(t *testing.T) {
	pipeServerConn, clientConn := net.Pipe()
	defer clientConn.Close()
	serverConn := &protocolDeadlineConn{Conn: pipeServerConn}

	s := &Server{
		ReadTimeout: time.Second,
		Handler: func(ctx *RequestCtx) {
			if ctx.Conn() != serverConn {
				t.Errorf("RequestCtx.Conn() = %T %p, want original connection %p", ctx.Conn(), ctx.Conn(), serverConn)
			}
			if _, ok := ctx.Conn().(io.ReaderFrom); !ok {
				t.Error("RequestCtx.Conn() lost the original connection's io.ReaderFrom capability")
			}
			ctx.SetBodyString("ok")
		},
		ConnState: func(conn net.Conn, state ConnState) {
			if state != StateNew && conn != serverConn {
				t.Errorf("ConnState(%v) received %T %p, want original connection %p", state, conn, conn, serverConn)
			}
		},
	}
	err := s.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("PROTOCOL-PREFACE"),
		Handler: protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error {
			return errors.New("unexpected protocol dispatch")
		}),
	})
	if err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}

	serveError := make(chan error, 1)
	go func() {
		serveError <- s.ServeConn(serverConn)
	}()
	if _, err := io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("writing request: %v", err)
	}

	var response Response
	if err := response.Read(bufio.NewReader(clientConn)); err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if got := string(response.Body()); got != "ok" {
		t.Fatalf("response body = %q, want %q", got, "ok")
	}
	if err := <-serveError; err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
	if got := serverConn.readDeadlineCount(); got != 1 {
		t.Fatalf("non-zero read deadlines = %d, want one shared cleartext/request deadline", got)
	}
}

func TestDetectCleartextProtocolStopsAfterEmptyReads(t *testing.T) {
	pipeServerConn, pipeClientConn := net.Pipe()
	defer pipeServerConn.Close()
	defer pipeClientConn.Close()

	conn := &emptyReadConn{Conn: pipeServerConn}
	server := &Server{}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("PROTOCOL-PREFACE"),
		Handler: protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error {
			return nil
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}

	_, err := server.detectCleartextProtocol(conn, new(atomic.Int64))
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("detectCleartextProtocol() error = %v, want io.ErrNoProgress", err)
	}
	if conn.reads != 100 {
		t.Fatalf("empty reads = %d, want 100", conn.reads)
	}
}

func TestServerShutdownDeadlineClosesProtocolConnections(t *testing.T) {
	const preface = "PROTOCOL-PREFACE"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	connClosed := make(chan struct{})
	server := &Server{
		// The closed connection's read error is expected; keep it out of the log.
		Logger: &testLogger{},
		ConnState: func(_ net.Conn, state ConnState) {
			if state == StateClosed {
				close(connClosed)
			}
		},
	}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte(preface),
		Handler: protocolHandlerFunc(func(ctx *ProtocolServerContext, conn net.Conn) error {
			defer close(handlerDone)
			if !ctx.CleartextPrefaceConsumed() {
				return errors.New("protocol preface wasn't marked consumed")
			}
			close(handlerStarted)
			var one [1]byte
			_, err := conn.Read(one[:])
			return err
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer client.Close()
	if _, err := io.WriteString(client, preface); err != nil {
		t.Fatalf("writing protocol preface: %v", err)
	}
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("protocol handler didn't start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.ShutdownWithContext(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShutdownWithContext() error = %v, want deadline exceeded", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("protocol connection wasn't closed after the shutdown deadline")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve() error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() didn't return")
	}
	// Serve returns with the listener; the worker finishes the connection after.
	select {
	case <-connClosed:
	case <-time.After(time.Second):
		t.Fatal("the protocol connection's worker didn't finish")
	}
}

// A protocol connection winds itself down once Shutdown closes Done; the idle
// sweep closing it underneath would cut that short.
func TestCloseIdleConnsLeavesProtocolConns(t *testing.T) {
	protocolConn, protocolPeer := net.Pipe()
	defer protocolPeer.Close()
	defer protocolConn.Close()
	plainConn, plainPeer := net.Pipe()
	defer plainPeer.Close()
	server := &Server{}
	idleSince := time.Now().Add(-time.Second)
	server.registerIdleConn(protocolConn, idleSince)
	server.registerProtocolConn(protocolConn)
	server.registerIdleConn(plainConn, idleSince)

	server.closeIdleConns()
	if _, ok := server.idleConns[protocolConn]; !ok {
		t.Fatal("the idle sweep closed a protocol connection")
	}
	if _, ok := server.idleConns[plainConn]; ok {
		t.Fatal("the idle sweep left an idle HTTP/1 connection open")
	}
	if _, err := plainPeer.Write([]byte{0}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writing to the swept connection's peer = %v, want closed pipe", err)
	}
}

func TestRequestCtxProtocolOperations(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	stream := &testProtocolStream{Context: base}
	ctx := &RequestCtx{
		s:              fakeServer,
		protocolStream: stream,
	}
	// The HTTP/1.0 rule belongs to the HTTP/1 writer; the stream decides for itself.
	ctx.Request.Header.SetProtocol("EXAMPLE/1")
	ctx.Response.Header.Add("Link", "</style.css>; rel=preload")

	if err := ctx.EarlyHints(); err != nil {
		t.Fatalf("EarlyHints() error: %v", err)
	}
	if err := ctx.Push("/style.css", nil); err != nil {
		t.Fatalf("Push() error: %v", err)
	}
	if err := ctx.AcceptStream(func(StreamConn) {}); err != nil {
		t.Fatalf("AcceptStream() error: %v", err)
	}

	stream.mu.Lock()
	status := stream.informationalStatus
	target := stream.pushTarget
	accepted := stream.accepted
	stream.mu.Unlock()
	if status != StatusEarlyHints || target != "/style.css" || !accepted {
		t.Fatalf("protocol operations = (%d, %q, %v)", status, target, accepted)
	}

	// The stream's cancellation stays with the protocol; RequestCtx's context
	// is server-scoped.
	cancel()
	select {
	case <-ctx.Done():
		t.Fatal("a stream's cancellation closed RequestCtx.Done")
	default:
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func TestRequestCtxTryHijackRejectsProtocolStream(t *testing.T) {
	stream := &testProtocolStream{Context: context.Background()}
	requestCtx := &RequestCtx{protocolStream: stream}
	handler := func(net.Conn) {}
	if err := requestCtx.TryHijack(handler); !errors.Is(err, ErrHijackNotSupported) {
		t.Fatalf("TryHijack() error = %v, want ErrHijackNotSupported", err)
	}
	if requestCtx.Hijacked() {
		t.Fatal("TryHijack() registered a handler for a multiplexed request")
	}

	requestCtx.Hijack(handler)
	if !stream.hijackRejected {
		t.Fatal("Hijack() didn't notify the protocol stream")
	}
}

func TestRequestCtxTryHijackPreservesHTTP1Behavior(t *testing.T) {
	requestCtx := &RequestCtx{}
	if err := requestCtx.TryHijack(func(net.Conn) {}); err != nil {
		t.Fatalf("TryHijack() error: %v", err)
	}
	if !requestCtx.Hijacked() {
		t.Fatal("TryHijack() didn't register the HTTP/1 hijack handler")
	}
}

func TestRequestHeaderConnectProtocol(t *testing.T) {
	var src RequestHeader
	src.SetConnectProtocol("websocket")

	var dst RequestHeader
	src.CopyTo(&dst)
	if got := string(dst.ConnectProtocol()); got != "websocket" {
		t.Fatalf("copied connect protocol = %q, want %q", got, "websocket")
	}

	dst.Reset()
	if len(dst.ConnectProtocol()) != 0 {
		t.Fatalf("ConnectProtocol() after Reset = %q, want empty", dst.ConnectProtocol())
	}
}

// MaxProtocolRequestCtxCacheBytes bounds what one connection keeps for reuse.
func TestMaxProtocolRequestCtxCacheBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int
		retain  int
		wantHit bool
	}{
		{"default keeps a 1MB body", 0, 1 << 20, true},
		{"custom limit keeps below it", 4 << 20, 1 << 20, true},
		{"custom limit drops above it", 4 << 20, 8 << 20, false},
		{"negative disables the cache", -1, 1024, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{
				Handler:                         func(*RequestCtx) {},
				MaxProtocolRequestCtxCacheBytes: tc.limit,
			}
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			ctx := &ProtocolServerContext{
				server:       server,
				conn:         serverConn,
				idleConnTime: &atomic.Int64{},
			}
			requestCtx := ctx.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
			requestCtx.Request.SetBody(make([]byte, tc.retain))
			ctx.ReleaseRequestCtx(requestCtx)

			ctx.requestMu.Lock()
			cached := len(ctx.requestCache)
			ctx.requestMu.Unlock()
			if got := cached != 0; got != tc.wantHit {
				t.Fatalf("cached = %v, want %v (limit %d, retained %d)", got, tc.wantHit, tc.limit, tc.retain)
			}
		})
	}
}

func TestProtocolAcquireRequestCtxAppliesServerDefaults(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server := &Server{
		NoDefaultContentType:          true,
		NoDefaultDate:                 true,
		SecureErrorLogMessage:         true,
		DisableHeaderNamesNormalizing: true,
	}
	protocolContext := &ProtocolServerContext{
		server:       server,
		conn:         serverConn,
		idleConnTime: new(atomic.Int64),
		isTLS:        true,
	}
	stream := &testProtocolStream{Context: context.Background()}
	requestCtx := protocolContext.AcquireRequestCtx(serverConn, stream)
	defer protocolContext.ReleaseRequestCtx(requestCtx)

	if !requestCtx.Request.isTLS {
		t.Error("Request.isTLS = false, want true")
	}
	if !requestCtx.Response.Header.noDefaultContentType || !requestCtx.Response.Header.noDefaultDate {
		t.Error("response header defaults weren't disabled")
	}
	if !requestCtx.Request.Header.secureErrorLogMessage || !requestCtx.Response.Header.secureErrorLogMessage ||
		!requestCtx.Request.secureErrorLogMessage || !requestCtx.Response.secureErrorLogMessage {
		t.Error("SecureErrorLogMessage wasn't applied")
	}
	if !requestCtx.Request.Header.disableNormalizing || !requestCtx.Response.Header.disableNormalizing {
		t.Error("DisableHeaderNamesNormalizing wasn't applied")
	}
}

func TestIsTLSConnUnwrapsPerIPConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	if isTLSConn(serverConn) {
		t.Error("plain connection reported as TLS")
	}
	tlsConn := tls.Server(serverConn, &tls.Config{})
	if !isTLSConn(tlsConn) {
		t.Error("TLS connection not reported as TLS")
	}
	if !isTLSConn(&perIPConn{Conn: tlsConn}) {
		t.Error("per-IP wrapped TLS connection not reported as TLS")
	}
}

func newTestProtocolServerContext(server *Server, conn net.Conn) *ProtocolServerContext {
	return &ProtocolServerContext{server: server, conn: conn, idleConnTime: new(atomic.Int64)}
}

func TestProtocolReleaseTwicePanicsAfterTimeout(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	protocolContext := newTestProtocolServerContext(&Server{}, serverConn)
	other := protocolContext.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
	defer protocolContext.ReleaseRequestCtx(other)
	requestCtx := protocolContext.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
	requestCtx.TimeoutError("timeout")
	protocolContext.ReleaseRequestCtx(requestCtx)
	defer func() {
		if recover() == nil {
			t.Fatal("a second release of a timed-out context went unnoticed")
		}
	}()
	protocolContext.ReleaseRequestCtx(requestCtx)
}

func TestProtocolRequestCtxDeadlinesDontReachSharedConn(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	conn := &protocolDeadlineConn{Conn: serverConn}
	protocolContext := newTestProtocolServerContext(&Server{}, conn)
	requestCtx := protocolContext.AcquireRequestCtx(conn, &testProtocolStream{Context: context.Background()})
	defer protocolContext.ReleaseRequestCtx(requestCtx)
	deadline := time.Now().Add(time.Second)
	if err := requestCtx.SetReadDeadline(deadline); !errors.Is(err, ErrProtocolNotSupported) {
		t.Fatalf("SetReadDeadline() = %v, want %v", err, ErrProtocolNotSupported)
	}
	if err := requestCtx.SetWriteDeadline(deadline); !errors.Is(err, ErrProtocolNotSupported) {
		t.Fatalf("SetWriteDeadline() = %v, want %v", err, ErrProtocolNotSupported)
	}
	if n := conn.readDeadlineCount(); n != 0 {
		t.Fatalf("shared connection got %d read deadlines", n)
	}
}

func TestProtocolIdleAccountingWithConcurrentStreams(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	protocolContext := newTestProtocolServerContext(&Server{}, serverConn)
	var wg sync.WaitGroup
	var idleWhileActive atomic.Int64
	for range 4 {
		wg.Go(func() {
			for range 2000 {
				requestCtx := protocolContext.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
				if protocolContext.idleConnTime.Load() != 0 {
					idleWhileActive.Add(1)
				}
				protocolContext.ReleaseRequestCtx(requestCtx)
			}
		})
	}
	wg.Wait()
	if n := idleWhileActive.Load(); n != 0 {
		t.Fatalf("connection counted idle %d times while a request was active", n)
	}
	if protocolContext.idleConnTime.Load() == 0 {
		t.Fatal("connection isn't idle after every request was released")
	}
}

func registerTestCleartextProtocol(t *testing.T, server *Server) {
	t.Helper()
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("PROTOCOL-PREFACE"),
		Handler: protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error {
			return nil
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
}

func TestServerProtocolDetectionCountsTowardIdle(t *testing.T) {
	// Shutdown closes a connection that never sends a byte once it counts as
	// idle, so it must be registered while the preface is awaited.
	server := &Server{Handler: func(*RequestCtx) {}}
	registerTestCleartextProtocol(t, server)
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(serverConn) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		server.idleConnsMu.Lock()
		_, registered := server.idleConns[serverConn]
		server.idleConnsMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a connection awaiting its preface isn't tracked for idle shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	clientConn.Close()
	if err := <-served; err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
}

func TestServerProtocolDetectionFirstReadMatchesHTTP1(t *testing.T) {
	serve := func(withProtocol bool, client func(net.Conn)) ([]byte, error) {
		server := &Server{Handler: func(*RequestCtx) {}, ReadTimeout: 50 * time.Millisecond, Logger: &testLogger{}}
		if withProtocol {
			registerTestCleartextProtocol(t, server)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		clientConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		serverConn, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- server.ServeConn(serverConn) }()
		var response []byte
		readDone := make(chan struct{})
		go func() {
			response, _ = io.ReadAll(clientConn)
			close(readDone)
		}()
		client(clientConn)
		serveErr := <-served
		// The server closed its side, which ends the read.
		<-readDone
		clientConn.Close()
		return response, serveErr
	}
	for _, tc := range []struct {
		name   string
		client func(net.Conn)
	}{
		{"closed before the first byte", func(c net.Conn) { c.Close() }},
		{"first-byte timeout", func(net.Conn) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantResponse, wantErr := serve(false, tc.client)
			gotResponse, gotErr := serve(true, tc.client)
			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("ServeConn() error = %v, without protocols %v", gotErr, wantErr)
			}
			statusLine := func(b []byte) string {
				line, _, _ := bufio.NewReader(bytes.NewReader(b)).ReadLine()
				return string(line)
			}
			if got, want := statusLine(gotResponse), statusLine(wantResponse); got != want {
				t.Fatalf("response %q, without protocols %q", got, want)
			}
		})
	}
}

func TestServerDeclinedUpgradeKeepsConnActive(t *testing.T) {
	idleDuringHandler := make(chan int64, 1)
	server := &Server{}
	server.Handler = func(ctx *RequestCtx) {
		server.idleConnsMu.Lock()
		idleConnTime := server.idleConns[ctx.Conn()]
		server.idleConnsMu.Unlock()
		idleDuringHandler <- idleConnTime.Load()
	}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "test-protocol",
		Handler:               upgraderHandlerFunc{},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	go func() { _ = server.ServeConn(serverConn) }()
	go func() {
		_, _ = clientConn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: test-protocol\r\n\r\n"))
		_, _ = io.Copy(io.Discard, clientConn)
	}()
	if idle := <-idleDuringHandler; idle != 0 {
		t.Fatal("the connection counts as idle while the HTTP/1 handler serves a declined upgrade")
	}
}

type acceptingUpgrader struct{}

func (acceptingUpgrader) ServeConn(*ProtocolServerContext, net.Conn) error { return nil }

func (acceptingUpgrader) UpgradeConn(ctx *ProtocolServerContext, c net.Conn, _ *Request, _ RequestConfig) (bool, error) {
	ctx.ReleaseRequestCtx(ctx.AcquireRequestCtx(c, &testProtocolStream{Context: context.Background()}))
	return true, nil
}

func TestServerAcceptedUpgradeReportsActiveOnce(t *testing.T) {
	var mu sync.Mutex
	var states []ConnState
	server := &Server{
		Handler: func(*RequestCtx) {},
		ConnState: func(_ net.Conn, state ConnState) {
			mu.Lock()
			states = append(states, state)
			mu.Unlock()
		},
	}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "test-protocol",
		Handler:               acceptingUpgrader{},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	go func() {
		_, _ = clientConn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: test-protocol\r\n\r\n"))
		_, _ = io.Copy(io.Discard, clientConn)
	}()
	_ = server.ServeConn(serverConn)
	clientConn.Close()
	mu.Lock()
	defer mu.Unlock()
	want := []ConnState{StateActive, StateIdle}
	var got []ConnState
	for _, state := range states {
		if state == StateActive || state == StateIdle {
			got = append(got, state)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ConnState transitions = %v, want %v", got, want)
	}
}

func TestServeProtocolConnRejectsTLSWithoutHandshake(t *testing.T) {
	server := &Server{Concurrency: 1}
	if !server.tryAcquireConcurrency() {
		t.Fatal("tryAcquireConcurrency() = false")
	}
	defer server.releaseConcurrency()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	served := make(chan error, 1)
	go func() {
		served <- server.ServeProtocolConn(tls.Server(serverConn, &tls.Config{}), protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error {
			return nil
		}))
	}()
	select {
	case err := <-served:
		if !errors.Is(err, ErrConcurrencyLimit) {
			t.Fatalf("ServeProtocolConn() error = %v, want %v", err, ErrConcurrencyLimit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rejecting a TLS connection waited for its handshake")
	}
}

func TestProtocolConnStartedBeforeServeObservesShutdown(t *testing.T) {
	const preface = "PROTOCOL-PREFACE"
	server := &Server{Handler: func(*RequestCtx) {}, Logger: &testLogger{}}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte(preface),
		Handler: protocolHandlerFunc(func(ctx *ProtocolServerContext, _ net.Conn) error {
			done := ctx.Done()
			if done == nil {
				return errors.New("Done() returned nil")
			}
			select {
			case <-done:
				return nil
			case <-time.After(2 * time.Second):
				return errors.New("shutdown wasn't signaled")
			}
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(serverConn) }()
	if _, err := io.WriteString(clientConn, preface); err != nil {
		t.Fatalf("writing preface: %v", err)
	}

	// Serve starts after the protocol connection did.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ln) }()
	waitFor(t, "Serve to register its listener", func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		return len(server.ln) != 0
	})
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error: %v", err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerReuseAfterTimedOutShutdownResetsProtocolDone(t *testing.T) {
	const preface = "PROTOCOL-PREFACE"
	var mu sync.Mutex
	var doneClosedAtStart []bool
	server := &Server{Handler: func(*RequestCtx) {}, Logger: &testLogger{}}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte(preface),
		Handler: protocolHandlerFunc(func(ctx *ProtocolServerContext, c net.Conn) error {
			closed := false
			select {
			case <-ctx.Done():
				closed = true
			default:
			}
			mu.Lock()
			doneClosedAtStart = append(doneClosedAtStart, closed)
			mu.Unlock()
			var one [1]byte
			_, err := c.Read(one[:])
			return err
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	started := func(n int) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(doneClosedAtStart) == n
		}
	}
	serve := func() (net.Listener, chan error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(ln) }()
		return ln, serveDone
	}
	connect := func(ln net.Listener) net.Conn {
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(client, preface); err != nil {
			t.Fatal(err)
		}
		return client
	}

	ln1, serveDone1 := serve()
	client1 := connect(ln1)
	defer client1.Close()
	waitFor(t, "the first protocol connection", started(1))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.ShutdownWithContext(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShutdownWithContext() error = %v, want deadline exceeded", err)
	}
	if err := <-serveDone1; err != nil {
		t.Fatalf("Serve() error: %v", err)
	}

	ln2, serveDone2 := serve()
	client2 := connect(ln2)
	waitFor(t, "the second protocol connection", started(2))
	mu.Lock()
	closedAtStart := doneClosedAtStart[1]
	mu.Unlock()
	if closedAtStart {
		t.Fatal("a protocol connection on the reused server saw Done closed at once")
	}
	client2.Close()
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}
	if err := <-serveDone2; err != nil {
		t.Fatalf("Serve() error: %v", err)
	}
}

func TestServerCleartextDetectionReportsFirstByte(t *testing.T) {
	const preface = "PROTOCOL-PREFACE"
	for _, tc := range []struct {
		name         string
		rest         string
		wantProtocol bool
	}{
		{"protocol", preface[1:], true},
		{"http1", "OST / HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var states []ConnState
			var serverSide net.Conn
			handlerStarted := make(chan *ProtocolServerContext, 1)
			server := &Server{
				Handler: func(ctx *RequestCtx) { ctx.SetBodyString("ok") },
				ConnState: func(c net.Conn, state ConnState) {
					mu.Lock()
					defer mu.Unlock()
					if state == StateNew {
						serverSide = c
					}
					states = append(states, state)
				},
				Logger: &testLogger{},
			}
			if err := server.RegisterProtocol(ProtocolRegistration{
				CleartextPreface: []byte(preface),
				Handler: protocolHandlerFunc(func(ctx *ProtocolServerContext, c net.Conn) error {
					handlerStarted <- ctx
					var one [1]byte
					_, err := c.Read(one[:])
					return err
				}),
			}); err != nil {
				t.Fatalf("RegisterProtocol() error: %v", err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(ln) }()
			client, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			if _, err := io.WriteString(client, preface[:1]); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "StateActive after the first byte", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return slices.Contains(states, StateActive)
			})
			idleTime := func() int64 {
				mu.Lock()
				c := serverSide
				mu.Unlock()
				server.idleConnsMu.Lock()
				defer server.idleConnsMu.Unlock()
				return server.idleConns[c].Load()
			}
			if got := idleTime(); got != 0 {
				t.Fatalf("idle time after the first byte = %d, want 0", got)
			}
			if _, err := io.WriteString(client, tc.rest); err != nil {
				t.Fatal(err)
			}
			if tc.wantProtocol {
				ctx := <-handlerStarted
				if got := idleTime(); got != 0 {
					t.Fatalf("idle time once the protocol runs = %d, want 0", got)
				}
				ctx.stateMu.Lock()
				reported := ctx.activeReported
				ctx.stateMu.Unlock()
				if !reported {
					t.Fatal("the protocol context doesn't know StateActive was reported")
				}
			} else {
				var response [64]byte
				if _, err := client.Read(response[:]); err != nil {
					t.Fatalf("reading the response: %v", err)
				}
			}
			client.Close()
			waitFor(t, "StateClosed", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return slices.Contains(states, StateClosed)
			})
			mu.Lock()
			active := 0
			for _, state := range states {
				if state == StateActive {
					active++
				}
			}
			got := slices.Clone(states)
			mu.Unlock()
			if active != 1 {
				t.Fatalf("StateActive reported %d times in %v", active, got)
			}
			if err := server.Shutdown(); err != nil {
				t.Fatalf("Shutdown() error: %v", err)
			}
			<-serveDone
		})
	}
}

func TestProtocolLateReleaseAfterServeConnReturned(t *testing.T) {
	var mu sync.Mutex
	var states []ConnState
	server := &Server{ConnState: func(_ net.Conn, state ConnState) {
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	}}
	var protoCtx *ProtocolServerContext
	var requestCtx *RequestCtx
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	err := server.ServeProtocolConn(serverConn, protocolHandlerFunc(func(ctx *ProtocolServerContext, c net.Conn) error {
		protoCtx = ctx
		requestCtx = ctx.AcquireRequestCtx(c, &testProtocolStream{Context: context.Background()})
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeProtocolConn() error: %v", err)
	}
	// ServeConn returned: the connection is closed and its idle time pooled.
	protoCtx.ReleaseRequestCtx(requestCtx)
	mu.Lock()
	got := slices.Clone(states)
	mu.Unlock()
	if got[len(got)-1] != StateClosed {
		t.Fatalf("ConnState sequence %v, want it to end with StateClosed", got)
	}
	protoCtx.requestMu.Lock()
	cached := len(protoCtx.requestCache)
	protoCtx.requestMu.Unlock()
	if cached != 0 {
		t.Fatal("a late release resurrected the drained request context cache")
	}
}

type readDeadlineLogConn struct {
	net.Conn

	mu        sync.Mutex
	deadlines []time.Time
}

func (c *readDeadlineLogConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(deadline)
}

func (c *readDeadlineLogConn) hasDeadlineAtOrAfter(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, deadline := range c.deadlines {
		if !deadline.IsZero() && !deadline.Before(at) {
			return true
		}
	}
	return false
}

func TestServerDetectionRestartsReadTimeoutAtFirstByte(t *testing.T) {
	server := &Server{
		Handler:           func(*RequestCtx) {},
		ReadTimeout:       time.Second,
		ReduceMemoryUsage: true,
		Logger:            &testLogger{},
	}
	registerTestCleartextProtocol(t, server)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	conn := &readDeadlineLogConn{Conn: accepted}
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(conn) }()
	waitFor(t, "the accept-time deadline", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return len(conn.deadlines) != 0
	})
	sentAt := time.Now()
	if _, err := io.WriteString(client, "P"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ReadTimeout to restart at the first byte", func() bool {
		return conn.hasDeadlineAtOrAfter(sentAt.Add(server.ReadTimeout))
	})
	client.Close()
	<-served
}

func TestProtocolAbandonRequestCtx(t *testing.T) {
	var mu sync.Mutex
	var states []ConnState
	server := &Server{ConnState: func(_ net.Conn, state ConnState) {
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	}}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx := newTestProtocolServerContext(server, serverConn)
	stream := &testProtocolStream{Context: context.Background()}

	first := ctx.AcquireRequestCtx(serverConn, stream)
	ctx.AbandonRequestCtx(first)
	mu.Lock()
	got := slices.Clone(states)
	mu.Unlock()
	if want := []ConnState{StateActive, StateIdle}; !slices.Equal(got, want) {
		t.Fatalf("ConnState sequence %v, want %v", got, want)
	}
	second := ctx.AcquireRequestCtx(serverConn, stream)
	if second == first {
		t.Fatal("an abandoned request context was reused")
	}
	ctx.ReleaseRequestCtx(second)
	// Its holder may keep using it.
	first.Response.SetBodyString("still mine")

	defer func() {
		if recover() == nil {
			t.Fatal("abandoning a context twice didn't panic")
		}
	}()
	ctx.AbandonRequestCtx(first)
}

func TestProtocolRequestLogsLikeHTTP1(t *testing.T) {
	logger := &testLogger{}
	server := &Server{Logger: logger, StreamRequestBody: true}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx := newTestProtocolServerContext(server, serverConn)
	requestCtx := ctx.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
	requestCtx.Request.SetBodyStream(strings.NewReader("abc"), 3)
	_ = requestCtx.Request.Body()
	ctx.ReleaseRequestCtx(requestCtx)
	logger.lock.Lock()
	out := logger.out
	logger.lock.Unlock()
	if !strings.Contains(out, "Request.Body() reads the entire stream") {
		t.Fatalf("the HTTP/1 warning wasn't logged for a protocol request; log = %q", out)
	}
}

type numberingUpgrader struct {
	protocolHandlerFunc

	mu   *sync.Mutex
	nums *[]uint64
}

func (u numberingUpgrader) UpgradeConn(ctx *ProtocolServerContext, c net.Conn, _ *Request, _ RequestConfig) (bool, error) {
	requestCtx := ctx.AcquireRequestCtx(c, &testProtocolStream{Context: context.Background()})
	u.mu.Lock()
	*u.nums = append(*u.nums, requestCtx.ConnRequestNum())
	u.mu.Unlock()
	ctx.ReleaseRequestCtx(requestCtx)
	return true, nil
}

// An upgraded connection's first stream is the Upgrade request itself and
// keeps its number; later streams continue the connection's numbering.
func TestUpgradedProtocolContinuesConnRequestNum(t *testing.T) {
	var mu sync.Mutex
	var nums []uint64
	server := &Server{Handler: func(ctx *RequestCtx) {
		mu.Lock()
		nums = append(nums, ctx.ConnRequestNum())
		mu.Unlock()
	}}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "test-protocol",
		Handler: numberingUpgrader{
			protocolHandlerFunc: func(*ProtocolServerContext, net.Conn) error { return nil },
			mu:                  &mu,
			nums:                &nums,
		},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	clientErr := make(chan error, 1)
	go func() {
		// Not pipelined: a request behind an unflushed response isn't upgraded.
		if _, err := io.WriteString(clientConn, "GET /a HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
			clientErr <- err
			return
		}
		reader := bufio.NewReader(clientConn)
		var resp Response
		if err := resp.Read(reader); err != nil {
			clientErr <- err
			return
		}
		_, err := io.WriteString(clientConn, "GET /up HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: test-protocol\r\n\r\n")
		clientErr <- err
		_, _ = io.Copy(io.Discard, reader)
	}()
	if err := server.ServeConn(serverConn); err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
	clientConn.Close()
	if err := <-clientErr; err != nil {
		t.Fatalf("client: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []uint64{1, 2}; !slices.Equal(nums, want) {
		t.Fatalf("ConnRequestNum sequence = %v, want %v (the Upgrade request is number 2 and the protocol's first stream)", nums, want)
	}
}

type deadlineRefusingConn struct {
	net.Conn
}

var errDeadlineRefused = errors.New("test: deadlines unsupported")

func (deadlineRefusingConn) SetReadDeadline(time.Time) error { return errDeadlineRefused }

// A connection that refuses read deadlines ends before any request, with or
// without a registered protocol.
func TestServerDetectionDeadlineErrorMatchesHTTP1(t *testing.T) {
	run := func(register bool) (int, error) {
		handled := 0
		server := &Server{ReadTimeout: time.Second, Handler: func(*RequestCtx) { handled++ }}
		if register {
			registerTestCleartextProtocol(t, server)
		}
		serverConn, clientConn := net.Pipe()
		defer clientConn.Close()
		go func() {
			_, _ = io.WriteString(clientConn, "GET /x HTTP/1.1\r\nHost: example.com\r\n\r\n")
			_, _ = io.Copy(io.Discard, clientConn)
		}()
		err := server.ServeConn(deadlineRefusingConn{Conn: serverConn})
		return handled, err
	}
	plainHandled, plainErr := run(false)
	handled, err := run(true)
	if plainHandled != 0 || !errors.Is(plainErr, errDeadlineRefused) {
		t.Fatalf("without protocols: handled=%d err=%v", plainHandled, plainErr)
	}
	if handled != plainHandled || !errors.Is(err, errDeadlineRefused) {
		t.Fatalf("with a protocol: handled=%d err=%v, without protocols handled=%d err=%v", handled, err, plainHandled, plainErr)
	}
}

func TestProtocolAcquireRequestCtxLazyRequestTime(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	for _, lazy := range []bool{false, true} {
		ctx := newTestProtocolServerContext(&Server{LazyRequestTime: lazy}, serverConn)
		requestCtx := ctx.AcquireRequestCtx(serverConn, &testProtocolStream{Context: context.Background()})
		if got := requestCtx.time.IsZero(); got != lazy {
			t.Fatalf("LazyRequestTime=%v: time unset = %v", lazy, got)
		}
		before := time.Now()
		if got := requestCtx.Time(); got.Before(before) && lazy {
			t.Fatalf("LazyRequestTime: Time() = %v, read before %v", got, before)
		}
		ctx.ReleaseRequestCtx(requestCtx)
	}
}

type closedErrUpgrader struct{ protocolHandlerFunc }

func (closedErrUpgrader) UpgradeConn(*ProtocolServerContext, net.Conn, *Request, RequestConfig) (bool, error) {
	return true, fmt.Errorf("read: %w", net.ErrClosed)
}

// A connection the server closed itself ends without an error, as for HTTP/1.
func TestProtocolServerCloseErrorIsMappedToNil(t *testing.T) {
	serve := func(t *testing.T, server *Server, input string) error {
		t.Helper()
		serverConn, clientConn := net.Pipe()
		defer clientConn.Close()
		go func() {
			_, _ = io.WriteString(clientConn, input)
			_, _ = io.Copy(io.Discard, clientConn)
		}()
		return server.ServeConn(serverConn)
	}
	server := &Server{}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "test-protocol",
		Handler: closedErrUpgrader{protocolHandlerFunc: func(*ProtocolServerContext, net.Conn) error {
			return fmt.Errorf("read: %w", net.ErrClosed)
		}},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if err := serve(t, server, "PROTOCOL-PREFACE"); err != nil {
		t.Fatalf("direct protocol: ServeConn() error = %v, want nil", err)
	}
	if err := serve(t, server, "GET /up HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: test-protocol\r\n\r\n"); err != nil {
		t.Fatalf("upgraded protocol: ServeConn() error = %v, want nil", err)
	}
}

type countingUpgrader struct {
	protocolHandlerFunc

	calls *atomic.Int64
}

func (u countingUpgrader) UpgradeConn(*ProtocolServerContext, net.Conn, *Request, RequestConfig) (bool, error) {
	u.calls.Add(1)
	return true, nil
}

// Upgrade lists protocol names, compared case-insensitively (RFC 9110 §7.8).
func TestServerUpgradeTokenMatching(t *testing.T) {
	for _, tc := range []struct {
		upgrade  string
		upgraded bool
	}{
		{"test-protocol", true},
		{"TEST-Protocol", true},
		{"test-protocol, websocket", true},
		{"websocket , test-protocol", true},
		{"test-protocolx", false},
		{"websocket", false},
		{"test-protocol/1", false},
	} {
		calls := new(atomic.Int64)
		served := new(atomic.Int64)
		server := &Server{Handler: func(*RequestCtx) { served.Add(1) }}
		if err := server.RegisterProtocol(ProtocolRegistration{
			CleartextPreface:      []byte("PROTOCOL-PREFACE"),
			CleartextUpgradeToken: "test-protocol",
			Handler: countingUpgrader{
				protocolHandlerFunc: func(*ProtocolServerContext, net.Conn) error { return nil },
				calls:               calls,
			},
		}); err != nil {
			t.Fatalf("RegisterProtocol() error: %v", err)
		}
		serverConn, clientConn := net.Pipe()
		go func() {
			_, _ = io.WriteString(clientConn, "GET /up HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: "+tc.upgrade+"\r\n\r\n")
			// An HTTP/1 response ends the client's interest; an upgrade ends the connection.
			var resp Response
			_ = resp.Read(bufio.NewReader(clientConn))
			clientConn.Close()
		}()
		if err := server.ServeConn(serverConn); err != nil {
			t.Fatalf("Upgrade %q: ServeConn() error: %v", tc.upgrade, err)
		}
		if got := calls.Load() == 1; got != tc.upgraded {
			t.Fatalf("Upgrade %q: upgraded = %v, want %v", tc.upgrade, got, tc.upgraded)
		}
		if got := served.Load() == 1; got == tc.upgraded {
			t.Fatalf("Upgrade %q: served as HTTP/1 = %v", tc.upgrade, got)
		}
	}
}

func TestServerRegisterProtocolUpgradeTokenCaseInsensitive(t *testing.T) {
	server := &Server{}
	handler := upgraderHandlerFunc{protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil })}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("AAAA"), CleartextUpgradeToken: "test-protocol", Handler: handler,
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("BBBB"), CleartextUpgradeToken: "TEST-PROTOCOL", Handler: handler,
	})
	if err == nil {
		t.Fatal("registering an upgrade token that differs only in case succeeded")
	}
}

func TestProtocolAcquireReplacementRequestCtx(t *testing.T) {
	var mu sync.Mutex
	var states []ConnState
	server := &Server{ConnState: func(_ net.Conn, state ConnState) {
		mu.Lock()
		states = append(states, state)
		mu.Unlock()
	}}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx := newTestProtocolServerContext(server, serverConn)
	stream := &testProtocolStream{Context: context.Background()}

	first := ctx.AcquireRequestCtx(serverConn, stream)
	replacement := ctx.AcquireReplacementRequestCtx(stream, first)
	if replacement == first {
		t.Fatal("the replacement is the replaced context")
	}
	if got, want := replacement.ConnRequestNum(), first.ConnRequestNum(); got != want {
		t.Fatalf("replacement ConnRequestNum() = %d, want %d", got, want)
	}
	if replacement.ConnID() != first.ConnID() || replacement.Conn() != first.Conn() {
		t.Fatal("the replacement doesn't share the replaced context's connection")
	}
	ctx.ReleaseRequestCtx(replacement)
	next := ctx.AcquireRequestCtx(serverConn, stream)
	if got := next.ConnRequestNum(); got != first.ConnRequestNum()+1 {
		t.Fatalf("the request after a replacement is number %d, want %d", got, first.ConnRequestNum()+1)
	}
	ctx.ReleaseRequestCtx(next)
	mu.Lock()
	got := slices.Clone(states)
	mu.Unlock()
	if want := []ConnState{StateActive}; !slices.Equal(got, want) {
		t.Fatalf("ConnState sequence %v, want %v (the replaced context is still active)", got, want)
	}
	ctx.AbandonRequestCtx(first)
	mu.Lock()
	got = slices.Clone(states)
	mu.Unlock()
	if want := []ConnState{StateActive, StateIdle}; !slices.Equal(got, want) {
		t.Fatalf("ConnState sequence %v, want %v", got, want)
	}
}

// A config GetConfigForClient returns gets the registrations' ALPN order and
// version floor; a prepared one is used as is, a shared one is cloned.
func TestServerRegisterProtocolWrapsGetConfigForClient(t *testing.T) {
	user := &tls.Config{NextProtos: []string{"http/1.1"}}
	prepared := &tls.Config{NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12}
	tooLow := &tls.Config{MaxVersion: tls.VersionTLS11}
	var want *tls.Config
	var wantErr error
	calls := 0
	server := &Server{TLSConfig: &tls.Config{
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			calls++
			return want, wantErr
		},
	}}
	handler := protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil })
	if err := server.RegisterProtocol(ProtocolRegistration{
		ALPN: []string{"h2"}, FallbackALPN: []string{"http/1.1"}, MinTLSVersion: tls.VersionTLS12, Handler: handler,
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	callback := server.TLSConfig.GetConfigForClient
	hello := &tls.ClientHelloInfo{}

	want = user
	got, err := callback(hello)
	if err != nil || got == nil || got == user {
		t.Fatalf("a shared returned config wasn't cloned: %v, %v", got, err)
	}
	if !slices.Equal(got.NextProtos, []string{"h2", "http/1.1"}) || got.MinVersion != tls.VersionTLS12 {
		t.Fatalf("prepared returned config: NextProtos %v, MinVersion %x", got.NextProtos, got.MinVersion)
	}
	if !slices.Equal(user.NextProtos, []string{"http/1.1"}) || user.MinVersion != 0 {
		t.Fatalf("the user's config was mutated: %v %x", user.NextProtos, user.MinVersion)
	}
	want = prepared
	if got, err = callback(hello); err != nil || got != prepared {
		t.Fatalf("a prepared returned config was replaced: %v, %v", got, err)
	}
	want = nil
	if got, err = callback(hello); err != nil || got != nil {
		t.Fatalf("nil return = %v, %v", got, err)
	}
	wantErr = errors.New("no config")
	if _, err = callback(hello); err == nil {
		t.Fatal("the callback's error was dropped")
	}
	wantErr = nil
	want = tooLow
	if _, err = callback(hello); err == nil {
		t.Fatal("a returned config below the version floor was accepted")
	}
	if calls != 5 {
		t.Fatalf("user callback called %d times, want 5", calls)
	}

	// A later registration rebuilds the wrapper around the user's callback.
	if err := server.RegisterProtocol(ProtocolRegistration{
		ALPN: []string{"foo"}, Handler: handler,
	}); err != nil {
		t.Fatalf("second RegisterProtocol() error: %v", err)
	}
	want = user
	got, err = server.TLSConfig.GetConfigForClient(hello)
	if err != nil || !slices.Equal(got.NextProtos, []string{"h2", "foo", "http/1.1"}) {
		t.Fatalf("after two registrations: NextProtos %v, err %v", got.NextProtos, err)
	}
	if calls != 6 {
		t.Fatalf("user callback called %d times, want 6 (wrappers stacked)", calls)
	}
}

// A request that fails to parse inside a replayed cleartext prefix is
// answered and the rest of its input drained before the close.
func TestServerDetectionParseErrorDrainsInput(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	server := &Server{Handler: func(*RequestCtx) {}, Logger: &testLogger{}}
	registerTestCleartextProtocol(t, server)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		<-serveDone
	})
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer client.Close()
	// A strict prefix of the protocol preface, then a request line HTTP/1
	// rejects, then more than the parser consumes.
	if _, err := io.WriteString(client, "PROTOCOL-PRE * HTTP/9.9\r\n\r\n"+strings.Repeat("x", 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	reader := bufio.NewReader(client)
	var resp Response
	if err := resp.Read(reader); err != nil {
		t.Fatalf("reading the error response: %v", err)
	}
	if resp.StatusCode() != StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode())
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("the close carried unread input: %v", err)
	}
}

// Shutdown signals and waits for protocol connections served directly, with
// no listener; a deadline closes them.
func TestServerShutdownWithoutListenerStopsProtocolConns(t *testing.T) {
	server := &Server{Logger: &testLogger{}}
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ServeProtocolConn(serverConn, protocolHandlerFunc(func(ctx *ProtocolServerContext, _ net.Conn) error {
			<-ctx.Done()
			return nil
		}))
	}()
	waitFor(t, "protocol connection registered", func() bool { return server.protocolConnCount() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.ShutdownWithContext(ctx); err != nil {
		t.Fatalf("ShutdownWithContext() error: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeProtocolConn() error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeProtocolConn() didn't return after Shutdown")
	}

	// A handler that ignores Done is closed at the deadline, without an error
	// (a TCP connection: the server-closed read error is net.ErrClosed).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	tcpClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcpClient.Close()
	serverConn = <-accepted
	go func() {
		serveDone <- server.ServeProtocolConn(serverConn, protocolHandlerFunc(func(_ *ProtocolServerContext, c net.Conn) error {
			var one [1]byte
			_, err := c.Read(one[:])
			return err
		}))
	}()
	waitFor(t, "protocol connection registered", func() bool { return server.protocolConnCount() == 1 })
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := server.ShutdownWithContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShutdownWithContext() error = %v, want deadline exceeded", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("ServeProtocolConn() error after the deadline close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the protocol connection wasn't closed at the deadline")
	}
}

// Without a listener an HTTP/1 connection served directly is left alone.
func TestServerShutdownWithoutListenerLeavesHTTP1Conns(t *testing.T) {
	server := &Server{Handler: func(ctx *RequestCtx) { ctx.SetBodyString("ok") }}
	serverConn, clientConn := net.Pipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(serverConn) }()
	reader := bufio.NewReader(clientConn)
	request := func() {
		t.Helper()
		if _, err := io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		var resp Response
		if err := resp.Read(reader); err != nil {
			t.Fatal(err)
		}
		if string(resp.Body()) != "ok" {
			t.Fatalf("body %q", resp.Body())
		}
	}
	request()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := server.ShutdownWithContext(ctx); err != nil {
		t.Fatalf("ShutdownWithContext() error: %v", err)
	}
	request()
	clientConn.Close()
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
}

// A protocol connection that starts while Shutdown waits is told to stop.
func TestProtocolConnRegisteringDuringShutdownSeesDone(t *testing.T) {
	server := &Server{Logger: &testLogger{}}
	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	second, secondPeer := net.Pipe()
	defer secondPeer.Close()
	secondDone := make(chan bool, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ServeProtocolConn(first, protocolHandlerFunc(func(ctx *ProtocolServerContext, _ net.Conn) error {
			<-ctx.Done()
			// Shutdown is waiting for this connection now.
			return server.ServeProtocolConn(second, protocolHandlerFunc(func(ctx *ProtocolServerContext, _ net.Conn) error {
				select {
				case <-ctx.Done():
					secondDone <- true
				default:
					secondDone <- false
				}
				return nil
			}))
		}))
	}()
	waitFor(t, "protocol connection registered", func() bool { return server.protocolConnCount() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.ShutdownWithContext(ctx); err != nil {
		t.Fatalf("ShutdownWithContext() error: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("ServeProtocolConn() error: %v", err)
	}
	if !<-secondDone {
		t.Fatal("a protocol connection started during Shutdown got an open Done")
	}
}

// Upgrade protocol versions compare exactly; only the name is case-insensitive.
func TestServerUpgradeTokenVersionIsCaseSensitive(t *testing.T) {
	for _, tc := range []struct {
		upgrade  string
		upgraded bool
	}{
		{"example/RevA", true},
		{"EXAMPLE/RevA", true},
		{"example/reva", false},
		{"example", false},
	} {
		calls := new(atomic.Int64)
		server := &Server{Handler: func(*RequestCtx) {}}
		if err := server.RegisterProtocol(ProtocolRegistration{
			CleartextPreface:      []byte("PROTOCOL-PREFACE"),
			CleartextUpgradeToken: "example/RevA",
			Handler: countingUpgrader{
				protocolHandlerFunc: func(*ProtocolServerContext, net.Conn) error { return nil },
				calls:               calls,
			},
		}); err != nil {
			t.Fatalf("RegisterProtocol() error: %v", err)
		}
		serverConn, clientConn := net.Pipe()
		go func() {
			_, _ = io.WriteString(clientConn, "GET /up HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: "+tc.upgrade+"\r\n\r\n")
			var resp Response
			_ = resp.Read(bufio.NewReader(clientConn))
			clientConn.Close()
		}()
		if err := server.ServeConn(serverConn); err != nil {
			t.Fatalf("Upgrade %q: ServeConn() error: %v", tc.upgrade, err)
		}
		if got := calls.Load() == 1; got != tc.upgraded {
			t.Fatalf("Upgrade %q: upgraded = %v, want %v", tc.upgrade, got, tc.upgraded)
		}
	}
	// Distinct versions of one protocol coexist; the same version doesn't.
	server := &Server{}
	handler := upgraderHandlerFunc{protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil })}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("AAAA"), CleartextUpgradeToken: "example/RevA", Handler: handler,
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("BBBB"), CleartextUpgradeToken: "EXAMPLE/reva", Handler: handler,
	}); err != nil {
		t.Fatalf("registering a different version failed: %v", err)
	}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("CCCC"), CleartextUpgradeToken: "EXAMPLE/RevA", Handler: handler,
	}); err == nil {
		t.Fatal("registering the same version with another case succeeded")
	}
}

// A cached context keeps no stale timestamp under LazyRequestTime.
func TestProtocolCachedRequestCtxLazyRequestTime(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	server := &Server{}
	ctx := newTestProtocolServerContext(server, serverConn)
	stream := &testProtocolStream{Context: context.Background()}
	first := ctx.AcquireRequestCtx(serverConn, stream)
	if first.time.IsZero() {
		t.Fatal("eager acquisition left the time unset")
	}
	ctx.ReleaseRequestCtx(first)
	server.LazyRequestTime = true
	second := ctx.AcquireRequestCtx(serverConn, stream)
	defer ctx.ReleaseRequestCtx(second)
	if !second.time.IsZero() {
		t.Fatalf("a cached context kept its old time %v under LazyRequestTime", second.time)
	}
}

// ServeConn doesn't report a connection the protocol closed itself.
func TestServeConnIgnoresTheProtocolsOwnClose(t *testing.T) {
	server := &Server{}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("PROTOCOL-PREFACE"),
		Handler: protocolHandlerFunc(func(_ *ProtocolServerContext, c net.Conn) error {
			return c.Close()
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	// A TCP connection: its second Close reports net.ErrClosed (a pipe's is silent).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()
	serverConn := <-accepted
	go func() {
		_, _ = io.WriteString(clientConn, "PROTOCOL-PREFACE")
		_, _ = io.Copy(io.Discard, clientConn)
	}()
	if err := server.ServeConn(serverConn); err != nil {
		t.Fatalf("ServeConn() error: %v", err)
	}
}

// closeErrConn reports err from Close, as a wrapper that closed the
// connection on its own does.
type closeErrConn struct {
	net.Conn

	err error
}

func (c closeErrConn) Close() error {
	_ = c.Conn.Close()
	return c.err
}

// ServeConn keeps reporting a wrapper's Close error unless a protocol served
// the connection and closed it itself.
func TestServeConnReportsHTTP1CloseErrors(t *testing.T) {
	errOther := errors.New("wrapper close failed")
	serveWith := func(t *testing.T, server *Server, request string, closeErr error) error {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				accepted <- conn
			}
		}()
		clientConn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer clientConn.Close()
		go func() {
			_, _ = io.WriteString(clientConn, request)
			_, _ = io.Copy(io.Discard, clientConn)
		}()
		return server.ServeConn(closeErrConn{<-accepted, closeErr})
	}
	serve := func(t *testing.T, server *Server, request string) error {
		t.Helper()
		return serveWith(t, server, request, net.ErrClosed)
	}
	handler := func(ctx *RequestCtx) { ctx.SetConnectionClose() }
	const request = "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if err := serve(t, &Server{Handler: handler}, request); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("plain HTTP/1 ServeConn() error = %v, want the wrapper's net.ErrClosed", err)
	}

	server := &Server{Handler: handler}
	if err := server.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "example",
		Handler:               acceptingUpgrader{},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if err := serve(t, server, request); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("HTTP/1 request with a protocol registered: ServeConn() error = %v, want the wrapper's net.ErrClosed", err)
	}
	const upgrade = "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: example\r\n\r\n"
	if err := serve(t, server, upgrade); err != nil {
		t.Fatalf("upgraded connection: ServeConn() error = %v", err)
	}
	if err := serve(t, server, "PROTOCOL-PREFACE"); err != nil {
		t.Fatalf("detected protocol: ServeConn() error = %v", err)
	}
	// Another Close error surfaces whoever served the connection.
	if err := serveWith(t, server, request, errOther); !errors.Is(err, errOther) {
		t.Fatalf("HTTP/1 request, other Close error: ServeConn() error = %v", err)
	}
	if err := serveWith(t, server, "PROTOCOL-PREFACE", errOther); !errors.Is(err, errOther) {
		t.Fatalf("detected protocol, other Close error: ServeConn() error = %v", err)
	}

	// A declined upgrade is served as HTTP/1.
	declining := &Server{Handler: handler}
	if err := declining.RegisterProtocol(ProtocolRegistration{
		CleartextPreface:      []byte("PROTOCOL-PREFACE"),
		CleartextUpgradeToken: "example",
		Handler:               upgraderHandlerFunc{protocolHandlerFunc(func(*ProtocolServerContext, net.Conn) error { return nil })},
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if err := serve(t, declining, upgrade); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("declined upgrade: ServeConn() error = %v, want the wrapper's net.ErrClosed", err)
	}

	// A serving error keeps precedence over the Close error.
	failing := &Server{Handler: handler}
	errServe := errors.New("protocol failed")
	if err := failing.RegisterProtocol(ProtocolRegistration{
		CleartextPreface: []byte("PROTOCOL-PREFACE"),
		Handler: protocolHandlerFunc(func(_ *ProtocolServerContext, c net.Conn) error {
			_ = c.Close()
			return errServe
		}),
	}); err != nil {
		t.Fatalf("RegisterProtocol() error: %v", err)
	}
	if err := serve(t, failing, "PROTOCOL-PREFACE"); !errors.Is(err, errServe) {
		t.Fatalf("failing protocol: ServeConn() error = %v, want the serving error", err)
	}
}

// Hijack tells the protocol stream whether the handler meant to answer.
func TestRequestCtxHijackReportsNoResponse(t *testing.T) {
	for _, noResponse := range []bool{false, true} {
		stream := &testProtocolStream{Context: context.Background()}
		var ctx RequestCtx
		ctx.protocolStream = stream
		ctx.HijackSetNoResponse(noResponse)
		ctx.Hijack(func(net.Conn) {})
		stream.mu.Lock()
		rejected, got := stream.hijackRejected, stream.hijackNoResponse
		stream.mu.Unlock()
		if !rejected || got != noResponse {
			t.Fatalf("HijackSetNoResponse(%v): rejected=%v noResponse=%v", noResponse, rejected, got)
		}
	}
}

// The stream gets the final HijackSetNoResponse setting whatever the call order.
func TestRequestCtxHijackFinalNoResponseSetting(t *testing.T) {
	flags := func(stream *testProtocolStream) (bool, bool) {
		stream.mu.Lock()
		defer stream.mu.Unlock()
		return stream.hijackRejected, stream.hijackNoResponse
	}
	stream := &testProtocolStream{Context: context.Background()}
	var ctx RequestCtx
	ctx.protocolStream = stream
	ctx.Hijack(func(net.Conn) {})
	ctx.HijackSetNoResponse(true)
	if rejected, noResponse := flags(stream); !rejected || !noResponse {
		t.Fatalf("Hijack then HijackSetNoResponse(true): rejected=%v noResponse=%v", rejected, noResponse)
	}
	ctx.HijackSetNoResponse(false)
	if _, noResponse := flags(stream); noResponse {
		t.Fatal("HijackSetNoResponse(false) after the rejection did not reach the stream")
	}
	ctx.reset()
	ctx.protocolStream = stream
	ctx.HijackSetNoResponse(true)
	if _, noResponse := flags(stream); noResponse {
		t.Fatal("HijackSetNoResponse without a hijack notified the stream")
	}

	stream = &testProtocolStream{Context: context.Background()}
	ctx.reset()
	ctx.protocolStream = stream
	if err := ctx.TryHijack(func(net.Conn) {}); !errors.Is(err, ErrHijackNotSupported) {
		t.Fatalf("TryHijack() error = %v", err)
	}
	ctx.HijackSetNoResponse(true)
	if rejected, _ := flags(stream); rejected {
		t.Fatal("TryHijack notified the stream")
	}
}

// BodyStreamPanicError is the error Response.Write reports for a panicking
// body stream.
func TestProtocolBodyStreamPanicError(t *testing.T) {
	ctx := newTestProtocolServerContext(&Server{}, nil)
	err := ctx.BodyStreamPanicError("boom")
	if _, ok := errors.AsType[*ErrBodyStreamWritePanic](err); !ok {
		t.Fatalf("BodyStreamPanicError() = %T", err)
	}
	var resp Response
	resp.SetBodyStream(panicReader{}, -1)
	var buf bytes.Buffer
	writeErr := resp.Write(bufio.NewWriter(&buf))
	if writeErr == nil || writeErr.Error() != err.Error() {
		t.Fatalf("Response.Write() error = %v, BodyStreamPanicError() = %v", writeErr, err)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("boom") }

func TestProtocolHeaderTooLargeError(t *testing.T) {
	ctx := newTestProtocolServerContext(&Server{}, nil)
	cause := errors.New("header list exceeds the limit")
	err := ctx.HeaderTooLargeError(cause)
	var small *ErrSmallBuffer
	if !errors.As(err, &small) || err.Error() != cause.Error() {
		t.Fatalf("HeaderTooLargeError() = %T %q", err, err)
	}
	var reqCtx RequestCtx
	defaultErrorHandler(&reqCtx, err)
	if reqCtx.Response.StatusCode() != StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("default ErrorHandler status = %d, want 431", reqCtx.Response.StatusCode())
	}
}
