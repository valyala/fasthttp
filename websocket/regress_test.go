package websocket

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"iter"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

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

// A Read that passed its preliminary checks before the connection was closed
// must not re-arm the deadline and block: on a hijacked conn Close is a no-op.
func TestReadAfterCloseDoesNotBlock(t *testing.T) {
	srv, _ := tcpPair(t)
	nc := hijackLike{Conn: srv}
	c := newConn(nc, nc, false, "", connConfig{})
	c.closeWith(net.ErrClosed)
	if _, err := c.rdWatch.begin(context.Background(), time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("begin after close: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read blocked after close")
	}
}

// A context cancelled between two calls that share it must still fail the
// next call, although the cached AfterFunc has already fired.
func TestCancelBetweenCalls(t *testing.T) {
	srv, _ := tcpPair(t)
	w := ctxWatch{nc: srv}
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := w.begin(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	w.end()
	cancel()
	if _, err := w.begin(ctx, time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin with cancelled ctx: %v", err)
	}
}

// Close must return within the ReadTimeout bound even when another goroutine
// keeps reading data from a peer that never answers the Close frame.
func TestCloseWithChattyPeer(t *testing.T) {
	closed := make(chan time.Duration, 1)
	ts := startServer(t, &AcceptOptions{ReadTimeout: 200 * time.Millisecond}, func(c *Conn) {
		go func() {
			for {
				if _, _, err := c.Read(c.Context()); err != nil {
					return
				}
			}
		}()
		time.Sleep(50 * time.Millisecond)
		start := time.Now()
		_ = c.Close(c.Context(), StatusNormalClosure, "")
		closed <- time.Since(start)
	})
	nc, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	req := "GET /ws HTTP/1.1\r\nHost: example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatal(err)
	}
	if _, err := readHead(bufio.NewReader(nc)); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, err := nc.Write(frame(opText, true, []byte("hi"))); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	select {
	case d := <-closed:
		if d > 2*time.Second {
			t.Fatalf("close took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return")
	}
}

// A reader left half-way when another goroutine closes the connection must
// report an error, not a clean end of message.
func TestReaderTruncatedByClose(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	if _, err := cli.Write(frame(opText, false, []byte("part1"))); err != nil {
		t.Fatal(err)
	}
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if n, err := r.Read(buf); err != nil || string(buf[:n]) != "part1" {
		t.Fatalf("first read: %q %v", buf[:n], err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx, StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(buf); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("read after close: %v", err)
	}
	// A reader invalidated by a later Read also fails.
	srv, cli = tcpPair(t)
	c = newConn(srv, srv, false, "", connConfig{})
	if _, err := cli.Write(frame(opText, false, []byte("a"))); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(frame(opContinuation, true, []byte("b"))); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(frame(opText, true, []byte("second"))); err != nil {
		t.Fatal(err)
	}
	_, r, err = c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(t.Context()); err != nil || string(p) != "second" {
		t.Fatalf("read: %q %v", p, err)
	}
	if _, err := r.Read(buf); !errors.Is(err, errReaderStale) {
		t.Fatalf("stale reader: %v", err)
	}
}

// Closing for any reason must not return the last Read's buffer to the pool
// while the caller may still hold it.
func TestReadBufferSurvivesClose(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	if _, err := cli.Write(frame(opText, true, []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	_, p, err := c.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseNow()
	if c.readBuf == nil || string(p) != "hello" {
		t.Fatalf("buffer released on close: %q", p)
	}
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

// Pong replies to a peer that stopped reading are bounded by WriteTimeout, so
// the reading goroutine cannot be stalled by pings.
func TestPongWriteBounded(t *testing.T) {
	srv, cli, _ := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{writeTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() { _, _ = cli.Write(frame(opPing, true, []byte("p"))) }()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(t.Context())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a write timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read stalled on the pong write")
	}
}

func TestClientRejectsMaskedFrame(t *testing.T) {
	pc := fasthttputil.NewPipeConns()
	c := newConn(pc.Conn1(), pc.Conn1(), true, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow(); pc.Conn2().Close() })
	if _, err := pc.Conn2().Write(frame(opText, true, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusProtocolError {
		t.Fatalf("read: %v", err)
	}
}

// A writer that fails releases the message lock; the connection is closed
// only when a fragment had already gone out or the transport failed.
func TestWriterFailure(t *testing.T) {
	t.Run("cancelled before the first flush", func(t *testing.T) {
		srv, _ := tcpPair(t)
		c := newConn(srv, srv, false, "", connConfig{})
		t.Cleanup(func() { _ = c.CloseNow() })
		ctx, cancel := context.WithCancel(t.Context())
		w, err := c.Writer(ctx, MessageBinary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("buffered")); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := w.Close(); !errors.Is(err, context.Canceled) {
			t.Fatalf("close: %v", err)
		}
		if err := c.Write(t.Context(), MessageText, []byte("next")); err != nil {
			t.Fatalf("write after an abandoned writer: %v", err)
		}
	})
	t.Run("connection closed", func(t *testing.T) {
		srv, _ := tcpPair(t)
		c := newConn(srv, srv, false, "", connConfig{})
		w, err := c.Writer(t.Context(), MessageBinary)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.CloseNow()
		if _, err := w.Write(make([]byte, writeChunkSize+1)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write on closed conn: %v", err)
		}
		if err := w.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("close after failure: %v", err)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		pc := fasthttputil.NewPipeConns()
		c := newConn(pc.Conn1(), pc.Conn1(), false, "", connConfig{})
		t.Cleanup(func() { _ = c.CloseNow() })
		w, err := c.Writer(t.Context(), MessageBinary)
		if err != nil {
			t.Fatal(err)
		}
		pc.Conn2().Close()
		if _, err := w.Write(make([]byte, 2*writeChunkSize)); err == nil {
			t.Fatal("expected a transport error")
		}
		if err := w.Close(); err == nil {
			t.Fatal("close after failure returned nil")
		}
		if !c.isClosed() {
			t.Fatal("connection still open after a failed flush")
		}
	})
}

// Close with an already cancelled context sends nothing and leaves the
// connection usable; a later Close still performs the handshake.
func TestCloseWithCancelledContext(t *testing.T) {
	ts := startServer(t, nil, echo)
	c := ts.dial(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Close(ctx, StatusNormalClosure, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("close: %v", err)
	}
	if err := c.Write(t.Context(), MessageText, []byte("still open")); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(t.Context()); err != nil || string(p) != "still open" {
		t.Fatalf("read: %q %v", p, err)
	}
	if err := c.Close(t.Context(), StatusNormalClosure, ""); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
	if err := c.Close(t.Context(), StatusNormalClosure, "\xff"); !errors.Is(err, errBadCloseCode) {
		t.Fatalf("invalid reason: %v", err)
	}
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

func TestDialResponseChecks(t *testing.T) {
	for _, tc := range []struct {
		name, response, want string
		subprotocols         []string
	}{
		{"missing upgrade", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "upgrade", nil},
		{"two accept values", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Accept: AAAAAAAAAAAAAAAAAAAAAAAAAAA=\r\n\r\n", "Sec-WebSocket-Accept", nil},
		{"extension", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Extensions: \r\n" + "Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n", "extension", nil},
		{"unoffered subprotocol", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: nope\r\n\r\n", "subprotocol", nil},
		{"second subprotocol", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: chat\r\nSec-WebSocket-Protocol: nope\r\n\r\n", "subprotocol", []string{"chat"}},
		{"empty subprotocol", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: \r\n\r\n", "subprotocol", []string{"chat"}},
		{"empty and chat subprotocol", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: \r\n" + "Sec-WebSocket-Protocol: chat\r\n\r\n", "subprotocol", []string{"chat"}},
		{"empty extension", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Extensions: \r\n\r\n", "extension", nil},
		{"unicode upgrade", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: web\u017focket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "upgrade", nil},
		{"http/1.0", "HTTP/1.0 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade, keep-alive\r\nSec-WebSocket-Accept: %s\r\n\r\n", "HTTP/1.1", nil},
		{"two connection values", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: WebSocket\r\nConnection: keep-alive\r\n" +
			"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: chat\r\n\r\n", "", []string{"chat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := rawServer(t, func(accept string) string { return strings.ReplaceAll(tc.response, "%s", accept) })
			c, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{Subprotocols: tc.subprotocols})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				if c.Subprotocol() != "chat" {
					t.Fatalf("subprotocol %q", c.Subprotocol())
				}
				_ = c.CloseNow()
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDialRequest(t *testing.T) {
	addr, request := rawServer(t, probeAccept)
	var resp fasthttp.Response
	resp.SetStatusCode(fasthttp.StatusTeapot)
	c, err := Dial(t.Context(), "ws://alice:secret@"+addr+"/a%2Fb//c?x=%2F", &DialOptions{Response: &resp})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseNow()
	head := <-request
	for _, want := range []string{"GET /a%2Fb//c?x=%2F HTTP/1.1\r\n", "Authorization: Basic YWxpY2U6c2VjcmV0\r\n"} {
		if !strings.Contains(head, want) {
			t.Fatalf("request lacks %q:\n%s", want, head)
		}
	}
	// Credentials are percent-decoded, a password alone counts, and an
	// explicit Authorization header wins over the URL.
	for _, tc := range []struct{ url, auth, want string }{
		{"ws://al%40ice:p%3Ass@", "", "Authorization: Basic YWxAaWNlOnA6c3M=\r\n"},
		{"ws://:secret@", "", "Authorization: Basic OnNlY3JldA==\r\n"},
		{"ws://:@", "", "Authorization: Basic Og==\r\n"},
		{"ws://alice:secret@", "Bearer tok", "Authorization: Bearer tok\r\n"},
	} {
		addr, request := rawServer(t, probeAccept)
		opts := &DialOptions{}
		if tc.auth != "" {
			opts.Header = &fasthttp.RequestHeader{}
			opts.Header.Set(fasthttp.HeaderAuthorization, tc.auth)
		}
		c, err := Dial(t.Context(), tc.url+addr+"/", opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		_ = c.CloseNow()
		head := <-request
		if !strings.Contains(head, tc.want) || strings.Count(head, "Authorization:") != 1 {
			t.Fatalf("%s: request:\n%s", tc.url, head)
		}
	}
	if _, err := Dial(t.Context(), "ws://"+addr+"/a#frag", nil); err == nil || !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("fragment: %v", err)
	}
	// An explicit empty Authorization still counts as one; handshake headers
	// are rejected; bad escapes fail before dialing; names must be distinct.
	refused := func(string) (net.Conn, error) { return nil, errors.New("must not dial") }
	var h fasthttp.RequestHeader
	h.Set(fasthttp.HeaderAuthorization, "")
	addr, request = rawServer(t, probeAccept)
	c, err = Dial(t.Context(), "ws://alice:secret@"+addr+"/", &DialOptions{Header: &h})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseNow()
	if head := <-request; strings.Contains(head, "Basic") {
		t.Fatalf("URL credentials overrode an explicit header:\n%s", head)
	}
	for _, reserved := range []string{fasthttp.HeaderConnection, fasthttp.HeaderUpgrade, fasthttp.HeaderSecWebSocketKey, fasthttp.HeaderContentLength} {
		var h fasthttp.RequestHeader
		h.Set(reserved, "5")
		if _, err := Dial(t.Context(), "ws://h/", &DialOptions{Header: &h, Dial: func(string) (net.Conn, error) {
			cli, srv := net.Pipe()
			srv.Close()
			return cli, nil
		}}); err == nil || !strings.Contains(err.Error(), "must not set") {
			t.Fatalf("%s: %v", reserved, err)
		}
	}
	if _, err := Dial(t.Context(), "ws://a%zz:x@h/", &DialOptions{Dial: refused}); err == nil || strings.Contains(err.Error(), "must not dial") {
		t.Fatalf("bad escape: %v", err)
	}
	// Caller headers are recognized whatever their name normalization.
	var raw fasthttp.RequestHeader
	raw.DisableNormalizing()
	raw.Set("connection", "keep-alive")
	if _, err := Dial(t.Context(), "ws://h/", &DialOptions{Header: &raw, Dial: refused}); err == nil || !strings.Contains(err.Error(), "must not set") {
		t.Fatalf("lowercase reserved header: %v", err)
	}
	raw.Reset()
	raw.DisableNormalizing()
	raw.Set("authorization", "Bearer tok")
	addr, request = rawServer(t, probeAccept)
	c, err = Dial(t.Context(), "ws://alice:secret@"+addr+"/", &DialOptions{Header: &raw})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseNow()
	if head := <-request; strings.Contains(head, "Basic") || !strings.Contains(head, "authorization: Bearer tok") {
		t.Fatalf("lowercase authorization not honored:\n%s", head)
	}
	if _, err := Dial(t.Context(), "ws://h/", &DialOptions{Dial: refused, Subprotocols: []string{"chat", "chat"}}); err == nil || strings.Contains(err.Error(), "must not dial") {
		t.Fatalf("duplicate subprotocols: %v", err)
	}
	// Response is reset even when the dial itself fails.
	if _, err := Dial(t.Context(), "ws://x/", &DialOptions{Response: &resp, Dial: func(string) (net.Conn, error) {
		return nil, errors.New("refused")
	}}); err == nil || resp.StatusCode() == fasthttp.StatusSwitchingProtocols {
		t.Fatalf("dial: %v, status %d", err, resp.StatusCode())
	}
	if _, err := Dial(t.Context(), "ws://x/", &DialOptions{Subprotocols: []string{"bad name"}}); err == nil {
		t.Fatal("subprotocol with a space accepted")
	}
}

func TestDialReadBufferSize(t *testing.T) {
	big := strings.Repeat("c", 5000)
	respond := func(accept string) string {
		return okHead + "Set-Cookie: s=" + big + "\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
	}
	addr, _ := rawServer(t, respond)
	if _, err := Dial(t.Context(), "ws://"+addr+"/", nil); err == nil {
		t.Fatal("5 KiB of response headers fit in the default buffer")
	}
	addr, _ = rawServer(t, respond)
	c, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{ReadBufferSize: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseNow()
}

func TestDialTLS(t *testing.T) {
	pair := testCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	serverName := make(chan string, 1)
	s := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		serverName <- ctx.TLSConnectionState().ServerName
		_ = Accept(ctx, nil, echo)
	}}
	go func() { _ = s.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{pair}})) }()
	dial := func(string) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) }
	c, err := Dial(t.Context(), "wss://example.com/ws", &DialOptions{Dial: dial, TLSConfig: insecureTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if name := <-serverName; name != "example.com" {
		t.Fatalf("server name %q", name)
	}
	if err := c.Write(t.Context(), MessageText, []byte("tls")); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(t.Context()); err != nil || string(p) != "tls" {
		t.Fatalf("read: %q %v", p, err)
	}
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

func TestAcceptRejectsRaw(t *testing.T) {
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"chat"}}, echo)
	const head = "Host: example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
	const key = "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for _, tc := range []struct{ name, req, status string }{
		{"post", "POST /ws HTTP/1.1\r\n" + head + key + "\r\n", "405"},
		{"no upgrade", "GET /ws HTTP/1.1\r\nHost: h\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "\r\n", "400"},
		{"no connection", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: keep-alive\r\nSec-WebSocket-Version: 13\r\n" + key + "\r\n", "400"},
		{"bad base64", "GET /ws HTTP/1.1\r\n" + head + "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ!!\r\n\r\n", "400"},
		{"origin mismatch", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: http://evil.com\r\n\r\n", "403"},
		{"http/1.0", "GET /ws HTTP/1.0\r\n" + head + key + "\r\n", "400"},
		{"long key", "GET /ws HTTP/1.1\r\n" + head + "Sec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAAAAAAAA\r\n\r\n", "400"},
		{"two keys", "GET /ws HTTP/1.1\r\n" + head + key + key + "\r\n", "400"},
		{"version list", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13, 8\r\n" + key + "\r\n", "426"},
		{"two versions", "GET /ws HTTP/1.1\r\n" + head + "Sec-WebSocket-Version: 8\r\n" + key + "\r\n", "426"},
		{"bad subprotocol", "GET /ws HTTP/1.1\r\n" + head + key + "Sec-WebSocket-Protocol: bad name, chat\r\n\r\n", "400"},
		{"two connection values", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: h2c\r\nUpgrade: WebSocket\r\nConnection: keep-alive\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "\r\n", "101"},
		{"unicode upgrade", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: web\u017focket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "\r\n", "400"},
		{"nbsp upgrade", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: \u00a0websocket\u00a0\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "\r\n", "400"},
		{"empty list items", "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: , websocket,\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "Sec-WebSocket-Protocol: , chat,\r\n\r\n", "101"},
		{"empty origin", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: \r\n\r\n", "403"},
		{"origin without scheme", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: example.com\r\n\r\n", "403"},
		{"origin null", "GET /ws HTTP/1.1\r\nHost: null\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" + key + "Origin: null\r\n\r\n", "403"},
		{"origin with path", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: http://example.com/\r\n\r\n", "403"},
		{"two origins", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: http://example.com\r\nOrigin: http://other.example\r\n\r\n", "403"},
		{"matching origin", "GET /ws HTTP/1.1\r\n" + head + key + "Origin: HTTP://Example.com\r\n\r\n", "101"},
		{"valid", "GET /ws HTTP/1.1\r\n" + head + key + "\r\n", "101"},
	} {
		resp := rawHandshake(t, ts.addr, tc.req)
		if !strings.Contains(resp[:strings.IndexByte(resp, '\n')], " "+tc.status+" ") {
			t.Fatalf("%s: %q", tc.name, resp)
		}
		switch tc.status {
		case "426":
			if !strings.Contains(resp, "Sec-Websocket-Version: 13\r\n") {
				t.Fatalf("%s: missing Sec-WebSocket-Version: %q", tc.name, resp)
			}
		case "101":
			if !strings.Contains(resp, "Sec-Websocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n") {
				t.Fatalf("%s: bad accept: %q", tc.name, resp)
			}
		}
	}
}

// Field names are matched case-insensitively whatever the server's header
// name normalization setting, including for the package's own handshake.
func TestAcceptWithoutNormalization(t *testing.T) {
	ts := startServerWith(t, nil, echo, func(s *fasthttp.Server) { s.DisableHeaderNamesNormalizing = true })
	c := ts.dial(t, nil)
	if err := c.Write(t.Context(), MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(t.Context()); err != nil || string(p) != "hi" {
		t.Fatalf("read: %q %v", p, err)
	}
	const head = "GET /ws HTTP/1.1\r\nHost: example.com\r\nupgrade: WebSocket\r\nconnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for _, tc := range []struct{ name, req, status string }{
		{"lowercase fields", head + "sec-websocket-version: 13\r\n\r\n", "101"},
		{"lowercase origin", head + "Sec-WebSocket-Version: 13\r\norigin: http://other.example\r\n\r\n", "403"},
		{"mixed-case duplicate version", head + "Sec-WebSocket-Version: 13\r\nSEC-WEBSOCKET-VERSION: 8\r\n\r\n", "426"},
	} {
		if resp := rawHandshake(t, ts.addr, tc.req); !strings.Contains(resp[:strings.IndexByte(resp, '\n')], " "+tc.status+" ") {
			t.Fatalf("%s: %q", tc.name, resp)
		}
	}
}

// Data frames must not follow the Close frame while Close waits for the peer.
func TestNoDataAfterCloseFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	if err := c.Write(t.Context(), MessageText, []byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close frame: %v", err)
	}
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), MessageText, []byte("late")); CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("write after close: %v", err)
	}
}

// A writer abandoned after a fragment went out leaves the peer mid-message;
// the connection must close rather than let a new message follow.
func TestWriterCancelledMidMessage(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	w, err := c.Writer(ctx, MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize+1)); err != nil {
		t.Fatal(err)
	}
	if h, p := peer.recv(); h != opBinary || len(p) != writeChunkSize+1 {
		t.Fatalf("first fragment %#x %d", h, len(p))
	}
	cancel()
	if err := w.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("close: %v", err)
	}
	<-c.Context().Done()
	if err := c.Write(t.Context(), MessageText, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write after abandoned writer: %v", err)
	}
}

// A message of exactly one chunk goes out as a single frame.
func TestWriterExactChunk(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	w, err := c.Writer(t.Context(), MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if h, p := peer.recv(); h != 0x80|opBinary || len(p) != writeChunkSize {
		t.Fatalf("frame %#x %d", h, len(p))
	}
}

// Waiting for the frame lock behind a stalled writer honors the context, and
// a connection closing wakes the waiters.
func TestLockWaitCancellable(t *testing.T) {
	srv, _, writes := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	stalled := make(chan error, 1)
	go func() { stalled <- c.Write(context.Background(), MessageBinary, []byte("stuck")) }()
	<-writes // The peer never reads; the writer is now stuck holding the frame lock.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx, StatusNormalClosure, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close behind stalled writer: %v", err)
	}
	if c.isClosed() {
		t.Fatal("a cancelled lock wait closed the connection")
	}
	waiting := make(chan error, 1)
	go func() { waiting <- c.Write(context.Background(), MessageText, []byte("queued")) }()
	time.Sleep(20 * time.Millisecond) // Let the second writer queue up behind the lock.
	_ = c.CloseNow()
	for _, ch := range []chan error{stalled, waiting} {
		select {
		case err := <-ch:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("writer after close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("writer not released by close")
		}
	}
}

func TestDialCancelErrorIdentity(t *testing.T) {
	silent := func(string) (net.Conn, error) {
		cli, srv := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, srv) }()
		return cli, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, "ws://example.com/", &DialOptions{Dial: silent}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := Dial(ctx, "ws://example.com/", &DialOptions{Dial: silent}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

// Unread data discarded on the way to the next message still counts against
// the size limit and must be valid UTF-8.
func TestDiscardKeepsValidating(t *testing.T) {
	c, peer := newRawServer(t, connConfig{maxMessageSize: 64})
	peer.send(frame(opBinary, false, make([]byte, 40)), frame(opContinuation, true, make([]byte, 30)))
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusMessageTooBig {
		t.Fatalf("size: %v", err)
	}

	c, peer = newRawServer(t, connConfig{})
	peer.send(frame(opText, false, []byte("ab")), frame(opContinuation, true, []byte{0xff}), frame(opText, true, []byte("next")))
	if _, r, err = c.Reader(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusInvalidFramePayloadData {
		t.Fatalf("utf-8: %v", err)
	}
}

// Close started while a Reader is half-way drains the rest and completes the
// handshake when the peer answers.
func TestCloseDrainsOpenReader(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	peer.send(frame(opText, false, []byte("part")))
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusGoingAway, "") }()
	peer.expectClose(StatusGoingAway)
	peer.send(frame(opContinuation, true, []byte("rest")), frame(opClose, true, []byte{3, 0xe9}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusGoingAway {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
}

// The peer's Close arriving while ours is blocked on its way out must not cut
// ours off: both frames are exchanged and the handshake ends with its status.
func TestSimultaneousClose(t *testing.T) {
	srv, cli, writes := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	readErr := make(chan error, 1)
	go func() {
		_, _, err := c.Read(context.Background())
		readErr <- err
	}()
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusGoingAway, "") }()
	<-writes // Our Close frame is blocked: the peer is not reading yet.
	go func() { _, _ = cli.Write(frame(opClose, true, []byte{3, 0xe8})) }()
	peer := &rawPeer{Conn: cli, t: t}
	peer.expectClose(StatusGoingAway)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-readErr; CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("read: %v", err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
}

func TestKeepHijackedConns(t *testing.T) {
	ts := startServerKeep(t, nil, echo, true)
	c := ts.dial(t, nil)
	if err := c.Write(t.Context(), MessageText, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if _, p, err := c.Read(t.Context()); err != nil || string(p) != "kept" {
		t.Fatalf("read: %q %v", p, err)
	}
	if err := c.Close(t.Context(), StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, c); CloseStatus(err) != StatusNormalClosure {
		t.Fatalf("after close: %v", err)
	}
}

func TestUTF8EarlyRejection(t *testing.T) {
	for _, tc := range []struct {
		in []byte
		ok bool
	}{
		{[]byte{0xe0, 0x80}, false},
		{[]byte{0xe0, 0xa0}, true},
		{[]byte{0xed, 0xa0}, false},
		{[]byte{0xed, 0x9f}, true},
		{[]byte{0xf0, 0x80}, false},
		{[]byte{0xf0, 0x90}, true},
		{[]byte{0xf4, 0x90}, false},
		{[]byte{0xf4, 0x8f}, true},
		{[]byte{0xc1}, false},
		{[]byte{0xf5}, false},
		{[]byte{0xe2, 0x82, 0x41}, false},
	} {
		var s utf8State
		if got := s.feed(tc.in); got != tc.ok {
			t.Fatalf("%x: got %v, want %v", tc.in, got, tc.ok)
		}
	}
}

// errConn fails every read with a fixed error.
type errConn struct {
	net.Conn

	err error
}

func (c *errConn) Read([]byte) (int, error) { return 0, c.err }

// Any transport failure without a Close frame is an abnormal closure.
func TestTransportErrorIsAbnormalClosure(t *testing.T) {
	srv, _ := tcpPair(t)
	nc := &errConn{Conn: srv, err: errors.New("connection reset by peer")}
	c := newConn(nc, nc, false, "", connConfig{})
	_, _, err := c.Read(t.Context())
	if CloseStatus(err) != StatusAbnormalClosure || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("read: %v", err)
	}
}

// When the Close reply cannot be written, the peer's violation is still what
// gets reported, not the write error.
func TestReplyFailureKeepsCause(t *testing.T) {
	srv, cli, _ := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() {
		_, _ = cli.Write([]byte{0x80 | 0x40 | opText, 0x80, 0, 0, 0, 0}) // RSV bit set.
		cli.Close()
	}()
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusProtocolError {
		t.Fatalf("read: %v", err)
	}
}

// A control reply is bounded by what is left of the read call's ReadTimeout,
// not by a fresh one.
func TestReplyWithinReadDeadline(t *testing.T) {
	srv, cli, _ := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{readTimeout: 300 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = cli.Write(frame(opPing, true, []byte("p"))) // The pong is never read.
	}()
	start := time.Now()
	if _, _, err := c.Read(context.Background()); err == nil {
		t.Fatal("expected the read to give up")
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Fatalf("read took %v, the reply restarted the timeout", d)
	}
}

// WriteTimeout bounds the wait for an open Writer too.
func TestWriteTimeoutBoundsLockWait(t *testing.T) {
	srv, _ := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{writeTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	w, err := c.Writer(t.Context(), MessageText)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(context.Background(), MessageText, []byte("queued")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write behind an open writer: %v", err)
	}
	if c.isClosed() {
		t.Fatal("a lock timeout closed the connection")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// CloseRead must not release the slice the last Read handed out.
func TestCloseReadKeepsReadBuffer(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	if _, err := cli.Write(frame(opText, true, []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	_, p, err := c.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.CloseRead(t.Context())
	if _, err := cli.Write(frame(opPing, true, nil)); err != nil {
		t.Fatal(err)
	}
	peer := &rawPeer{Conn: cli, t: t}
	if h, _ := peer.recv(); h != 0x80|opPong {
		t.Fatalf("expected a pong, got %#x", h)
	}
	if c.readBuf == nil || string(p) != "hello" {
		t.Fatalf("buffer released by CloseRead: %q", p)
	}
}

func TestWriterRejectsDoneContext(t *testing.T) {
	srv, _ := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Writer(ctx, MessageText); !errors.Is(err, context.Canceled) {
		t.Fatalf("writer: %v", err)
	}
	if err := c.Write(t.Context(), MessageText, []byte("still usable")); err != nil {
		t.Fatal(err)
	}
}

// Cancelling the read while its pong is blocked ends the read promptly.
func TestCancelDuringBlockedPong(t *testing.T) {
	srv, cli, writes := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() { _, _ = cli.Write(frame(opPing, true, []byte("p"))) }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(ctx)
		done <- err
	}()
	<-writes // The pong is blocked: the peer never reads.
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
			t.Fatalf("read: %v after %v", err, time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not reach the pong write")
	}
}

// One deadline covers the lock wait and the write of a reply.
func TestReplyBudgetAcrossLockWait(t *testing.T) {
	srv, cli, writes := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{readTimeout: 300 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	stuck := make(chan error, 1)
	go func() { stuck <- c.Write(context.Background(), MessageBinary, []byte("held")) }()
	<-writes // A writer holds the frame lock.
	go func() { _, _ = cli.Write(frame(opPing, true, []byte("p"))) }()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(context.Background())
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	// Let the held write finish; the pong then blocks on the peer.
	peer := &rawPeer{Conn: cli, t: t}
	peer.recv()
	<-stuck
	if err := <-done; err == nil {
		t.Fatal("expected the read to give up")
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Fatalf("read took %v: the lock wait was not counted", d)
	}
}

// One deadline covers the message lock wait and the write of a Write call.
func TestWriteBudgetAcrossLockWait(t *testing.T) {
	srv, cli, _ := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{writeTimeout: 300 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	w, err := c.Writer(t.Context(), MessageText)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Write(context.Background(), MessageText, []byte("queued")) }()
	time.Sleep(200 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	peer := &rawPeer{Conn: cli, t: t}
	peer.recv() // The empty final frame; after it the peer stops reading.
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("expected the write to give up")
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Fatalf("write took %v: the lock wait was not counted", d)
	}
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

// The connection's context is cancelled before the transport close starts, so
// callers that return early see the cause.
func TestContextCancelledBeforeTransportClose(t *testing.T) {
	srv, _ := tcpPair(t)
	nc := &slowCloseConn{Conn: srv}
	c := newConn(nc, nc, false, "", connConfig{})
	go func() { _ = c.CloseNow() }()
	select {
	case <-c.Context().Done():
		if !errors.Is(context.Cause(c.Context()), net.ErrClosed) {
			t.Fatalf("cause: %v", context.Cause(c.Context()))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("context not cancelled until the transport closed")
	}
}

// CloseRead's read ending for any reason closes the connection, so a handler
// waiting on the context is released.
func TestCloseReadEndsConnection(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	c.CloseRead(t.Context())
	cli.Close()
	select {
	case <-c.Context().Done():
		if CloseStatus(context.Cause(c.Context())) != StatusAbnormalClosure {
			t.Fatalf("cause: %v", context.Cause(c.Context()))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after the peer left")
	}

	srv, _ = tcpPair(t)
	c = newConn(srv, srv, false, "", connConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	c.CloseRead(ctx)
	cancel()
	select {
	case <-c.Context().Done():
		if !errors.Is(context.Cause(c.Context()), context.Canceled) {
			t.Fatalf("cause: %v", context.Cause(c.Context()))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after the read was cancelled")
	}
}

// A transport whose timeout error is not a net.Error is still a timeout.
func TestPipeTimeoutIsTimeout(t *testing.T) {
	c, _ := newRawServer(t, connConfig{readTimeout: 30 * time.Millisecond})
	_, _, err := c.Read(t.Context())
	if !errors.Is(err, fasthttputil.ErrTimeout) || CloseStatus(err) != -1 {
		t.Fatalf("read: %v", err)
	}
}

// testCert returns a self-signed certificate for example.com.
func testCert(t *testing.T) tls.Certificate {
	t.Helper()
	cert, key, err := fasthttp.GenerateTestCertificate("example.com")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// tlsPipe returns the client end of a synchronous pipe whose server end
// speaks TLS and runs serve after the handshake.
func tlsPipe(t *testing.T, serve func(net.Conn)) net.Conn {
	t.Helper()
	pair := testCert(t)
	cli, srv := net.Pipe()
	t.Cleanup(func() { cli.Close(); srv.Close() })
	go func() {
		tc := tls.Server(srv, &tls.Config{Certificates: []tls.Certificate{pair}})
		if tc.Handshake() == nil {
			serve(tc)
		}
	}()
	return cli
}

var insecureTLS = &tls.Config{InsecureSkipVerify: true} // Self-signed test certificates.

// A failed TLS handshake response is cleaned up without waiting for the
// close_notify alert that a peer which stopped reading would never take.
func TestDialTLSFailureCleanup(t *testing.T) {
	cli := tlsPipe(t, func(c net.Conn) {
		_, _ = readHead(bufio.NewReader(c))
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		time.Sleep(2 * time.Second) // Neither reads nor closes.
	})
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, "wss://example.com/", &DialOptions{Dial: func(string) (net.Conn, error) { return cli, nil }, TLSConfig: insecureTLS})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("dial: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cleanup took %v", d)
	}
}

// perIPLike stands in for fasthttp's per-IP TLS wrapper: closing must look
// through it and the hijacked conn to skip close_notify.
type perIPLike struct{ *tls.Conn }

func TestCloseTransportLooksThroughWrappers(t *testing.T) {
	cli := tlsPipe(t, func(net.Conn) { time.Sleep(2 * time.Second) }) // The peer never reads.
	tc := tls.Client(cli, insecureTLS)
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	closeTransport(hijackLike{Conn: perIPLike{tc}, keep: true})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close took %v", d)
	}
}

// eofConn delivers its data and the end of the stream in one Read, as TLS can.
type eofConn struct {
	net.Conn

	data []byte
}

func (c *eofConn) Read(p []byte) (int, error) {
	n := copy(p, c.data)
	c.data = c.data[n:]
	if len(c.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

// A complete final frame delivered together with EOF is a complete message;
// the transport's end is reported by the next read.
func TestReaderCompleteFrameWithEOF(t *testing.T) {
	srv, _ := tcpPair(t)
	nc := &eofConn{Conn: srv, data: frame(opBinary, true, make([]byte, 5000))}
	c := newConn(nc, nc, false, "", connConfig{})
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := io.ReadAll(r); err != nil || len(p) != 5000 {
		t.Fatalf("stream: %d bytes, %v", len(p), err)
	}
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusAbnormalClosure {
		t.Fatalf("read after EOF: %v", err)
	}
}

// A call waiting on the connection's own context reports the close reason,
// not a cancellation, when the connection closes.
func TestWaitOnConnContextReportsCloseReason(t *testing.T) {
	srv, _ := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	w, err := c.Writer(t.Context(), MessageText)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		done := make(chan error, 1)
		go func() { done <- c.Write(c.Context(), MessageText, []byte("queued")) }()
		time.Sleep(5 * time.Millisecond)
		_ = c.CloseNow()
		if err := <-done; !errors.Is(err, net.ErrClosed) {
			t.Fatalf("queued write: %v", err)
		}
		if w.Close() == nil {
			t.Fatal("writer closed cleanly on a closed connection")
		}
		srv, _ = tcpPair(t)
		c = newConn(srv, srv, false, "", connConfig{})
		if w, err = c.Writer(t.Context(), MessageText); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.CloseNow()
}

func TestAcceptRejectsDuplicateSubprotocols(t *testing.T) {
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"chat", "chat"}}, echo)
	req := "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if resp := rawHandshake(t, ts.addr, req); !strings.Contains(resp[:strings.IndexByte(resp, '\n')], " 500 ") {
		t.Fatalf("response: %q", resp)
	}
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
