package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/valyala/fasthttp"
)

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

// Closing the connection closes the socket at once, although the handler has
// not returned.
func TestCloseNowClosesSocketBeforeHandlerReturns(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ts := startServer(t, nil, func(c *Conn) {
		_ = c.CloseNow()
		<-release
	})
	c := ts.dial(t, nil)
	_ = c.nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.nc.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the socket is still open: %v", err)
	}
}

// The conn given to Server.ServeConn is closed once: by fasthttp after the
// handler, or with the Conn when the server keeps hijacked conns.
func TestServeConnClosedOnce(t *testing.T) {
	var h fasthttp.ResponseHeader
	_ = h.Header() // fasthttp's date goroutine must not start inside the bubble.
	for _, keep := range []bool{false, true} {
		for _, closeNow := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				srv, cli := net.Pipe()
				defer srv.Close()
				defer cli.Close()
				var closed atomic.Int32
				dialed, inHandler := make(chan struct{}), make(chan int32, 1)
				s := &fasthttp.Server{KeepHijackedConns: keep, Handler: func(ctx *fasthttp.RequestCtx) {
					_ = Accept(ctx, nil, func(c *Conn) {
						<-dialed
						if closeNow {
							_ = c.CloseNow()
						}
						inHandler <- closed.Load()
					})
				}}
				go func() { _ = s.ServeConn(closeCountConn{Conn: srv, closed: &closed}) }()
				opts := &DialOptions{Dial: func(string) (net.Conn, error) { return cli, nil }}
				if _, err := Dial(t.Context(), "ws://example.com/", opts); err != nil {
					t.Fatal(err)
				}
				close(dialed)
				want := int32(0)
				if keep && closeNow {
					want = 1
				}
				if n := <-inHandler; n != want {
					t.Fatalf("keep=%v closeNow=%v: %d Close calls in the handler, want %d", keep, closeNow, n, want)
				}
				synctest.Wait() // The handler has returned and fasthttp is done with the conn.
				if n := closed.Load(); n != 1 {
					t.Fatalf("keep=%v closeNow=%v: the served conn was closed %d times", keep, closeNow, n)
				}
			})
		}
	}
}

func TestHandlerReturnClosesConn(t *testing.T) {
	ts := startServer(t, nil, func(*Conn) {})
	c := ts.dial(t, nil)
	if err := readErr(t, c); CloseStatus(err) != StatusAbnormalClosure {
		t.Fatalf("read: %v", err)
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

func TestCloseFrames(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	peer.send(frame(opClose, true, []byte{0x0b, 0xb8, 'o', 'k'}))
	_, _, err := c.Read(t.Context())
	if CloseStatus(err) != 3000 || err.Error() != "websocket: close 3000: ok" {
		t.Fatalf("read: %v", err)
	}
	if h, p := peer.recv(); h != 0x80|opClose || !bytes.Equal(p, []byte{0x0b, 0xb8}) {
		t.Fatalf("reply %#x %v", h, p)
	}

	c, peer = newRawServer(t, connConfig{})
	peer.send(frame(opClose, true, nil))
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusNoStatusReceived {
		t.Fatalf("read: %v", err)
	}
	if h, p := peer.recv(); h != 0x80|opClose || len(p) != 0 {
		t.Fatalf("reply %#x %v", h, p)
	}
}

func TestCloseNoReply(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx, StatusGoingAway, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > time.Second {
		t.Fatalf("close took %v", d)
	}
	peer.expectClose(StatusGoingAway)
	<-c.Context().Done()
	if err := context.Cause(c.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cause: %v", err)
	}
}

func TestCtxWatch(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	w := &newConn(c1, c1, false, "", connConfig{}).rdWatch
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := w.begin(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	w.end()
	cancel()
	// An idle watch must not disturb the next call with a fresh context.
	start := time.Now()
	if _, err := w.begin(t.Context(), start.Add(50*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c1.Read(b[:]); err == nil {
		t.Fatal("expected timeout")
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("read returned after %v", d)
	}
	w.end()
	// Cancellation during a call aborts it.
	ctx, cancel = context.WithCancel(t.Context())
	if _, err := w.begin(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := c1.Read(b[:]); err == nil {
		t.Fatal("expected abort")
	}
	w.end()
}

// Two concurrent Close calls send exactly one Close frame and both return nil.
func TestCloseConcurrentCallsSendOneFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- c.Close(t.Context(), StatusNormalClosure, "") }()
	}
	peer.expectClose(StatusNormalClosure)
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := peer.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after the handshake: %d bytes, %v", n, err)
	}
}

// A peer Close or violation arriving while our Close frame is on its way out
// yields exactly one Close frame on the wire, and both Close calls return nil.
func TestCloseFrameDedupAgainstPeer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []byte
		code  StatusCode
	}{
		{"peer close", frame(opClose, true, []byte{3, 0xe8}), StatusNormalClosure},
		{"bad frame", []byte{0xc1, 0x80}, StatusProtocolError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli, writes := pipePair(t)
			c := newConn(srv, srv, false, "", connConfig{})
			t.Cleanup(func() { _ = c.CloseNow() })
			readDone := make(chan error, 1)
			go func() {
				_, _, err := c.Read(t.Context())
				readDone <- err
			}()
			closeDone := make(chan error, 2)
			go func() { closeDone <- c.Close(t.Context(), StatusGoingAway, "") }()
			<-writes // Our Close frame is blocked: the peer is not reading yet.
			if _, err := cli.Write(tc.input); err != nil {
				t.Fatal(err)
			}
			go func() { closeDone <- c.Close(t.Context(), StatusGoingAway, "") }()
			wire, err := io.ReadAll(cli)
			if err != nil || !bytes.Equal(wire, []byte{0x80 | opClose, 2, 3, 0xe9}) {
				t.Fatalf("wire %x, %v; want exactly one Close frame", wire, err)
			}
			if err := <-readDone; CloseStatus(err) != tc.code {
				t.Fatalf("read: %v", err)
			}
			for range 2 {
				if err := <-closeDone; err != nil {
					t.Fatalf("close: %v", err)
				}
			}
			if err := c.Write(t.Context(), MessageText, []byte("late")); err == nil {
				t.Fatal("data accepted after the Close frame")
			}
		})
	}
}

// lockWaiter returns the lock that method needs and a call that waits for it.
func lockWaiter(t *testing.T, ctx context.Context, c *Conn, method string) (lock, func() error) {
	t.Helper()
	switch method {
	case "Write":
		return c.msgMu, func() error { return c.Write(ctx, MessageText, nil) }
	case "Writer":
		return c.msgMu, func() error { _, err := c.Writer(ctx, MessageText); return err }
	case "Ping":
		return c.pingMu, func() error { return c.Ping(ctx) }
	case "Ping frame":
		return c.frameMu, func() error { return c.Ping(ctx) }
	case "Close":
		return c.frameMu, func() error { return c.Close(ctx, StatusNormalClosure, "") }
	case "Read":
		return c.readMu, func() error { _, _, err := c.Read(ctx); return err }
	case "Reader":
		return c.readMu, func() error { _, _, err := c.Reader(ctx); return err }
	case "msgReader":
		r := &msgReader{c: c, ctx: ctx}
		return c.readMu, func() error { _, err := r.Read(make([]byte, 1)); return err }
	default:
		w, err := c.Writer(ctx, MessageText)
		if err != nil {
			t.Fatal(err)
		}
		return c.frameMu, w.Close
	}
}

// A lock wait ends with the caller's error when its context ends and with the
// close reason when the connection closes, taking no token and closing nothing.
func TestLockWaitHonorsContextAndClose(t *testing.T) {
	methods := []string{"Write", "Writer", "Ping", "Ping frame", "Close", "Read", "Reader", "msgReader", "msgWriter"}
	for _, closeConn := range []bool{false, true} {
		for _, method := range methods {
			t.Run(method+" closed="+strconv.FormatBool(closeConn), func(t *testing.T) {
				srv, _ := tcpPair(t)
				c := newConn(srv, srv, false, "", connConfig{})
				t.Cleanup(func() { _ = c.CloseNow() })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				held, call := lockWaiter(t, ctx, c, method)
				held <- struct{}{}
				done := make(chan error, 1)
				go func() { done <- call() }()
				want := context.Canceled
				if closeConn {
					_ = c.CloseNow()
					want = net.ErrClosed
				} else {
					cancel()
				}
				err := <-done
				// Close reports a closed connection with nil, whoever closed it.
				if closeConn && method == "Close" {
					want, err = nil, nil
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
				if len(held) != 1 {
					t.Fatal("the waiter took the holder's token")
				}
				if !closeConn && c.isClosed() {
					t.Fatal("a cancelled wait closed the connection")
				}
				held.release()
			})
		}
	}
}

// shutdown returns only after the in-flight read and write have left the
// transport, and no call made afterwards touches it.
func TestShutdownWaitsForInFlightIO(t *testing.T) {
	srv, cli := net.Pipe()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	nc := newGatedConn(srv)
	c := newConn(nc, nc, false, "", connConfig{})
	readDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := c.Read(t.Context())
		readDone <- err
	}()
	go func() { writeDone <- c.Write(t.Context(), MessageBinary, []byte("x")) }()
	<-nc.readEntered
	<-nc.writeEntered
	shutdownDone := make(chan struct{})
	go func() { c.shutdown(); close(shutdownDone) }()
	<-nc.readReturned // The shutdown deadline interrupted both pipe operations.
	<-nc.writeReturned
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned with a read and a write in flight")
	default:
	}
	close(nc.readExit)
	if err := <-readDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read: %v", err)
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned with a write in flight")
	default:
	}
	close(nc.writeExit)
	if err := <-writeDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write: %v", err)
	}
	<-shutdownDone
	nc.retired.Store(true)
	if _, _, err := c.Read(t.Context()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("late read: %v", err)
	}
	_ = c.Write(t.Context(), MessageText, nil)
	_, _ = c.Writer(t.Context(), MessageText)
	_ = c.Ping(t.Context())
	_ = c.Close(t.Context(), StatusNormalClosure, "")
	c.CloseRead(t.Context())
	c.readMu.wait()
	c.shutdown()
	if n := nc.late.Load(); n != 0 {
		t.Fatalf("%d transport calls after shutdown", n)
	}
}

// Once the connection reports itself closed, context.Cause already carries the
// reason, although the transport close may still be in progress.
func TestCloseCauseVisibleWithClosedState(t *testing.T) {
	srv, _ := tcpPair(t)
	nc := &slowCloseConn{Conn: srv}
	c := newConn(nc, nc, false, "", connConfig{})
	go func() { _ = c.CloseNow() }()
	for !c.isClosed() {
		runtime.Gosched()
	}
	if err := context.Cause(c.Context()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("cause: %v", err)
	}
	if err := c.Close(t.Context(), StatusNormalClosure, ""); err != nil {
		t.Fatalf("close on a closing connection: %v", err)
	}
}

// CloseRead, a local Close, the peer's Close and a CloseNow racing on one
// connection never deadlock: Close returns nil and shutdown completes.
func TestCloseReadClosePeerCloseStorm(t *testing.T) {
	for i := range 60 {
		mode := i % 3
		srv, cli := tcpPair(t)
		c := newConn(srv, srv, false, "", connConfig{})
		c.CloseRead(t.Context())
		input := frame(opClose, true, []byte{3, 0xe8})
		if mode == 1 {
			input = append(frame(opText, true, []byte("x")), input...)
		}
		go func() { _, _ = cli.Write(input) }()
		go func() { _, _ = io.Copy(io.Discard, cli) }()
		if mode == 2 {
			go func() { _ = c.CloseNow() }()
		}
		done := make(chan error, 1)
		go func() { done <- c.Close(c.Context(), StatusNormalClosure, "") }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: close: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: close hung", i)
		}
		<-c.Context().Done()
		cause := context.Cause(c.Context())
		if code := CloseStatus(cause); code != StatusNormalClosure && code != StatusPolicyViolation && !errors.Is(cause, net.ErrClosed) {
			t.Fatalf("iteration %d: cause %v", i, cause)
		}
		shutdownDone := make(chan struct{})
		go func() { c.shutdown(); close(shutdownDone) }()
		select {
		case <-shutdownDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: shutdown hung", i)
		}
		cli.Close()
	}
}

// A handler that returns while its own goroutines still read and write on the
// connection leaves fasthttp's recycled transport untouched.
func TestShutdownRacesHandlerGoroutines(t *testing.T) {
	const conns = 20
	finished := make(chan struct{}, conns)
	ts := startServer(t, nil, func(c *Conn) {
		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				if _, _, err := c.Read(c.Context()); err != nil {
					return
				}
			}
		})
		wg.Go(func() {
			for {
				if err := c.Write(c.Context(), MessageText, []byte("tick")); err != nil {
					return
				}
			}
		})
		go func() { wg.Wait(); finished <- struct{}{} }()
	})
	for range conns {
		c := ts.dial(t, nil)
		_ = c.Write(t.Context(), MessageText, []byte("x"))
		_, _, _ = c.Read(t.Context())
		_ = c.CloseNow()
	}
	for range conns {
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("handler goroutines did not finish")
		}
	}
}

// A pong satisfies only the ping it answers: duplicates, pongs for an earlier
// ping and foreign pongs do not complete the next one.
func TestPingIgnoresStalePongs(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	c.CloseRead(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Ping(t.Context()) }()
	h, first := peer.recv()
	if h != 0x80|opPing {
		t.Fatalf("got %#x", h)
	}
	peer.send(frame(opPong, true, first), frame(opPong, true, first))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	go func() { done <- c.Ping(ctx) }()
	_, second := peer.recv()
	if bytes.Equal(first, second) {
		t.Fatal("the ping payload did not change")
	}
	peer.send(frame(opPong, true, first), frame(opPong, true, []byte("foreign")))
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stale pong completed the ping: %v", err)
	}
	go func() { done <- c.Ping(t.Context()) }()
	_, third := peer.recv()
	peer.send(frame(opPong, true, third))
	if err := <-done; err != nil {
		t.Fatalf("ping after stale pongs: %v", err)
	}
}

// Ping waiting for its pong on the connection's own context reports the close
// reason, not a cancellation, when the connection closes.
func TestPingOnConnContextReportsCloseReason(t *testing.T) {
	for range 20 {
		c, peer := newRawServer(t, connConfig{})
		done := make(chan error, 1)
		go func() { done <- c.Ping(c.Context()) }()
		peer.recv() // The ping is out; the pong never comes.
		_ = c.CloseNow()
		if err := <-done; !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ping: %v", err)
		}
	}
}

// ReadTimeout does not apply to CloseRead: a write-only connection whose peer
// is silent for longer than ReadTimeout still answers a later ping.
func TestCloseReadIgnoresReadTimeout(t *testing.T) {
	c, peer := newRawServer(t, connConfig{readTimeout: 20 * time.Millisecond})
	c.CloseRead(t.Context())
	time.Sleep(80 * time.Millisecond) // Longer than ReadTimeout; nothing else to wait for.
	peer.send(frame(opPing, true, []byte("p")))
	if h, p := peer.recv(); h != 0x80|opPong || string(p) != "p" {
		t.Fatalf("got %#x %q", h, p)
	}
	if c.isClosed() {
		t.Fatal("CloseRead closed the connection after ReadTimeout")
	}
}

// Read, Write and Ping with an already ended context fail with the context's
// error on the uncontended path and leave the connection usable.
func TestDoneContextLeavesConnectionUsable(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := c.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("read: %v", err)
	}
	if err := c.Write(ctx, MessageText, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write: %v", err)
	}
	if err := c.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ping: %v", err)
	}
	if c.isClosed() {
		t.Fatal("a call with an ended context closed the connection")
	}
	if err := c.Write(t.Context(), MessageText, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if h, p := peer.recv(); h != 0x80|opText || string(p) != "ok" {
		t.Fatalf("got %#x %q", h, p)
	}
}

// A peer that sends its Close frame and then ends the TCP connection is
// reported with its status code, not as an abnormal closure.
func TestCloseFrameThenEOFReportsPeerStatus(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	if _, err := cli.Write(frame(opClose, true, []byte{3, 0xe9})); err != nil {
		t.Fatal(err)
	}
	cli.Close()
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusGoingAway {
		t.Fatalf("read: %v", err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusGoingAway {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
}

// A failed acquire, whatever ended it, never takes or disturbs the holder's
// token, and wait leaves the lock free.
func TestAcquireFailureKeepsToken(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline", "expired", "closed", "released"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := newRawServer(t, connConfig{})
			l := newLock()
			l <- struct{}{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var deadline time.Time
			var want error
			switch mode {
			case "cancelled":
				cancel()
				want = context.Canceled
			case "deadline":
				deadline, want = time.Now().Add(20*time.Millisecond), os.ErrDeadlineExceeded
			case "expired":
				deadline, want = time.Now().Add(-time.Second), os.ErrDeadlineExceeded
			case "closed":
				_ = c.CloseNow()
				want = net.ErrClosed
			default:
				l.release()
			}
			if err := c.acquire(ctx, l, deadline); !errors.Is(err, want) {
				t.Fatalf("acquire: %v, want %v", err, want)
			}
			if len(l) != 1 {
				t.Fatalf("%d tokens after acquire", len(l))
			}
			l.release()
			l.wait()
			if len(l) != 0 {
				t.Fatal("wait leaked a token")
			}
		})
	}
}

// deadlineConn records the read deadlines set on it.
type deadlineConn struct {
	net.Conn

	mu   sync.Mutex
	last time.Time
	n    int
}

func (c *deadlineConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.last, c.n = d, c.n+1
	c.mu.Unlock()
	return nil
}

func (c *deadlineConn) deadline() (time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.n
}

// An abort from a call's earlier context must not touch the deadline of a
// later call, and after stopWatch nothing is armed or aborted any more.
func TestWatchStaleAbortIgnored(t *testing.T) {
	nc := &deadlineConn{}
	w := &newConn(nc, nc, false, "", connConfig{}).rdWatch
	old, cancelOld := context.WithCancel(t.Context())
	defer cancelOld()
	if _, err := w.begin(old, time.Time{}); err != nil {
		t.Fatal(err)
	}
	oldGen := w.gen
	w.end()
	next := time.Now().Add(time.Hour)
	if _, err := w.begin(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	w.abort(oldGen, context.Canceled) // The old context's callback runs late.
	if d, _ := nc.deadline(); !d.Equal(next) {
		t.Fatalf("a stale abort changed the deadline to %v", d)
	}
	w.abort(w.gen, context.Canceled)
	if d, _ := nc.deadline(); !d.Equal(aLongTimeAgo) {
		t.Fatalf("the current abort left the deadline at %v", d)
	}
	w.end()
	w.stopWatch()
	_, n := nc.deadline()
	w.abort(w.gen, context.Canceled)
	if _, err := w.begin(t.Context(), time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("begin after stop: %v", err)
	}
	if _, m := nc.deadline(); m != n {
		t.Fatal("the stopped watch touched the transport")
	}
}

// refusingConn reports its reads and refuses the deadline a cancellation sets.
type refusingConn struct {
	net.Conn

	reading chan struct{}
}

func (c refusingConn) Read(p []byte) (int, error) {
	signal(c.reading)
	return c.Conn.Read(p)
}

func (c refusingConn) SetReadDeadline(d time.Time) error {
	if d.Equal(aLongTimeAgo) {
		return errors.New("deadline refused")
	}
	return c.Conn.SetReadDeadline(d)
}

// A cancelled call whose transport refuses the deadline is interrupted by
// closing the connection, which reports the context's error.
func TestCancelClosesConnWhenDeadlineRefused(t *testing.T) {
	srv, _ := tcpPair(t)
	reading := make(chan struct{}, 1)
	nc := hijackLike{Conn: refusingConn{Conn: srv, reading: reading}}
	c := newConn(nc, nc, false, "", connConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Read(ctx)
		done <- err
	}()
	<-reading
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(context.Cause(c.Context()), context.Canceled) {
			t.Fatalf("read: %v, cause: %v", err, context.Cause(c.Context()))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read was not interrupted")
	}
}

// fatal reports the context's error first, then an expired deadline, keeps a
// timeout, maps anything else to an abnormal closure; the first reason sticks.
func TestFatalPrecedence(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context //nolint:containedctx // Table input.
		in   error
		want error
	}{
		{"cancelled context wins over EOF", cancelled, io.EOF, context.Canceled},
		{"expired deadline wins over EOF", pastDeadlineContext{t.Context()}, io.EOF, context.DeadlineExceeded},
		{"timeout kept", t.Context(), os.ErrDeadlineExceeded, os.ErrDeadlineExceeded},
		{"transport error", t.Context(), io.EOF, CloseError{Code: StatusAbnormalClosure, Reason: "EOF"}},
	} {
		c, _ := newRawServer(t, connConfig{})
		if err := c.fatal(tc.ctx, tc.in); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := c.fatal(t.Context(), errors.New("later")); !errors.Is(err, tc.want) {
			t.Fatalf("%s: a second fatal returned %v", tc.name, err)
		}
	}
}

// A pong that CloseRead cannot deliver within WriteTimeout ends the connection
// rather than leaving a silent reader behind a live context.
func TestCloseReadPongTimeoutClosesConnection(t *testing.T) {
	srv, cli, _ := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{writeTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	c.CloseRead(t.Context())
	if _, err := cli.Write(frame(opPing, true, []byte("p"))); err != nil { // The pong is never read.
		t.Fatal(err)
	}
	select {
	case <-c.Context().Done():
		if cause := context.Cause(c.Context()); !isTimeout(cause) {
			t.Fatalf("cause: %v", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection not closed after the pong timed out")
	}
}

// When two Close calls wait for the peer, the one with the earlier deadline
// closes the connection at its deadline and both return nil.
func TestCloseShorterDeadlineEndsBothWaits(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	long := make(chan error, 1)
	go func() { long <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Close(ctx, StatusNormalClosure, ""); err != nil {
		t.Fatalf("close with the shorter deadline: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("close with the shorter deadline took %v", d)
	}
	if err := <-long; err != nil {
		t.Fatalf("close with the longer deadline: %v", err)
	}
	if cause := context.Cause(c.Context()); !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause: %v", cause)
	}
}

// The wait of Close ends at the caller's deadline; a shorter ReadTimeout does
// not cut it short.
func TestCloseContextDeadlineOutlivesReadTimeout(t *testing.T) {
	const readTimeout = 30 * time.Millisecond
	c, peer := newRawServer(t, connConfig{readTimeout: readTimeout})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Close(ctx, StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	time.Sleep(4 * readTimeout) // The peer answers only after ReadTimeout has passed.
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cause := context.Cause(c.Context()); CloseStatus(cause) != StatusNormalClosure {
		t.Fatalf("cause: %v", cause)
	}
}

// Once the Close frame is out, a later Close with its own deadline ends the
// wait at that deadline even while a control reply holds the frame lock.
func TestSecondCloseDeadlineEndsWait(t *testing.T) {
	srv, cli, writes := pipePair(t)
	c := newConn(srv, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	peer := &rawPeer{Conn: cli, t: t}
	firstDone := make(chan error, 1)
	go func() { firstDone <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	<-writes // The first Close's frame.
	if _, err := cli.Write(frame(opPing, true, []byte("p"))); err != nil {
		t.Fatal(err)
	}
	<-writes // The pong holds the frame lock until the peer reads it.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx, StatusNormalClosure, ""); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cause := context.Cause(c.Context()); !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause: %v", cause)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first Close did not end")
	}
}

// After the peer closed the connection, Ping reports the close reason like
// Read and Write do.
func TestPingAfterPeerCloseReportsReason(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	peer.send(frame(opClose, true, append([]byte{3, 0xf0}, "policy"...)))
	if _, _, err := c.Read(t.Context()); CloseStatus(err) != StatusPolicyViolation {
		t.Fatalf("read: %v", err)
	}
	if err := c.Ping(t.Context()); CloseStatus(err) != StatusPolicyViolation {
		t.Fatalf("ping: %v", err)
	}
	if err := c.Write(t.Context(), MessageText, nil); CloseStatus(err) != StatusPolicyViolation {
		t.Fatalf("write: %v", err)
	}
}

// Nothing but the peer's reply may follow the Close frame; Ping is refused.
func TestPingAfterCloseFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close(t.Context(), StatusNormalClosure, "bye") }()
	if h, _ := peer.recv(); h != 0x80|opClose {
		t.Fatalf("first frame %#x, want close", h)
	}
	if err := c.Ping(t.Context()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ping after the Close frame: %v", err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var h [2]byte
	if _, err := io.ReadFull(peer, h[:]); !isTimeout(err) {
		t.Fatalf("after the Close frame: frame %#x, %v", h[0], err)
	}
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-closeDone; err != nil {
		t.Fatalf("close: %v", err)
	}
}

// deadlineSpyConn records the read and write deadlines actually set.
type deadlineSpyConn struct {
	net.Conn

	mu          sync.Mutex
	read, write time.Time
}

func (c *deadlineSpyConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.read = d
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(d)
}

func (c *deadlineSpyConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.write = d
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(d)
}

func (c *deadlineSpyConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.read, c.write = d, d
	c.mu.Unlock()
	return c.Conn.SetDeadline(d)
}

// The deadline cache agrees with the socket after every call while the
// connection stays usable, contexts expiring between calls included.
func TestDeadlineCacheAgreesWithSocket(t *testing.T) {
	srv, cli := tcpPair(t)
	dc := &deadlineSpyConn{Conn: srv}
	c := newConn(dc, dc, false, "", connConfig{readTimeout: 2 * time.Second, writeTimeout: 3 * time.Second})
	t.Cleanup(func() { _ = c.CloseNow() })
	peer := bufio.NewReader(cli)
	agree := func(stage string) {
		t.Helper()
		dc.mu.Lock()
		defer dc.mu.Unlock()
		for _, w := range []struct {
			name string
			w    *ctxWatch
			got  time.Time
		}{{"read", &c.rdWatch, dc.read}, {"write", &c.wrWatch, dc.write}} {
			w.w.mu.Lock()
			set, last := w.w.set, w.w.last
			w.w.mu.Unlock()
			if set && !last.Equal(w.got) {
				t.Fatalf("%s: %s cache says %v, socket has %v", stage, w.name, last, w.got)
			}
		}
	}
	ctx := t.Context()
	// 1. Write with a context deadline, then let that deadline pass between calls.
	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := c.Write(wctx, MessageBinary, []byte("a")); err != nil {
		t.Fatal(err)
	}
	agree("write with ctx deadline")
	if _, _, _, err := readPlainFrame(peer); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // The AfterFunc fires with no call active.
	cancel()
	agree("after the write context expired")
	// 2. A plain Write afterwards.
	if err := c.Write(ctx, MessageBinary, []byte("b")); err != nil {
		t.Fatal(err)
	}
	agree("plain write")
	if _, _, _, err := readPlainFrame(peer); err != nil {
		t.Fatal(err)
	}
	// 3. Read serving a ping, whose reply arms the write side, then data.
	if _, err := cli.Write(append(frame(opPing, true, []byte("p")), frame(opBinary, true, []byte("x"))...)); err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithTimeout(ctx, time.Second)
	if _, p, err := c.Read(rctx); err != nil || string(p) != "x" {
		t.Fatalf("read: %q %v", p, err)
	}
	agree("read with reply")
	if op, _, _, err := readPlainFrame(peer); err != nil || op != opPong {
		t.Fatalf("pong: %d %v", op, err)
	}
	time.Sleep(1100 * time.Millisecond) // The read context expires with no call active.
	rcancel()
	agree("after the read context expired")
	// 4. Ping/pong round trip from our side, pong handled by a Read.
	go func() {
		op, _, payload, err := readPlainFrame(peer)
		if err == nil && op == opPing {
			_, _ = cli.Write(frame(opPong, true, payload))
			_, _ = cli.Write(frame(opBinary, true, []byte("y")))
		}
	}()
	readDone := make(chan error, 1)
	go func() {
		_, _, err := c.Read(ctx)
		readDone <- err
	}()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	agree("after ping and read")
	// 5. A Reader whose context is cancelled between its Read calls.
	if _, err := cli.Write(append(frame(opBinary, false, []byte("12")), frame(opContinuation, true, []byte("34"))...)); err != nil {
		t.Fatal(err)
	}
	mctx, mcancel := context.WithCancel(ctx)
	_, r, err := c.Reader(mctx)
	if err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := r.Read(one[:]); err != nil {
		t.Fatal(err)
	}
	mcancel()
	time.Sleep(20 * time.Millisecond)
	agree("after the reader context was cancelled between reads")
	if _, err := r.Read(one[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel: %v", err)
	}
	if c.isClosed() {
		t.Fatal("closed")
	}
	// 6. Plain write still works and the socket agrees.
	if err := c.Write(ctx, MessageBinary, []byte("z")); err != nil {
		t.Fatal(err)
	}
	agree("final write")
}
