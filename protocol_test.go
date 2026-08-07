package fasthttp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type protocolHandlerFunc func(*ProtocolServerContext, net.Conn) error

func (f protocolHandlerFunc) ServeConn(ctx *ProtocolServerContext, c net.Conn) error {
	return f(ctx, c)
}

type deadlineRecordingConn struct {
	net.Conn

	mu                   sync.Mutex
	nonzeroReadDeadlines int
}

func (c *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.mu.Lock()
		c.nonzeroReadDeadlines++
		c.mu.Unlock()
	}
	return c.Conn.SetReadDeadline(deadline)
}

func (c *deadlineRecordingConn) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

func (c *deadlineRecordingConn) readDeadlineCount() int {
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
}

func (s *testProtocolStream) RejectHijack() {
	s.mu.Lock()
	s.hijackRejected = true
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
			name: "second registration",
			registrations: []ProtocolRegistration{
				{ALPN: []string{"example"}, Handler: validHandler},
				{ALPN: []string{"other"}, Handler: validHandler},
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
	if got := s.protocol.alpn[0]; got != "example" {
		t.Fatalf("registered ALPN = %q, want %q", got, "example")
	}
	if got := string(s.protocol.cleartextPreface); got != "PROTOCOL" {
		t.Fatalf("registered preface = %q, want %q", got, "PROTOCOL")
	}
}

func TestServerRegisterProtocolPreservesAndOrdersTLSConfig(t *testing.T) {
	original := &tls.Config{NextProtos: []string{"custom", "http/1.1"}} //nolint:gosec
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
	original := &tls.Config{ //nolint:gosec
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
	if server.protocol != nil {
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
	server := &Server{protocol: &registeredProtocol{
		cleartextPreface: []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"),
	}}
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(clientConn, "PRI * HTTP/2.0\r\n\r\nSX\r\n\r\n")
		writeDone <- err
	}()
	protocol, prefix, _, err := server.detectCleartextProtocol(counted)
	if err != nil {
		t.Fatalf("detectCleartextProtocol() error: %v", err)
	}
	if protocol != nil || len(prefix) != len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n") {
		t.Fatalf("detection = protocol:%v prefix:%q", protocol, prefix)
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
	if protocolContext.active.Load() != 0 {
		t.Fatalf("active requests = %d, want 0", protocolContext.active.Load())
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
	serverConn := &deadlineRecordingConn{Conn: pipeServerConn}

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

	_, _, _, err := server.detectCleartextProtocol(conn) //nolint:dogsled
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
	server := &Server{}
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
}

func TestRequestCtxProtocolOperations(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	stream := &testProtocolStream{Context: base}
	ctx := &RequestCtx{
		s:              fakeServer,
		protocolStream: stream,
	}
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

	cancel()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want context.Canceled", ctx.Err())
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
