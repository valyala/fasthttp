package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

type testServer struct {
	addr  string
	reads chan struct{}
}

// startServer serves h on a TCP listener: in-memory pipes cannot interrupt a
// blocked read by moving its deadline, which Conn relies on.
func startServer(t testing.TB, opts *AcceptOptions, h func(*Conn)) *testServer {
	t.Helper()
	return startServerKeep(t, opts, h, false)
}

func startServerKeep(t testing.TB, opts *AcceptOptions, h func(*Conn), keep bool) *testServer {
	t.Helper()
	return startServerWith(t, opts, h, func(s *fasthttp.Server) { s.KeepHijackedConns = keep })
}

func startServerWith(t testing.TB, opts *AcceptOptions, h func(*Conn), configure func(*fasthttp.Server)) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			_ = Accept(ctx, opts, h)
		},
	}
	configure(s)
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { ln.Close() })
	return &testServer{addr: ln.Addr().String(), reads: make(chan struct{}, 64)}
}

// signal sends on ch without blocking; a nil ch is ignored.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// signalConn reports every Read call on the client connection, so tests can
// act once a Read is under way instead of sleeping.
type signalConn struct {
	net.Conn

	reads chan struct{}
}

func (c *signalConn) Read(p []byte) (int, error) {
	signal(c.reads)
	return c.Conn.Read(p)
}

func (ts *testServer) dialOpts(opts *DialOptions) *DialOptions {
	if opts == nil {
		opts = &DialOptions{}
	}
	opts.Dial = func(string) (net.Conn, error) {
		nc, err := net.Dial("tcp", ts.addr)
		if err != nil {
			return nil, err
		}
		return &signalConn{Conn: nc, reads: ts.reads}, nil
	}
	return opts
}

// awaitRead returns once a Read has started since the last drainReads.
func (ts *testServer) awaitRead(t *testing.T) {
	t.Helper()
	select {
	case <-ts.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("no read started")
	}
}

func (ts *testServer) dial(t *testing.T, opts *DialOptions) *Conn {
	t.Helper()
	c, err := Dial(t.Context(), "ws://example.com/ws", ts.dialOpts(opts))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	ts.drainReads()
	return c
}

// drainReads forgets the reads of the handshake, so that awaitRead reports
// the next one.
func (ts *testServer) drainReads() {
	for {
		select {
		case <-ts.reads:
		default:
			return
		}
	}
}

func echo(c *Conn) {
	ctx := c.Context()
	for {
		typ, p, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, typ, p); err != nil {
			return
		}
	}
}

func readErr(t *testing.T, c *Conn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if err == nil {
		t.Fatal("expected read error")
	}
	return err
}

func TestEcho(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx := t.Context()
	for _, typ := range []MessageType{MessageText, MessageBinary} {
		for _, n := range []int{0, 1, 125, 126, 127, 65535, 65536, 200000} {
			msg := bytes.Repeat([]byte("x"), n)
			if err := c.Write(ctx, typ, msg); err != nil {
				t.Fatalf("write %d: %v", n, err)
			}
			gotTyp, got, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("read %d: %v", n, err)
			}
			if gotTyp != typ || !bytes.Equal(got, msg) {
				t.Fatalf("size %d: got type %d, %d bytes", n, gotTyp, len(got))
			}
		}
	}
}

func TestWriterReader(t *testing.T) {
	ts := startServer(t, nil, func(c *Conn) {
		ctx := c.Context()
		for {
			typ, r, err := c.Reader(ctx)
			if err != nil {
				return
			}
			w, err := c.Writer(ctx, typ)
			if err != nil {
				return
			}
			buf := make([]byte, 1000)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					break
				}
			}
			if err := w.Close(); err != nil {
				return
			}
		}
	})
	c := ts.dial(t, nil)
	ctx := t.Context()
	msg := bytes.Repeat([]byte("0123456789"), 3000)
	w, err := c.Writer(ctx, MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{msg[:10], msg[10:5000], msg[5000:]} {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	typ, r, err := c.Reader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if typ != MessageBinary || !bytes.Equal(got, msg) {
		t.Fatalf("got type %d, %d bytes", typ, len(got))
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read after EOF: %d, %v", n, err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, errWriterClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

func TestReaderDiscard(t *testing.T) {
	ts := startServer(t, nil, func(c *Conn) {
		ctx := c.Context()
		_, r, err := c.Reader(ctx)
		if err != nil {
			return
		}
		if _, err := r.Read(make([]byte, 10)); err != nil {
			return
		}
		_, p, err := c.Read(ctx)
		if err != nil {
			return
		}
		_ = c.Write(ctx, MessageText, p)
	})
	c := ts.dial(t, nil)
	ctx := t.Context()
	w, err := c.Writer(ctx, MessageText)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("a"), 20000)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, MessageText, []byte("second")); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.Read(ctx)
	if err != nil || string(got) != "second" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPing(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err := c.Ping(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ping without reader: %v", err)
	}
	c.CloseRead(t.Context())
	for range 3 {
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("ping: %v", err)
		}
	}
}

func TestCloseByClient(t *testing.T) {
	errCh := make(chan error, 1)
	ts := startServer(t, nil, func(c *Conn) {
		_, _, err := c.Read(c.Context())
		errCh <- err
	})
	c := ts.dial(t, nil)
	if err := c.Close(t.Context(), StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(t.Context(), StatusNormalClosure, ""); err != nil {
		t.Fatalf("second close: %v", err)
	}
	err := <-errCh
	if ce, ok := errors.AsType[CloseError](err); !ok || ce.Code != StatusNormalClosure || ce.Reason != "bye" {
		t.Fatalf("server read: %v", err)
	}
	<-c.Context().Done()
	if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
	if err := c.Write(t.Context(), MessageText, nil); CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("write after close: %v", err)
	}
}

func TestCloseByServer(t *testing.T) {
	ts := startServer(t, nil, func(c *Conn) {
		_ = c.Close(c.Context(), StatusGoingAway, "shutting down")
	})
	c := ts.dial(t, nil)
	err := readErr(t, c)
	if CloseStatus(err) != StatusGoingAway || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("read: %v", err)
	}
}

func TestCloseWhileReading(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.Read(c.Context())
		errCh <- err
	}()
	ts.awaitRead(t)
	if err := c.Close(t.Context(), StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-errCh; CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("read: %v", err)
	}
}

func TestCloseTimeout(t *testing.T) {
	took := make(chan time.Duration, 1)
	ts := startServer(t, &AcceptOptions{ReadTimeout: 50 * time.Millisecond}, func(c *Conn) {
		start := time.Now()
		_ = c.Close(c.Context(), StatusNormalClosure, "")
		took <- time.Since(start)
	})
	c := ts.dial(t, nil)
	// The client does not answer; the server must give up after ReadTimeout.
	if d := <-took; d > time.Second {
		t.Fatalf("server close took %v", d)
	}
	if err := readErr(t, c); CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("client read: %v", err)
	}
}

func TestCloseRead(t *testing.T) {
	ts := startServer(t, nil, func(c *Conn) {
		ctx := c.Context()
		c.CloseRead(ctx)
		for i := range 3 {
			if err := c.Write(ctx, MessageText, []byte{byte('a' + i)}); err != nil {
				return
			}
		}
		<-ctx.Done()
	})
	c := ts.dial(t, nil)
	ctx := t.Context()
	for i := range 3 {
		_, p, err := c.Read(ctx)
		if err != nil || string(p) != string(rune('a'+i)) {
			t.Fatalf("read %d: %q, %v", i, p, err)
		}
	}
	if err := c.Write(ctx, MessageText, []byte("unexpected")); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, c); CloseStatus(err) != StatusPolicyViolation {
		t.Fatalf("read: %v", err)
	}
}

func TestMaxMessageSize(t *testing.T) {
	ts := startServer(t, &AcceptOptions{MaxMessageSize: 10}, echo)
	c := ts.dial(t, nil)
	ctx := t.Context()
	if err := c.Write(ctx, MessageBinary, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(ctx); err != nil || len(p) != 10 {
		t.Fatalf("read: %d, %v", len(p), err)
	}
	w, err := c.Writer(ctx, MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, MessageBinary, make([]byte, 11)); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, c); CloseStatus(err) != StatusMessageTooBig {
		t.Fatalf("read: %v", err)
	}
}

func TestReadTimeout(t *testing.T) {
	errCh := make(chan error, 1)
	ts := startServer(t, &AcceptOptions{ReadTimeout: 50 * time.Millisecond}, func(c *Conn) {
		_, _, err := c.Read(c.Context())
		errCh <- err
	})
	c := ts.dial(t, nil)
	if err := <-errCh; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server read: %v", err)
	}
	if err := readErr(t, c); CloseStatus(err) != StatusAbnormalClosure {
		t.Fatalf("client read: %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.Read(ctx)
		errCh <- err
	}()
	ts.awaitRead(t)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("read: %v", err)
	}
	<-c.Context().Done()
	if _, _, err := c.Read(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after close: %v", err)
	}
}

func TestCloseNowUnblocksRead(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.Read(c.Context())
		errCh <- err
	}()
	ts.awaitRead(t)
	if err := c.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read: %v", err)
	}
}

func TestHandlerReturnClosesConn(t *testing.T) {
	ts := startServer(t, nil, func(*Conn) {})
	c := ts.dial(t, nil)
	if err := readErr(t, c); CloseStatus(err) != StatusAbnormalClosure {
		t.Fatalf("read: %v", err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx := t.Context()
	const writers, perWriter = 4, 50
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			for range perWriter {
				msg := bytes.Repeat([]byte{byte('a' + i)}, 100+i*1000)
				if err := c.Write(ctx, MessageBinary, msg); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		})
	}
	for range writers * perWriter {
		_, p, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) == 0 || bytes.Count(p, p[:1]) != len(p) {
			t.Fatalf("corrupted message of %d bytes", len(p))
		}
	}
	wg.Wait()
}

func TestWriterBlocksWrite(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx := t.Context()
	w, err := c.Writer(ctx, MessageText)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = c.Write(ctx, MessageText, []byte("second"))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, want := range []string{"first", "second"} {
		_, p, err := c.Read(ctx)
		if err != nil || string(p) != want {
			t.Fatalf("got %q, %v; want %q", p, err, want)
		}
	}
}

func TestReadWriteAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops entries at random under the race detector")
	}
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx := c.Context()
	msg := bytes.Repeat([]byte("m"), 300)
	roundTrip := func() {
		if err := c.Write(ctx, MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		if _, p, err := c.Read(ctx); err != nil || len(p) != len(msg) {
			t.Fatalf("read: %d, %v", len(p), err)
		}
	}
	for range 10 {
		roundTrip()
	}
	if n := testing.AllocsPerRun(1000, roundTrip); n > 0 {
		t.Fatalf("%v allocations per round trip", n)
	}
}

func TestSubprotocol(t *testing.T) {
	got := make(chan string, 1)
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"b", "a"}}, func(c *Conn) {
		got <- c.Subprotocol()
	})
	c := ts.dial(t, &DialOptions{Subprotocols: []string{"a", "b"}})
	if c.Subprotocol() != "b" || <-got != "b" {
		t.Fatalf("client got %q", c.Subprotocol())
	}
	c = ts.dial(t, &DialOptions{Subprotocols: []string{"c"}})
	if c.Subprotocol() != "" || <-got != "" {
		t.Fatalf("client got %q", c.Subprotocol())
	}
}

func TestDialHeaders(t *testing.T) {
	seen := make(chan string, 1)
	ts2 := startServer(t, &AcceptOptions{CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
		seen <- string(ctx.Request.Header.Peek("X-Token")) + "|" + string(ctx.Host()) +
			"|" + string(ctx.Request.Header.Peek(fasthttp.HeaderOrigin))
		return true
	}}, echo)
	var h fasthttp.RequestHeader
	h.Set("X-Token", "secret")
	h.Set(fasthttp.HeaderOrigin, "https://other.example")
	h.SetHost("override.example")
	var resp fasthttp.Response
	c := ts2.dial(t, &DialOptions{Header: &h, Response: &resp})
	if s := <-seen; s != "secret|override.example|https://other.example" {
		t.Fatalf("server saw %q", s)
	}
	if resp.StatusCode() != fasthttp.StatusSwitchingProtocols || !resp.Header.ConnectionUpgrade() {
		t.Fatalf("response: %d", resp.StatusCode())
	}
	_ = c.CloseNow()
}

func TestDialRejected(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	t.Cleanup(func() { ln.Close() })
	s := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/forbidden":
			ctx.Error("no", fasthttp.StatusForbidden)
		case "/badaccept":
			ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
			ctx.Response.Header.Set(fasthttp.HeaderUpgrade, "websocket")
			ctx.Response.Header.Set(fasthttp.HeaderConnection, "Upgrade")
			ctx.Response.Header.Set(fasthttp.HeaderSecWebSocketAccept, "AAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			ctx.Hijack(func(net.Conn) {})
		case "/protocol":
			ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
			ctx.Response.Header.Set(fasthttp.HeaderUpgrade, "websocket")
			ctx.Response.Header.Set(fasthttp.HeaderConnection, "Upgrade")
			key := ctx.Request.Header.Peek(fasthttp.HeaderSecWebSocketKey)
			ctx.Response.Header.SetBytesV(fasthttp.HeaderSecWebSocketAccept, appendAcceptKey(nil, key))
			ctx.Response.Header.Set(fasthttp.HeaderSecWebSocketProtocol, "nope")
			ctx.Hijack(func(net.Conn) {})
		}
	}}
	go func() { _ = s.Serve(ln) }()
	dial := func(string) (net.Conn, error) { return ln.Dial() }
	var resp fasthttp.Response
	for _, tc := range []struct {
		url, want string
		status    int
	}{
		{"ws://h/forbidden", "status 403", fasthttp.StatusForbidden},
		{"ws://h/badaccept", "Sec-WebSocket-Accept", fasthttp.StatusSwitchingProtocols},
		{"ws://h/protocol", "subprotocol", fasthttp.StatusSwitchingProtocols},
		{"ftp://h/", "scheme", 0},
		{"ws:///nohost", "host", 0},
	} {
		resp.Reset()
		_, err := Dial(t.Context(), tc.url, &DialOptions{Dial: dial, Response: &resp})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.url, err, tc.want)
		}
		if tc.status != 0 && resp.StatusCode() != tc.status {
			t.Fatalf("%s: status %d", tc.url, resp.StatusCode())
		}
	}
}

func TestBadArguments(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx := t.Context()
	if err := c.Write(ctx, 3, nil); !errors.Is(err, errBadMessageType) {
		t.Fatalf("write: %v", err)
	}
	if _, err := c.Writer(ctx, 0); !errors.Is(err, errBadMessageType) {
		t.Fatalf("writer: %v", err)
	}
	if err := c.Close(ctx, StatusNoStatusReceived, ""); !errors.Is(err, errBadCloseCode) {
		t.Fatalf("close 1005: %v", err)
	}
	if err := c.Close(ctx, StatusNormalClosure, strings.Repeat("r", 124)); !errors.Is(err, errBadCloseCode) {
		t.Fatalf("close long reason: %v", err)
	}
	if err := c.Close(ctx, 4000, strings.Repeat("r", 123)); err != nil {
		t.Fatalf("close 4000: %v", err)
	}
}

// hijackLike stands in for fasthttp's hijacked conn: UnsafeConn exposes the
// socket and Close is a no-op unless keep is set, as with KeepHijackedConns.
type hijackLike struct {
	net.Conn

	keep bool
}

func (h hijackLike) Close() error {
	if h.keep {
		return h.Conn.Close()
	}
	return nil
}

func (h hijackLike) UnsafeConn() net.Conn { return h.Conn }

func tcpPair(t *testing.T) (srv, cli net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			close(ch)
			return
		}
		ch <- c
	}()
	cli, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv = <-ch
	if srv == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { srv.Close(); cli.Close() })
	return srv, cli
}

// pipePair returns a synchronous pipe, where deadlines interrupt a blocked
// write, and a channel that reports every Write on the server side.
func pipePair(t *testing.T) (srv, cli net.Conn, writes chan struct{}) {
	t.Helper()
	srv, cli = net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	writes = make(chan struct{}, 64)
	return &writeSignalConn{Conn: srv, writes: writes}, cli, writes
}

type writeSignalConn struct {
	net.Conn

	writes chan struct{}
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	signal(c.writes)
	return c.Conn.Write(p)
}

// readHead reads header lines up to and including the blank line that ends them.
func readHead(br *bufio.Reader) (string, error) {
	var head strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return head.String(), err
		}
		head.WriteString(line)
		if strings.TrimSpace(line) == "" {
			return head.String(), nil
		}
	}
}

// rawServer answers one handshake with the response respond builds from the
// computed Sec-WebSocket-Accept value, and reports the request it received.
func rawServer(t *testing.T, respond func(accept string) string) (addr string, request <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	req := make(chan string, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		head, err := readHead(bufio.NewReader(nc))
		if err != nil {
			return
		}
		var key string
		for line := range strings.Lines(head) {
			if name, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Key") {
				key = strings.TrimSpace(v)
			}
		}
		req <- head
		_, _ = io.WriteString(nc, respond(string(appendAcceptKey(nil, []byte(key)))))
	}()
	return ln.Addr().String(), req
}

// rawHandshake sends req to the server and returns the response head.
func rawHandshake(t *testing.T, addr, req string) string {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatal(err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	head, err := readHead(bufio.NewReader(nc))
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// slowCloseConn takes a while to close, like a TLS connection sending its
// close_notify alert to a peer that does not read.
type slowCloseConn struct {
	net.Conn
}

func (c *slowCloseConn) Close() error {
	time.Sleep(300 * time.Millisecond)
	return c.Conn.Close()
}

// fieldCount returns the last value of the header field and how often it
// occurs, matching the name ASCII case-insensitively.
func fieldCount(all iter.Seq2[[]byte, []byte], name string) ([]byte, int) {
	var value []byte
	n := 0
	for k, v := range all {
		if equalFoldASCII(k, name) {
			value, n = trimHTTPSpace(v), n+1
		}
	}
	return value, n
}
