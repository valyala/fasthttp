package fasthttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

type protocolHandlerFunc func(*ProtocolServerContext, net.Conn) error

func (f protocolHandlerFunc) ServeConn(ctx *ProtocolServerContext, c net.Conn) error {
	return f(ctx, c)
}

type testProtocolStream struct {
	context.Context

	mu                  sync.Mutex
	informationalStatus int
	pushTarget          string
	accepted            bool
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
			name: "alpn and preface",
			registrations: []ProtocolRegistration{{
				ALPN:             []string{"example"},
				CleartextPreface: []byte("PROTOCOL"),
				Handler:          validHandler,
			}},
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
			got := make([]byte, len(preface))
			if _, err := io.ReadFull(c, got); err != nil {
				return err
			}
			if !bytes.Equal(got, []byte(preface)) {
				return errors.New("protocol preface wasn't replayed")
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

func TestServerCleartextProtocolFallsBackToHTTP1(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetBodyString("ok")
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
