package fasthttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoTimeoutClosableBodyAllocations(t *testing.T) {
	client := &HostClient{
		Addr: "example.com:80",
		Dial: func(string) (net.Conn, error) {
			return &fakeClientConn{
				ch: make(chan struct{}, 1),
				s:  []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"),
			}, nil
		},
	}
	defer client.CloseIdleConnections()

	var req Request
	var resp Response
	defer req.Reset()
	defer resp.Reset()
	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodPost)

	reader := strings.NewReader("body")
	body := io.NopCloser(reader)
	allocs := func(timeout time.Duration) float64 {
		return testing.AllocsPerRun(100, func() {
			reader.Reset("body")
			req.SetBodyStream(body, 4)
			var err error
			if timeout == 0 {
				err = client.Do(&req, &resp)
			} else {
				err = client.DoTimeout(&req, &resp, timeout)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	withoutTimeout := allocs(0)
	withTimeout := allocs(time.Second)
	if withTimeout > withoutTimeout {
		t.Fatalf("timeout adds %.0f allocations per request (without: %.0f, with: %.0f)",
			withTimeout-withoutTimeout, withoutTimeout, withTimeout)
	}
}

type contextRoundTripperFunc func(context.Context, *HostClient, *Request, *Response) (bool, error)

func (f contextRoundTripperFunc) RoundTrip(
	hc *HostClient, req *Request, resp *Response,
) (bool, error) {
	return f(context.Background(), hc, req, resp)
}

func (f contextRoundTripperFunc) RoundTripContext(
	ctx context.Context, hc *HostClient, req *Request, resp *Response,
) (bool, error) {
	return f(ctx, hc, req, resp)
}

type clientContextDoer interface {
	DoContext(context.Context, *Request, *Response) error
}

type dualRoundTripper struct {
	roundTripCalls        atomic.Int32
	roundTripContextCalls atomic.Int32
}

func (rt *dualRoundTripper) RoundTrip(*HostClient, *Request, *Response) (bool, error) {
	rt.roundTripCalls.Add(1)
	return false, nil
}

func (rt *dualRoundTripper) RoundTripContext(
	context.Context, *HostClient, *Request, *Response,
) (bool, error) {
	rt.roundTripContextCalls.Add(1)
	return false, nil
}

func TestDoKeepsLegacyRoundTripperDispatch(t *testing.T) {
	t.Parallel()

	rt := &dualRoundTripper{}
	hc := &HostClient{Addr: "example.com:80", Transport: rt}
	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response

	if err := hc.Do(&req, &resp); err != nil {
		t.Fatalf("unexpected Do error: %v", err)
	}
	if got := rt.roundTripCalls.Load(); got != 1 {
		t.Fatalf("RoundTrip calls: got %d, want 1", got)
	}
	if got := rt.roundTripContextCalls.Load(); got != 0 {
		t.Fatalf("ordinary Do unexpectedly called RoundTripContext %d times", got)
	}

	if err := hc.DoContext(context.Background(), &req, &resp); err != nil {
		t.Fatalf("unexpected DoContext error: %v", err)
	}
	if got := rt.roundTripContextCalls.Load(); got != 1 {
		t.Fatalf("DoContext calls: got %d, want 1", got)
	}
}

func TestDoContextPropagatesContextAndRestoresRequest(t *testing.T) {
	t.Parallel()

	type contextKey string
	const key contextKey = "key"

	for _, tc := range []struct {
		name string
		new  func(RoundTripper) clientContextDoer
	}{
		{
			name: "Client",
			new: func(rt RoundTripper) clientContextDoer {
				return &Client{Transport: rt}
			},
		},
		{
			name: "HostClient",
			new: func(rt RoundTripper) clientContextDoer {
				return &HostClient{Addr: "example.com:80", Transport: rt}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			capturedContext := make(chan context.Context, 1)
			capturedTimeout := make(chan time.Duration, 1)
			rt := contextRoundTripperFunc(func(
				ctx context.Context, _ *HostClient, req *Request, _ *Response,
			) (bool, error) {
				capturedContext <- ctx
				capturedTimeout <- req.timeout
				return false, nil
			})

			ctx, cancel := context.WithTimeout(
				context.WithValue(context.Background(), key, "current"),
				testTimeout(5*time.Second),
			)
			defer cancel()

			var req Request
			req.SetRequestURI("http://example.com/")
			req.SetTimeout(testTimeout(500 * time.Millisecond))
			originalTimeout := req.timeout

			var resp Response
			if err := tc.new(rt).DoContext(ctx, &req, &resp); err != nil {
				t.Fatalf("unexpected DoContext error: %v", err)
			}

			gotContext := <-capturedContext
			if gotContext != ctx {
				t.Fatal("RoundTripper did not receive the request context")
			}
			if value := gotContext.Value(key); value != "current" {
				t.Fatalf("unexpected context value %v", value)
			}
			gotTimeout := <-capturedTimeout
			if gotTimeout <= 0 || gotTimeout > originalTimeout {
				t.Fatalf("request timeout was not preserved as the earlier limit: got %s, original %s", gotTimeout, originalTimeout)
			}
			if req.timeout != originalTimeout {
				t.Fatalf("DoContext did not restore the request timeout: got %s, want %s", req.timeout, originalTimeout)
			}
		})
	}
}

func TestDoContextUsesEarlierContextDeadline(t *testing.T) {
	t.Parallel()

	var capturedTimeout time.Duration
	hc := &HostClient{
		Addr: "example.com:80",
		Transport: contextRoundTripperFunc(func(
			_ context.Context, _ *HostClient, req *Request, _ *Response,
		) (bool, error) {
			capturedTimeout = req.timeout
			return false, nil
		}),
	}

	ctxTimeout := testTimeout(500 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()

	var req Request
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(testTimeout(5 * time.Second))
	originalTimeout := req.timeout

	var resp Response
	if err := hc.DoContext(ctx, &req, &resp); err != nil {
		t.Fatalf("unexpected DoContext error: %v", err)
	}
	if capturedTimeout <= 0 || capturedTimeout > ctxTimeout {
		t.Fatalf("context deadline was not used as the earlier limit: got %s, context limit %s", capturedTimeout, ctxTimeout)
	}
	if req.timeout != originalTimeout {
		t.Fatalf("DoContext did not restore the request timeout: got %s, want %s", req.timeout, originalTimeout)
	}
}

func TestDoContextRejectsCanceledAndNilContexts(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	hc := &HostClient{
		Addr: "example.com:80",
		Transport: contextRoundTripperFunc(func(
			_ context.Context, _ *HostClient, _ *Request, _ *Response,
		) (bool, error) {
			called.Store(true)
			return false, nil
		}),
	}
	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hc.DoContext(ctx, &req, &resp); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected canceled-context error: %v", err)
	}
	if called.Load() {
		t.Fatal("transport was called for an already canceled context")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("DoContext did not panic for a nil context")
		}
	}()
	var nilContext context.Context
	_ = hc.DoContext(nilContext, &req, &resp)
}

func TestDoContextCancelsConnectionWait(t *testing.T) {
	t.Parallel()

	hc := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: testTimeout(5 * time.Second),
	}
	hc.connsCount = 1

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()

	deadline := time.Now().Add(testTimeout(time.Second))
	for {
		hc.connsLock.Lock()
		queued := hc.connsWait != nil && hc.connsWait.len() > 0
		hc.connsLock.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter the connection wait queue")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected connection-wait cancellation error: %v", err)
	}
}

func TestDoContextCanceledWaiterReleasesRequestState(t *testing.T) {
	t.Parallel()

	hc := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: testTimeout(5 * time.Second),
	}
	hc.connsCount = 1

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()

	waitForConnectionQueue(t, hc)
	cancel()
	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected connection-wait cancellation error: %v", err)
	}

	hc.connsLock.Lock()
	w := hc.connsWait.peekFront()
	if w == nil {
		hc.connsLock.Unlock()
		t.Fatal("canceled waiter was unexpectedly removed from the queue")
	}
	w.mu.Lock()
	if w.ctx != nil || !w.requestDeadline.IsZero() {
		w.mu.Unlock()
		hc.connsLock.Unlock()
		t.Fatal("canceled waiter retained request state")
	}
	w.mu.Unlock()
	hc.connsLock.Unlock()
}

func TestDoContextCancelsWaiterDial(t *testing.T) {
	t.Parallel()

	dialStarted := make(chan struct{})
	hc := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: testTimeout(5 * time.Second),
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	hc.connsCount = 1

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()

	deadline := time.Now().Add(testTimeout(time.Second))
	for {
		hc.connsLock.Lock()
		queued := hc.connsWait != nil && hc.connsWait.len() > 0
		hc.connsLock.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter the connection wait queue")
		}
		time.Sleep(time.Millisecond)
	}

	// Simulate the active connection closing. The waiting request now owns the
	// connection slot and must dial using its own context.
	hc.decConnsCount()
	<-dialStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected waiter-dial cancellation error: %v", err)
	}
	waitForConnectionCount(t, hc, 0)
}

func TestDoContextWaiterDialUsesRemainingRequestTimeout(t *testing.T) {
	t.Parallel()

	capturedTimeout := make(chan time.Duration, 1)
	hc := &HostClient{
		Addr:                      "example.com:80",
		MaxConns:                  1,
		MaxConnWaitTimeout:        testTimeout(5 * time.Second),
		MaxIdemponentCallAttempts: 1,
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("dial context has no deadline")
			}
			capturedTimeout <- time.Until(deadline)
			return nil, errors.New("stop dial")
		},
	}
	hc.connsCount = 1

	requestTimeout := testTimeout(500 * time.Millisecond)
	waitDuration := testTimeout(50 * time.Millisecond)
	var req Request
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(requestTimeout)
	result := make(chan error, 1)
	go func() {
		result <- hc.Do(&req, nil)
	}()

	waitForConnectionQueue(t, hc)
	time.Sleep(waitDuration)
	hc.decConnsCount()

	if err := waitContextResult(t, result); err == nil {
		t.Fatal("expected waiter dial error")
	}
	gotTimeout := <-capturedTimeout
	if gotTimeout <= 0 || gotTimeout >= requestTimeout-waitDuration/2 {
		t.Fatalf("waiter dial did not receive the remaining request timeout: got %s, original %s", gotTimeout, requestTimeout)
	}
}

func TestRequestTimeoutStillInterruptsLegacyWaiterDial(t *testing.T) {
	t.Parallel()

	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	hc := &HostClient{
		Addr:               "example.com:80",
		MaxConns:           1,
		MaxConnWaitTimeout: testTimeout(5 * time.Second),
		Dial: func(string) (net.Conn, error) {
			close(dialStarted)
			<-releaseDial
			return nil, errors.New("stop dial")
		},
	}
	hc.connsCount = 1

	var req Request
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(testTimeout(100 * time.Millisecond))
	result := make(chan error, 1)
	go func() {
		result <- hc.Do(&req, nil)
	}()

	waitForConnectionQueue(t, hc)
	hc.decConnsCount()
	<-dialStarted
	if err := waitContextResult(t, result); !errors.Is(err, ErrTimeout) {
		t.Fatalf("legacy waiter dial did not preserve request timeout: %v", err)
	}
	close(releaseDial)
	waitForConnectionCount(t, hc, 0)
}

func TestDoContextCancelsDialContext(t *testing.T) {
	t.Parallel()

	dialStarted := make(chan struct{})
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	<-dialStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected dial cancellation error: %v", err)
	}
}

func TestDialContextHonorsRequestTimeout(t *testing.T) {
	t.Parallel()

	capturedTimeout := make(chan time.Duration, 1)
	hc := &HostClient{
		Addr:                      "example.com:80",
		MaxIdemponentCallAttempts: 1,
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("dial context has no deadline")
			}
			capturedTimeout <- time.Until(deadline)
			return nil, errors.New("stop dial")
		},
	}

	requestTimeout := testTimeout(500 * time.Millisecond)
	var req Request
	req.SetRequestURI("http://example.com/")
	req.SetTimeout(requestTimeout)
	if err := hc.Do(&req, nil); err == nil {
		t.Fatal("expected dial error")
	}

	gotTimeout := <-capturedTimeout
	if gotTimeout <= 0 || gotTimeout > requestTimeout {
		t.Fatalf("DialContext did not receive request timeout: got %s, want at most %s", gotTimeout, requestTimeout)
	}
}

func TestRequestTimeoutCoversAllDialAddresses(t *testing.T) {
	t.Parallel()

	var dialCalls atomic.Int32
	hc := &HostClient{
		Addr:                      "first.example.com:80,second.example.com:80",
		MaxIdemponentCallAttempts: 1,
		DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
			dialCalls.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	var req Request
	req.SetRequestURI("http://first.example.com/")
	req.SetTimeout(testTimeout(100 * time.Millisecond))
	if err := hc.Do(&req, nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unexpected multi-address timeout error: %v", err)
	}
	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("request timeout was restarted for another address: got %d dial attempts, want 1", got)
	}
}

type signalWriteConn struct {
	net.Conn

	started chan struct{}
	once    sync.Once
}

func (c *signalWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestDoContextCancelsBlockedWrite(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	writeStarted := make(chan struct{})
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			return &signalWriteConn{Conn: clientConn, started: writeStarted}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	<-writeStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected write cancellation error: %v", err)
	}
}

type cancelableBlockingRequestBody struct {
	readStarted chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
	closeCalls  atomic.Int32
}

func (b *cancelableBlockingRequestBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *cancelableBlockingRequestBody) Close() error {
	b.closeCalls.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestDoContextCancelsBlockedRequestBodyRead(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	body := &cancelableBlockingRequestBody{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodPost)
	req.SetBodyStream(body, 1)
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	<-body.readStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected request-body cancellation error: %v", err)
	}
	if got := body.closeCalls.Load(); got != 1 {
		t.Fatalf("request body close calls: got %d, want 1", got)
	}
	if req.IsBodyStream() {
		t.Fatal("canceled request retained its body stream")
	}
}

func TestRequestTimeoutCancelsBlockedRequestBodyRead(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	body := &cancelableBlockingRequestBody{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout(5*time.Second))
	defer cancel()
	var req Request
	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodPost)
	req.SetBodyStream(body, 1)
	req.SetTimeout(testTimeout(100 * time.Millisecond))
	if err := hc.DoContext(ctx, &req, nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unexpected request-body timeout error: %v", err)
	}
	if got := body.closeCalls.Load(); got != 1 {
		t.Fatalf("request body close calls: got %d, want 1", got)
	}
	if req.IsBodyStream() {
		t.Fatal("timed-out request retained its body stream")
	}
}

type blockingWriteDeadlineConn struct {
	net.Conn

	setStarted chan struct{}
	closed     chan struct{}
	setOnce    sync.Once
	closeOnce  sync.Once
}

func (c *blockingWriteDeadlineConn) SetWriteDeadline(time.Time) error {
	c.setOnce.Do(func() { close(c.setStarted) })
	<-c.closed
	return net.ErrClosed
}

func (c *blockingWriteDeadlineConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.Conn.Close()
	})
	return nil
}

func TestDoContextCancellationBeforeBodyWriteClosesBodyOnce(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	conn := &blockingWriteDeadlineConn{
		Conn:       clientConn,
		setStarted: make(chan struct{}),
		closed:     make(chan struct{}),
	}
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			return conn, nil
		},
	}
	body := &cancelableBlockingRequestBody{
		readStarted: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodPost)
	req.SetBodyStream(body, 1)
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	<-conn.setStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected pre-write cancellation error: %v", err)
	}
	if got := body.closeCalls.Load(); got != 1 {
		t.Fatalf("request body close calls: got %d, want 1", got)
	}
	if req.IsBodyStream() {
		t.Fatal("pre-write cancellation retained its body stream")
	}
}

func TestDoContextCancelsTLSHandshake(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	handshakeStarted := make(chan struct{})
	hc := &HostClient{
		Addr:  "example.com:443",
		IsTLS: true,
		DialContext: func(context.Context, string) (net.Conn, error) {
			return &signalWriteConn{Conn: clientConn, started: handshakeStarted}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("https://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	<-handshakeStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected TLS handshake cancellation error: %v", err)
	}
}

func TestRequestTimeoutInterruptsTLSHandshake(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	hc := &HostClient{
		Addr:         "example.com:443",
		IsTLS:        true,
		WriteTimeout: testTimeout(5 * time.Second),
		DialContext: func(context.Context, string) (net.Conn, error) {
			return clientConn, nil
		},
	}

	var req Request
	req.SetRequestURI("https://example.com/")
	req.SetTimeout(testTimeout(100 * time.Millisecond))
	if err := hc.Do(&req, nil); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unexpected TLS request-timeout error: %v", err)
	}
}

type signalReadConn struct {
	net.Conn

	started chan struct{}
	once    sync.Once
}

func (c *signalReadConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Read(p)
}

func TestDoContextCancelsBlockedRead(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	readStarted := make(chan struct{})
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			return &signalReadConn{Conn: clientConn, started: readStarted}, nil
		},
	}

	serverRead := make(chan error, 1)
	go func() {
		serverRead <- readRequestHeader(serverConn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	result := make(chan error, 1)
	go func() {
		result <- hc.DoContext(ctx, &req, nil)
	}()
	if err := waitContextResult(t, serverRead); err != nil {
		t.Fatalf("server could not read request: %v", err)
	}
	<-readStarted
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected read cancellation error: %v", err)
	}
}

func TestDoContextCancellationDoesNotCloseReleasedConnection(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	serverDone := make(chan error, 1)
	go func() {
		for range 2 {
			if err := readRequestHeader(serverConn); err != nil {
				serverDone <- err
				return
			}
			if _, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()

	var dialCount atomic.Int32
	hc := &HostClient{
		Addr: "example.com:80",
		DialContext: func(context.Context, string) (net.Conn, error) {
			if dialCount.Add(1) != 1 {
				return nil, errors.New("unexpected second dial")
			}
			return clientConn, nil
		},
	}
	t.Cleanup(hc.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/first")
	var resp Response
	if err := hc.DoContext(ctx, &req, &resp); err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	cancel()

	req.Reset()
	req.SetRequestURI("http://example.com/second")
	if err := hc.Do(&req, &resp); err != nil {
		t.Fatalf("released connection was closed by late cancellation: %v", err)
	}
	if err := waitContextResult(t, serverDone); err != nil {
		t.Fatalf("server failed: %v", err)
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("connection was not reused: got %d dials", got)
	}
}

type cancelOnHeaderConn struct {
	net.Conn

	cancel     context.CancelFunc
	firstClose chan struct{}
	readOnce   sync.Once
	closeCalls atomic.Int32
}

func (c *cancelOnHeaderConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.readOnce.Do(func() {
			c.cancel()
			<-c.firstClose
		})
	}
	return n, err
}

func (c *cancelOnHeaderConn) Close() error {
	if c.closeCalls.Add(1) == 1 {
		close(c.firstClose)
		return nil
	}
	return c.Conn.Close()
}

func TestDoContextCleansStreamingResponseCanceledDuringHeaderTransfer(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	wrappedConn := &cancelOnHeaderConn{
		Conn:       clientConn,
		cancel:     cancel,
		firstClose: make(chan struct{}),
	}
	hc := &HostClient{
		Addr:               "example.com:80",
		StreamResponseBody: true,
		DialContext: func(context.Context, string) (net.Conn, error) {
			return wrappedConn, nil
		},
	}

	serverDone := make(chan error, 1)
	go func() {
		if err := readRequestHeader(serverConn); err != nil {
			serverDone <- err
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nabc")
		serverDone <- err
	}()

	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response
	if err := hc.DoContext(ctx, &req, &resp); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected header-transfer cancellation error: %v", err)
	}
	if resp.BodyStream() != nil {
		t.Fatal("canceled header transfer retained a response body stream")
	}
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("canceled header transfer retained a connection slot: %d", got)
	}
	if err := waitContextResult(t, serverDone); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("unexpected server error: %v", err)
	}
}

func TestDoContextCancelsStreamingResponseRead(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	serverDone := make(chan error, 1)
	go func() {
		if err := readRequestHeader(serverConn); err != nil {
			serverDone <- err
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nabc")
		serverDone <- err
	}()

	hc := &HostClient{
		Addr:               "example.com:80",
		StreamResponseBody: true,
		DialContext: func(context.Context, string) (net.Conn, error) {
			return clientConn, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response
	if err := hc.DoContext(ctx, &req, &resp); err != nil {
		t.Fatalf("request failed before streaming: %v", err)
	}
	if err := waitContextResult(t, serverDone); err != nil {
		t.Fatalf("server failed: %v", err)
	}

	buf := make([]byte, 3)
	if n, err := io.ReadFull(resp.BodyStream(), buf); n != len(buf) || err != nil || string(buf) != "abc" {
		t.Fatalf("unexpected initial stream read: n=%d err=%v body=%q", n, err, buf[:n])
	}
	readResult := make(chan error, 1)
	go func() {
		_, err := resp.BodyStream().Read(make([]byte, 1))
		readResult <- err
	}()
	cancel()

	if err := waitContextResult(t, readResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected stream cancellation error: %v", err)
	}
	if err := resp.CloseBodyStream(); err != nil {
		t.Fatalf("unexpected body close error: %v", err)
	}
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("canceled streaming connection was retained: %d", got)
	}
}

type blockingContextResolver struct {
	started chan struct{}
}

func (r *blockingContextResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	close(r.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTCPDialerContextCancelsDNSResolution(t *testing.T) {
	t.Parallel()

	resolver := &blockingContextResolver{started: make(chan struct{})}
	dialer := &TCPDialer{Resolver: resolver}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := dialer.dialContext(ctx, "example.com:80", false, testTimeout(5*time.Second))
		result <- err
	}()
	<-resolver.started
	cancel()

	if err := waitContextResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected DNS cancellation error: %v", err)
	}
}

func readRequestHeader(conn net.Conn) error {
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" {
			return nil
		}
	}
}

func waitContextResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(testTimeout(2 * time.Second)):
		t.Fatal("timed out waiting for context-aware operation")
		return nil
	}
}

func waitForConnectionQueue(t *testing.T, hc *HostClient) {
	t.Helper()
	deadline := time.Now().Add(testTimeout(time.Second))
	for {
		hc.connsLock.Lock()
		queued := hc.connsWait != nil && hc.connsWait.len() > 0
		hc.connsLock.Unlock()
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter the connection wait queue")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForConnectionCount(t *testing.T, hc *HostClient, want int) {
	t.Helper()
	deadline := time.Now().Add(testTimeout(time.Second))
	for {
		if got := hc.ConnsCount(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection count did not reach %d: got %d", want, hc.ConnsCount())
		}
		time.Sleep(time.Millisecond)
	}
}
