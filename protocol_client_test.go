package fasthttp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testProtocolTransport struct {
	roundTripCalled         atomic.Bool
	protocolRoundTripCalled atomic.Bool
	closeIdleCalled         atomic.Bool
}

func (t *testProtocolTransport) RoundTrip(
	_ *HostClient,
	_ *Request,
	_ *Response,
) (bool, error) {
	t.roundTripCalled.Store(true)
	return false, nil
}

func (t *testProtocolTransport) RoundTripWithContext(
	_ *ProtocolClientContext,
	_ *HostClient,
	_ *Request,
	_ *Response,
) (bool, error) {
	t.protocolRoundTripCalled.Store(true)
	return false, nil
}

func (t *testProtocolTransport) CloseIdleConnections(_ *HostClient) {
	t.closeIdleCalled.Store(true)
}

func TestHostClientProtocolRoundTripper(t *testing.T) {
	transport := &testProtocolTransport{}
	hc := &HostClient{
		Addr: "example.com:80",
	}
	if err := hc.RegisterProtocolTransport(transport); err != nil {
		t.Fatalf("RegisterProtocolTransport() error: %v", err)
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !transport.protocolRoundTripCalled.Load() {
		t.Fatal("Do() didn't call ProtocolRoundTripper")
	}
	if transport.roundTripCalled.Load() {
		t.Fatal("Do() called the legacy RoundTripper path")
	}

	hc.CloseIdleConnections()
	if !transport.closeIdleCalled.Load() {
		t.Fatal("CloseIdleConnections() didn't notify the protocol transport")
	}
}

func TestSetMaxConnsWakesQueuedSlotWaiters(t *testing.T) {
	hc := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: 5 * time.Second,
	}
	if err := hc.reserveProtocolConn(0); err != nil {
		t.Fatalf("reserving the only slot: %v", err)
	}

	result := make(chan error, 1)
	go func() { result <- hc.reserveProtocolConn(0) }()
	for i := 0; ; i++ {
		hc.connsLock.Lock()
		queued := hc.connsWait != nil && hc.connsWait.len() > 0
		hc.connsLock.Unlock()
		if queued {
			break
		}
		if i > 1000 {
			t.Fatal("slot waiter never queued")
		}
		time.Sleep(time.Millisecond)
	}

	hc.SetMaxConns(2)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queued reservation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetMaxConns() didn't wake the queued slot waiter")
	}
	if got := hc.ConnsCount(); got != 2 {
		t.Fatalf("ConnsCount() = %d, want 2", got)
	}
	hc.releaseProtocolConnSlot()
	hc.releaseProtocolConnSlot()
}

type timeoutRecordingTransport struct {
	seen time.Duration
}

func (t *timeoutRecordingTransport) RoundTrip(_ *HostClient, req *Request, _ *Response) (bool, error) {
	t.seen = req.timeout
	return false, nil
}

func TestHostClientProtocolTransportKeepsCustomHTTP1Fallback(t *testing.T) {
	roundTripper := &timeoutRecordingTransport{}
	hc := &HostClient{
		Addr:      "example.com:80",
		Transport: roundTripper,
	}
	if err := hc.RegisterProtocolTransport(&testProtocolTransport{}); err != nil {
		t.Fatalf("RegisterProtocolTransport() error: %v", err)
	}

	var req Request
	var resp Response
	req.timeout = 5 * time.Second
	ctx := &ProtocolClientContext{hostClient: hc, deadline: time.Now().Add(time.Second)}
	if _, err := ctx.RoundTripHTTP1(&req, &resp); err != nil {
		t.Fatalf("RoundTripHTTP1() error: %v", err)
	}
	if roundTripper.seen <= 0 || roundTripper.seen > time.Second {
		t.Fatalf("fallback timeout = %v, want the remaining ~1s budget", roundTripper.seen)
	}
	if req.timeout != 5*time.Second {
		t.Fatalf("request timeout = %v after fallback, want restored 5s", req.timeout)
	}
}

func TestProtocolClientContextAcquireConnALPN(t *testing.T) {
	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("generating certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	serverError := make(chan error, 1)
	go func() {
		serverTLS := tls.Server(serverConn, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"h2", "http/1.1"},
		})
		if err := serverTLS.Handshake(); err != nil {
			serverError <- err
			return
		}
		var one [1]byte
		_, err := serverTLS.Read(one[:])
		if err != nil && err != io.EOF {
			serverError <- err
			return
		}
		serverError <- nil
		_ = serverTLS.Close()
	}()

	hc := &HostClient{
		Addr:  "localhost:443",
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	ctx := ProtocolClientContext{
		hostClient: hc,
		deadline:   time.Now().Add(time.Second),
	}
	conn, err := ctx.AcquireConn([]string{"h2", "http/1.1"})
	if err != nil {
		t.Fatalf("AcquireConn() error: %v", err)
	}
	if got := conn.NegotiatedProtocol(); got != "h2" {
		t.Fatalf("NegotiatedProtocol() = %q, want %q", got, "h2")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := <-serverError; err != nil {
		t.Fatalf("server handshake error: %v", err)
	}
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("ConnsCount() = %d, want 0", got)
	}
}

func TestProtocolClientConnRoundTripHTTP1(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		var req Request
		if err := req.Read(reader); err != nil {
			serverError <- err
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		serverError <- err
	}()

	hc := &HostClient{
		Addr: "example.com:80",
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	ctx := ProtocolClientContext{hostClient: hc}
	conn, err := ctx.AcquireConn(nil)
	if err != nil {
		t.Fatalf("AcquireConn() error: %v", err)
	}

	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response
	retry, err := conn.RoundTripHTTP1(&req, &resp)
	if err != nil {
		t.Fatalf("RoundTripHTTP1() error: %v (retry=%v)", err, retry)
	}
	if got := string(resp.Body()); got != "ok" {
		t.Fatalf("response body = %q, want %q", got, "ok")
	}
	if err := <-serverError; err != nil {
		t.Fatalf("server error: %v", err)
	}

	hc.CloseIdleConnections()
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("ConnsCount() after CloseIdleConnections = %d, want 0", got)
	}
}

func TestHostClientOpenStreamUnsupported(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response

	stream, err := hc.OpenStream(&req, &resp)
	if stream != nil {
		t.Fatal("OpenStream() returned a stream for the default transport")
	}
	if err != ErrProtocolNotSupported {
		t.Fatalf("OpenStream() error = %v, want ErrProtocolNotSupported", err)
	}
}

func TestReleaseProtocolConnSlotServesHTTP1Waiters(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	server := &Server{Handler: func(*RequestCtx) {}}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = server.Shutdown()
		<-serveDone
		_ = ln.Close()
	})

	hc := &HostClient{
		Addr:               ln.Addr().String(),
		MaxConns:           1,
		MaxConnWaitTimeout: 2 * time.Second,
	}
	// Take the only slot the way a protocol connection would.
	if err := hc.reserveProtocolConn(0); err != nil {
		t.Fatalf("reserveProtocolConn() error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		req := AcquireRequest()
		resp := AcquireResponse()
		defer ReleaseRequest(req)
		defer ReleaseResponse(resp)
		req.SetRequestURI("http://" + ln.Addr().String() + "/")
		done <- hc.Do(req, resp)
	}()

	// Wait until the HTTP/1 request is queued as a connection waiter.
	deadline := time.Now().Add(time.Second)
	for {
		hc.connsLock.Lock()
		queued := hc.connsWait != nil && hc.connsWait.len() > 0
		hc.connsLock.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP/1 request never queued as a connection waiter")
		}
		time.Sleep(time.Millisecond)
	}

	// Freeing the protocol slot must serve the queued HTTP/1 waiter promptly;
	// both connection kinds share connsCount.
	released := time.Now()
	hc.releaseProtocolConnSlot()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued request error: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("queued HTTP/1 request wasn't served after the protocol slot was freed")
	}
	if elapsed := time.Since(released); elapsed > 500*time.Millisecond {
		t.Fatalf("queued request served after %v; want promptly after slot release", elapsed)
	}
	hc.CloseIdleConnections()
}

func TestProtocolClientContextMaxConns(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	ctx := hc.acquireProtocolClientContext(&Request{})
	defer releaseProtocolClientContext(ctx)
	if got := ctx.MaxConns(); got != DefaultMaxConnsPerHost {
		t.Fatalf("MaxConns() = %d, want the default %d", got, DefaultMaxConnsPerHost)
	}
	hc.SetMaxConns(3)
	if got := ctx.MaxConns(); got != 3 {
		t.Fatalf("MaxConns() = %d, want 3", got)
	}
}

func TestProtocolClientContextAcquireConnDialTimeoutAfterSlotWait(t *testing.T) {
	var mu sync.Mutex
	var dialTimeouts []time.Duration
	hc := &HostClient{
		Addr:               "127.0.0.1:1",
		MaxConns:           1,
		MaxConnWaitTimeout: time.Second,
		DialTimeout: func(_ string, timeout time.Duration) (net.Conn, error) {
			mu.Lock()
			dialTimeouts = append(dialTimeouts, timeout)
			mu.Unlock()
			return nil, errors.New("test: no dial")
		},
	}
	if !hc.tryReserveConnSlot() {
		t.Fatal("couldn't take the only slot")
	}
	req := &Request{}
	req.SetTimeout(300 * time.Millisecond)
	ctx := hc.acquireProtocolClientContext(req)
	defer releaseProtocolClientContext(ctx)
	time.AfterFunc(150*time.Millisecond, hc.releaseProtocolConnSlot)
	if _, err := ctx.AcquireConn(nil); err == nil {
		t.Fatal("AcquireConn() succeeded without a dialer")
	}
	mu.Lock()
	got := slices.Clone(dialTimeouts)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("dial attempts = %d, want 1", len(got))
	}
	if got[0] <= 0 || got[0] > 200*time.Millisecond {
		t.Fatalf("dial timeout = %v after a 150ms slot wait on a 300ms deadline", got[0])
	}
	if n := hc.ConnsCount(); n != 0 {
		t.Fatalf("ConnsCount() = %d after the failed dial, want 0", n)
	}
}

func TestProtocolClientContextPrepareRequestMatchesWrite(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	ctx := hc.acquireProtocolClientContext(&Request{})
	defer releaseProtocolClientContext(ctx)
	for _, tc := range []struct {
		name string
		make func() *Request
	}{
		{"post args", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/form")
			req.Header.SetMethod(MethodPost)
			req.PostArgs().Set("a", "1")
			req.PostArgs().Set("b", "x y")
			return req
		}},
		{"multipart form", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/upload")
			req.Header.SetMethod(MethodPost)
			req.multipartForm = &multipart.Form{Value: map[string][]string{"field": {"value"}}}
			req.multipartFormBoundary = "testboundary"
			return req
		}},
		{"host from the uri", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/path?q=1")
			req.Header.SetHost("stale.example")
			req.URI()
			return req
		}},
		{"host header kept", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/path")
			req.Header.SetHost("virtual.example")
			req.UseHostHeader = true
			req.URI()
			return req
		}},
		{"userinfo", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://user:pass@example.com/secret")
			return req
		}},
		{"userinfo with authorization set", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://user:pass@example.com/secret")
			req.Header.Set(HeaderAuthorization, "Bearer token")
			return req
		}},
		{"stale content-length", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/put")
			req.Header.SetMethod(MethodPut)
			req.SetBodyString("hello")
			req.Header.SetContentLength(2)
			return req
		}},
		{"empty post", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/empty")
			req.Header.SetMethod(MethodPost)
			return req
		}},
		{"no default content-type", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/raw")
			req.Header.SetMethod(MethodPost)
			req.Header.SetNoDefaultContentType(true)
			req.SetBodyString("raw")
			return req
		}},
		{"raw stale content-length", func() *Request {
			req := AcquireRequest()
			req.Header.DisableSpecialHeader()
			req.SetRequestURI("http://example.com/raw")
			req.Header.SetMethod(MethodPut)
			req.Header.Set(HeaderHost, "example.com")
			req.SetBodyString("hello")
			req.Header.Set(HeaderContentLength, "2")
			return req
		}},
		{"raw body without content-length", func() *Request {
			req := AcquireRequest()
			req.Header.DisableSpecialHeader()
			req.SetRequestURI("http://example.com/raw")
			req.Header.SetMethod(MethodPost)
			req.Header.Set(HeaderHost, "example.com")
			req.SetBodyString("hello")
			return req
		}},
		{"raw sized body stream with stale content-length", func() *Request {
			req := AcquireRequest()
			req.Header.DisableSpecialHeader()
			req.SetRequestURI("http://example.com/raw")
			req.Header.SetMethod(MethodPut)
			req.Header.Set(HeaderHost, "example.com")
			req.SetBodyStream(strings.NewReader("hello"), len("hello"))
			req.Header.Set(HeaderContentLength, "2")
			return req
		}},
		{"raw sized body stream without content-length", func() *Request {
			req := AcquireRequest()
			req.Header.DisableSpecialHeader()
			req.SetRequestURI("http://example.com/raw")
			req.Header.SetMethod(MethodPut)
			req.Header.Set(HeaderHost, "example.com")
			req.SetBodyStream(strings.NewReader("hello"), len("hello"))
			return req
		}},
		{"limited reader stream without size", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/stream")
			req.Header.SetMethod(MethodPost)
			req.SetBodyStream(io.LimitReader(strings.NewReader("streamed"), 4), -1)
			return req
		}},
		{"sized body stream", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/stream")
			req.Header.SetMethod(MethodPost)
			req.SetBodyStream(strings.NewReader("streamed"), len("streamed"))
			return req
		}},
		{"get", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/")
			return req
		}},
		// Request.Write stores the default Content-Type itself for these two.
		{"buffered body with trailers", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/t")
			req.Header.SetMethod(MethodPost)
			_ = req.Header.AddTrailer("X-T")
			req.Header.Set("X-T", "v")
			req.SetBodyString("abc")
			return req
		}},
		{"sized body stream with trailers", func() *Request {
			req := AcquireRequest()
			req.SetRequestURI("http://example.com/t")
			req.Header.SetMethod(MethodPost)
			_ = req.Header.AddTrailer("X-T")
			req.Header.Set("X-T", "v")
			req.SetBodyStream(strings.NewReader("abc"), 3)
			return req
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written := tc.make()
			var buf bytes.Buffer
			bw := bufio.NewWriter(&buf)
			if err := written.Write(bw); err != nil {
				t.Fatalf("Write() error: %v", err)
			}
			if err := bw.Flush(); err != nil {
				t.Fatal(err)
			}
			// Write settles the header on the request itself; the body it
			// sent is read back from the wire.
			wire := AcquireRequest()
			if err := wire.Read(bufio.NewReader(&buf)); err != nil {
				t.Fatalf("reading the written request: %v", err)
			}

			got := tc.make()
			body, defaultContentType, err := ctx.PrepareRequest(got)
			if err != nil {
				t.Fatalf("PrepareRequest() error: %v", err)
			}
			wireContentType := string(got.Header.ContentType())
			if wireContentType == "" {
				wireContentType = string(defaultContentType)
			} else if defaultContentType != nil {
				t.Errorf("default Content-Type %q reported next to an explicit %q", defaultContentType, wireContentType)
			}
			// The header keeps the caller's Content-Type, as Request.Write
			// leaves it, except where Write stores the default itself.
			storesDefault := strings.Contains(tc.name, "with trailers")
			if storesDefault && len(got.Header.ContentType()) != 0 {
				t.Errorf("PrepareRequest stored a Content-Type: %q", got.Header.ContentType())
			}
			if !storesDefault && !bytes.Equal(got.Header.ContentType(), written.Header.ContentType()) {
				t.Errorf("header Content-Type = %q, Request.Write leaves %q", got.Header.ContentType(), written.Header.ContentType())
			}
			for _, field := range []struct {
				name      string
				got, want string
			}{
				{"request URI", string(got.Header.RequestURI()), string(written.Header.RequestURI())},
				{"host", string(got.Header.Host()), string(written.Header.Host())},
				{"authorization", string(got.Header.Peek(HeaderAuthorization)), string(written.Header.Peek(HeaderAuthorization))},
				{"wire content-type", wireContentType, string(wire.Header.ContentType())},
				{"body", string(body), string(wire.Body())},
			} {
				if field.name == "body" && got.IsBodyStream() {
					continue
				}
				if field.got != field.want {
					t.Errorf("%s = %q, Request.Write sends %q", field.name, field.got, field.want)
				}
			}
			if !storesDefault && got.Header.ContentLength() != written.Header.ContentLength() {
				t.Errorf("content length = %d, Request.Write sends %d", got.Header.ContentLength(), written.Header.ContentLength())
			}
			if got.IsBodyStream() && body != nil {
				t.Errorf("PrepareRequest() returned a body for a body stream")
			}
		})
	}
}

// A HostClient the Client drops releases its protocol transport state.
func TestClientCleanerClosesDroppedHostClients(t *testing.T) {
	transport := &testProtocolTransport{}
	client := &Client{
		ConfigureClient: func(hc *HostClient) error {
			return hc.RegisterProtocolTransport(transport)
		},
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")
	if err := client.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !transport.protocolRoundTripCalled.Load() {
		t.Fatal("the protocol transport did not serve the request")
	}
	client.mLock.Lock()
	hosts := len(client.m)
	client.mLock.Unlock()
	if hosts != 1 {
		t.Fatalf("HostClients = %d, want 1", hosts)
	}
	if !client.cleanHostClients(client.m) {
		t.Fatal("the unused HostClient was not dropped")
	}
	if !transport.closeIdleCalled.Load() {
		t.Fatal("the dropped HostClient did not close its protocol transport state")
	}
}

// PrepareRequest reports the wire-only default Content-Type instead of
// storing it, as RequestHeader.AppendBytes writes it.
func TestProtocolClientContextPrepareRequestDefaultContentType(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	ctx := hc.acquireProtocolClientContext(&Request{})
	defer releaseProtocolClientContext(ctx)
	for _, tc := range []struct {
		name string
		make func(*Request)
		want string
	}{
		{"buffered body", func(req *Request) { req.SetBodyString("abc") }, "application/octet-stream"},
		{"sized stream", func(req *Request) { req.SetBodyStream(strings.NewReader("abc"), 3) }, "application/octet-stream"},
		{"unsized stream", func(req *Request) { req.SetBodyStream(strings.NewReader("abc"), -1) }, ""},
		{"empty post", func(*Request) {}, ""},
		{"explicit", func(req *Request) { req.SetBodyString("abc"); req.Header.SetContentType("text/plain") }, ""},
		{"no default", func(req *Request) { req.SetBodyString("abc"); req.Header.SetNoDefaultContentType(true) }, ""},
		{"raw", func(req *Request) {
			req.Header.DisableSpecialHeader()
			req.Header.Set(HeaderHost, "example.com")
			req.SetBodyString("abc")
		}, ""},
	} {
		req := AcquireRequest()
		req.SetRequestURI("http://example.com/")
		req.Header.SetMethod(MethodPost)
		tc.make(req)
		_, defaultContentType, err := ctx.PrepareRequest(req)
		if err != nil {
			t.Fatalf("%s: PrepareRequest() error: %v", tc.name, err)
		}
		if string(defaultContentType) != tc.want {
			t.Fatalf("%s: default Content-Type = %q, want %q", tc.name, defaultContentType, tc.want)
		}
		if tc.name != "explicit" && len(req.Header.ContentType()) != 0 {
			t.Fatalf("%s: PrepareRequest stored Content-Type %q", tc.name, req.Header.ContentType())
		}
		ReleaseRequest(req)
	}
}

// The body readers apply Request.Write's and Response.Write's fixed-size
// rules to a protocol's transfer.
func TestProtocolContextBodyReaders(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	clientCtx := hc.acquireProtocolClientContext(&Request{})
	defer releaseProtocolClientContext(clientCtx)
	serverCtx := newTestProtocolServerContext(&Server{}, nil)
	file, err := os.CreateTemp(t.TempDir(), "body")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("0123456789"); err != nil {
		t.Fatal(err)
	}
	readers := func(stream func() io.Reader, size int) map[string]io.Reader {
		var req Request
		req.SetBodyStream(stream(), size)
		var resp Response
		resp.SetBodyStream(stream(), size)
		return map[string]io.Reader{
			"request":  clientCtx.RequestBodyReader(&req),
			"response": serverCtx.ResponseBodyReader(&resp),
		}
	}
	text := func(s string) func() io.Reader {
		return func() io.Reader { return strings.NewReader(s) }
	}
	for name, r := range readers(text("abc"), -1) {
		if _, ok := r.(*strings.Reader); !ok {
			t.Fatalf("%s: unsized stream wrapped as %T", name, r)
		}
	}
	for name, r := range readers(text("abc"), 3) {
		if got, err := io.ReadAll(r); err != nil || string(got) != "abc" {
			t.Fatalf("%s exact: %q, %v", name, got, err)
		}
	}
	for name, r := range readers(text("ab"), 3) {
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "instead of 3 bytes") {
			t.Fatalf("%s short: err = %v", name, err)
		}
	}
	for name, r := range readers(text("abcd"), 3) {
		if _, err := io.ReadAll(r); !errors.Is(err, errBodyStreamTooLong) {
			t.Fatalf("%s long: err = %v", name, err)
		}
	}
	grown := func() io.Reader {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		return file
	}
	for name, r := range readers(grown, 4) {
		grown()
		if got, err := io.ReadAll(r); err != nil || string(got) != "0123" {
			t.Fatalf("%s grown file: %q, %v", name, got, err)
		}
	}
	failing := func() io.Reader { return &lastBytesErrReader{data: []byte("abc"), err: io.ErrClosedPipe} }
	for name, r := range readers(failing, 3) {
		got, err := io.ReadAll(r)
		if string(got) != "abc" || !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("%s error with the last bytes: %q, %v", name, got, err)
		}
	}
}

// ParseTrailer records declarations as the HTTP/1 parsers do: header-section
// values stay, names accumulate, a bad name is reported and the rest recorded.
func TestProtocolContextParseTrailer(t *testing.T) {
	serverCtx := newTestProtocolServerContext(&Server{}, nil)

	var req Request
	req.Header.Set("X-Check", "initial")
	var resp Response
	resp.Header.Set("X-Check", "initial")
	parse := map[string]struct {
		h     interface{ Peek(string) []byte }
		keys  func() [][]byte
		apply func([]byte) error
	}{
		"request": {&req.Header, req.Header.PeekTrailerKeys, func(v []byte) error { return serverCtx.ParseTrailer(&req.Header, v) }},
		"response": {&resp.Header, resp.Header.PeekTrailerKeys, func(v []byte) error {
			return (&ProtocolClientConn{}).ParseTrailer(&resp.Header, v)
		}},
	}
	for name, p := range parse {
		if err := p.apply([]byte("X-Check, X-A")); err != nil {
			t.Fatalf("%s: ParseTrailer() error: %v", name, err)
		}
		if err := p.apply([]byte("Content-Length, X-B")); !errors.Is(err, ErrBadTrailer) {
			t.Fatalf("%s: ParseTrailer(forbidden) error = %v", name, err)
		}
		if got := string(p.h.Peek("X-Check")); got != "initial" {
			t.Fatalf("%s: header-section value = %q after the declaration", name, got)
		}
		var keys []string
		for _, key := range p.keys() {
			keys = append(keys, string(key))
		}
		if strings.Join(keys, ",") != "X-Check,X-A,X-B" {
			t.Fatalf("%s: trailer keys = %v", name, keys)
		}
	}

	// The HTTP/1 request parser gives the same picture.
	var parsed Request
	err := parsed.Header.Read(bufio.NewReader(strings.NewReader(
		"POST / HTTP/1.1\r\nHost: example.com\r\nX-Check: initial\r\nTrailer: X-Check, X-A\r\nTrailer: X-B\r\n\r\n")))
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if got := string(parsed.Header.Peek("X-Check")); got != "initial" {
		t.Fatalf("HTTP/1 header-section value = %q", got)
	}
	if got := len(parsed.Header.PeekTrailerKeys()); got != 3 {
		t.Fatalf("HTTP/1 trailer keys = %d, want 3", got)
	}
}
