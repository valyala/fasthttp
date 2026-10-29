package websocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// okHead is a 101 response up to its Sec-WebSocket-Accept field.
const okHead = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"

// probeAccept answers a handshake with a plain 101 response.
func probeAccept(accept string) string {
	return okHead + "Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
}

// Two concurrent Close calls send exactly one Close frame and both return nil.
func TestProbeCloseConcurrentCallsSendOneFrame(t *testing.T) {
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
func TestProbeCloseFrameDedupAgainstPeer(t *testing.T) {
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

// A Writer whose fragment went out before the Close frame is refused with
// net.ErrClosed when it flushes afterwards and must not end the handshake.
func TestProbeWriterMidMessageRefusedAfterCloseFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	w, err := c.Writer(t.Context(), MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize+1)); err != nil {
		t.Fatal(err)
	}
	if h, _ := peer.recv(); h != opBinary {
		t.Fatalf("first fragment %#x", h)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	if err := w.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("writer close: %v", err)
	}
	if c.isClosed() {
		t.Fatal("the refused fragment ended the closing handshake")
	}
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := peer.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after the Close frame: %d bytes, %v", n, err)
	}
}

// probeWaiter returns the lock that method needs and a call that waits for it.
func probeWaiter(t *testing.T, ctx context.Context, c *Conn, method string) (lock, func() error) {
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
func TestProbeLockWaitHonorsContextAndClose(t *testing.T) {
	methods := []string{"Write", "Writer", "Ping", "Ping frame", "Close", "Read", "Reader", "msgReader", "msgWriter"}
	for _, closeConn := range []bool{false, true} {
		for _, method := range methods {
			t.Run(method+" closed="+strconv.FormatBool(closeConn), func(t *testing.T) {
				srv, _ := tcpPair(t)
				c := newConn(srv, srv, false, "", connConfig{})
				t.Cleanup(func() { _ = c.CloseNow() })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				held, call := probeWaiter(t, ctx, c, method)
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

// gatedConn reports when a Read or Write has entered the transport, holds its
// return until released and counts calls made after the connection was retired.
type gatedConn struct {
	net.Conn

	readEntered, writeEntered   chan struct{}
	readReturned, writeReturned chan struct{}
	readExit, writeExit         chan struct{}
	retired                     atomic.Bool
	late                        atomic.Int64
}

func newGatedConn(nc net.Conn) *gatedConn {
	return &gatedConn{
		Conn: nc, readEntered: make(chan struct{}, 1), writeEntered: make(chan struct{}, 1),
		readReturned: make(chan struct{}, 1), writeReturned: make(chan struct{}, 1),
		readExit: make(chan struct{}), writeExit: make(chan struct{}),
	}
}

func (c *gatedConn) touch(ch chan struct{}) {
	if c.retired.Load() {
		c.late.Add(1)
	}
	signal(ch)
}

func (c *gatedConn) Read(p []byte) (int, error) {
	c.touch(c.readEntered)
	n, err := c.Conn.Read(p)
	c.touch(c.readReturned)
	<-c.readExit
	return n, err
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.touch(c.writeEntered)
	n, err := c.Conn.Write(p)
	c.touch(c.writeReturned)
	<-c.writeExit
	return n, err
}

func (c *gatedConn) Close() error                  { c.touch(nil); return c.Conn.Close() }
func (c *gatedConn) SetDeadline(d time.Time) error { c.touch(nil); return c.Conn.SetDeadline(d) }
func (c *gatedConn) SetReadDeadline(d time.Time) error {
	c.touch(nil)
	return c.Conn.SetReadDeadline(d)
}

func (c *gatedConn) SetWriteDeadline(d time.Time) error {
	c.touch(nil)
	return c.Conn.SetWriteDeadline(d)
}

// shutdown returns only after the in-flight read and write have left the
// transport, and no call made afterwards touches it.
func TestProbeShutdownWaitsForInFlightIO(t *testing.T) {
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
func TestProbeCloseCauseVisibleWithClosedState(t *testing.T) {
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
func TestProbeCloseReadClosePeerCloseStorm(t *testing.T) {
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
func TestProbeShutdownRacesHandlerGoroutines(t *testing.T) {
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

// A Read bounded by its context ends at the deadline although the peer keeps
// the connection busy with pings, whether or not it reads the pongs.
func TestProbeReadDeadlineBoundsPingStream(t *testing.T) {
	for _, readsPongs := range []bool{false, true} {
		t.Run("peer reads pongs "+strconv.FormatBool(readsPongs), func(t *testing.T) {
			srv, cli, _ := pipePair(t)
			c := newConn(srv, srv, false, "", connConfig{})
			t.Cleanup(func() { _ = c.CloseNow() })
			ping := frame(opPing, true, []byte("p"))
			go func() {
				for {
					if _, err := cli.Write(ping); err != nil {
						return
					}
				}
			}()
			if readsPongs {
				go func() { _, _ = io.Copy(io.Discard, cli) }()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, _, err := c.Read(ctx)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				t.Fatalf("read: %v after %v", err, time.Since(start))
			}
		})
	}
}

// A pong satisfies only the ping it answers: duplicates, pongs for an earlier
// ping and foreign pongs do not complete the next one.
func TestProbePingIgnoresStalePongs(t *testing.T) {
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
func TestProbePingOnConnContextReportsCloseReason(t *testing.T) {
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
func TestProbeCloseReadIgnoresReadTimeout(t *testing.T) {
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
func TestProbeDoneContextLeavesConnectionUsable(t *testing.T) {
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

// Discarding the rest of a half-read message validates UTF-8 and the size limit
// across empty fragments and pings; a Close frame in it ends with its status.
func TestProbeDiscardMatrix(t *testing.T) {
	split := frame(opText, false, []byte{'a', 0xe2})
	ping := frame(opPing, true, []byte("p"))
	for _, tc := range []struct {
		name   string
		frames [][]byte
		code   StatusCode
	}{
		{"valid sequence split three ways", [][]byte{
			split, frame(opContinuation, false, nil), ping, frame(opContinuation, false, []byte{0x82}),
			frame(opPong, true, []byte("x")), frame(opContinuation, true, []byte{0xac}),
		}, 0},
		{"invalid continuation byte", [][]byte{
			split, frame(opContinuation, false, nil), ping, frame(opContinuation, true, []byte{'x'}),
		}, StatusInvalidFramePayloadData},
		{"incomplete sequence before an empty final fragment", [][]byte{
			split, ping, frame(opContinuation, true, nil),
		}, StatusInvalidFramePayloadData},
		{"size limit across empty fragments", [][]byte{
			frame(opBinary, false, []byte("ab")), frame(opContinuation, false, nil), ping, frame(opContinuation, true, []byte("cde")),
		}, StatusMessageTooBig},
		{"peer close during the discard", [][]byte{split, ping, frame(opClose, true, []byte{3, 0xe8})}, StatusNormalClosure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{maxMessageSize: 4})
			peer.send(append(tc.frames, frame(opText, true, []byte("next")))...)
			_, r, err := c.Reader(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			_, p, err := c.Read(t.Context())
			if tc.code == 0 && (err != nil || string(p) != "next") {
				t.Fatalf("read: %q %v", p, err)
			}
			if tc.code != 0 && CloseStatus(err) != tc.code {
				t.Fatalf("read: %v, want close %d", err, tc.code)
			}
			if h, p := peer.recv(); h != 0x80|opPong || string(p) != "p" {
				t.Fatalf("pong %#x %q", h, p)
			}
		})
	}
}

// A streaming reader that takes one byte at a time still validates UTF-8
// sequences split across fragments and delivers them intact.
func TestProbeReaderUTF8ByteAtATime(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	peer.send(frame(opText, false, []byte{'a', 0xe2}), frame(opContinuation, false, []byte{0x82}),
		frame(opContinuation, true, []byte{0xac, 'b'}))
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != "a\u20acb" {
		t.Fatalf("got %q", got)
	}
	peer.send(frame(opText, false, []byte{0xe2}), frame(opContinuation, true, []byte{0x82, 'x'}))
	if _, r, err = c.Reader(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); CloseStatus(err) != StatusInvalidFramePayloadData {
		t.Fatalf("invalid split sequence: %v", err)
	}
}

// The size limit is inclusive, counts all fragments of a message and leaves
// control frame payloads out.
func TestProbeMaxMessageSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames [][]byte
		ok     bool
	}{
		{"exactly the limit", [][]byte{frame(opText, true, []byte("12345"))}, true},
		{"one over", [][]byte{frame(opText, true, []byte("123456"))}, false},
		{"fragments at the limit", [][]byte{frame(opText, false, []byte("123")), frame(opContinuation, true, []byte("45"))}, true},
		{"fragments over", [][]byte{frame(opText, false, []byte("123")), frame(opContinuation, true, []byte("456"))}, false},
		{"ping payload not counted", [][]byte{
			frame(opPing, true, bytes.Repeat([]byte("p"), maxControlPayload)), frame(opText, true, []byte("12345")),
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{maxMessageSize: 5})
			peer.send(tc.frames...)
			_, p, err := c.Read(t.Context())
			if tc.ok && (err != nil || string(p) != "12345") {
				t.Fatalf("read: %q %v", p, err)
			}
			if !tc.ok && CloseStatus(err) != StatusMessageTooBig {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

// A message cut short by the peer's EOF, inside a frame or between fragments,
// is reported by the streaming reader as an abnormal closure, not a clean end.
func TestProbeReaderTruncatedByEOF(t *testing.T) {
	whole := frame(opBinary, true, make([]byte, 100))
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"inside a frame", whole[:len(whole)-50]},
		{"between fragments", frame(opBinary, false, make([]byte, 50))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := tcpPair(t)
			c := newConn(srv, srv, false, "", connConfig{})
			t.Cleanup(func() { _ = c.CloseNow() })
			if _, err := cli.Write(tc.wire); err != nil {
				t.Fatal(err)
			}
			cli.Close()
			_, r, err := c.Reader(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			p, err := io.ReadAll(r)
			if CloseStatus(err) != StatusAbnormalClosure || len(p) != 50 {
				t.Fatalf("stream: %d bytes, %v", len(p), err)
			}
			if CloseStatus(context.Cause(c.Context())) != StatusAbnormalClosure {
				t.Fatalf("cause: %v", context.Cause(c.Context()))
			}
		})
	}
}

// A peer that sends its Close frame and then ends the TCP connection is
// reported with its status code, not as an abnormal closure.
func TestProbeCloseFrameThenEOFReportsPeerStatus(t *testing.T) {
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

// Handshake fields are checked as HTTP lists and exact values under both header
// normalization settings.
func TestProbeAcceptHeaderMatrix(t *testing.T) {
	const key = "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	const head = "GET /ws HTTP/1.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
	const noVersion = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
	cases := []struct{ name, req, status string }{
		{"connection close list", noVersion + "Sec-WebSocket-Version: 13\r\n" + strings.Replace(key, "Sec", "Connection: close, Upgrade\r\nSec", 1) + "\r\n", "400"},
		{"connection close line", head + "Host: h\r\nConnection: close\r\n" + key + "\r\n", "400"},
		{"tabs around key", head + "Host: h\r\nSec-WebSocket-Key:\tdGhlIHNhbXBsZSBub25jZQ==\t\r\n\r\n", "101"},
		{"key with inner space", head + "Host: h\r\nSec-WebSocket-Key: dGhlIHNhbXBs ZSBub25jZQ==\r\n\r\n", "400"},
		{"version 013", noVersion + "Sec-WebSocket-Version: 013\r\n" + key + "\r\n", "426"},
		{"empty version", noVersion + "Sec-WebSocket-Version:\r\n" + key + "\r\n", "426"},
		{"extensions offered", head + "Host: h\r\n" + key + "Sec-WebSocket-Extensions: permessage-deflate\r\n\r\n", "101"},
		{"origin with default port", head + "Host: example.com\r\n" + key + "Origin: http://example.com:80\r\n\r\n", "403"},
		{"host with port, origin without", head + "Host: example.com:80\r\n" + key + "Origin: http://example.com\r\n\r\n", "403"},
		{"ipv6 origin case", head + "Host: [2001:db8::a]\r\n" + key + "Origin: HTTP://[2001:DB8::A]\r\n\r\n", "101"},
		{"ipv6 origin port", head + "Host: [::1]:8080\r\n" + key + "Origin: http://[::1]:8080\r\n\r\n", "101"},
		{"ipv6 origin wrong port", head + "Host: [::1]:8080\r\n" + key + "Origin: http://[::1]:8081\r\n\r\n", "403"},
		{"quoted subprotocol", head + "Host: h\r\n" + key + "Sec-WebSocket-Protocol: \"chat\"\r\n\r\n", "400"},
		{"subprotocol list of commas", head + "Host: h\r\n" + key + "Sec-WebSocket-Protocol: ,\r\n\r\n", "400"},
	}
	for _, normalize := range []bool{true, false} {
		ts := startServerWith(t, &AcceptOptions{Subprotocols: []string{"chat"}}, echo,
			func(s *fasthttp.Server) { s.DisableHeaderNamesNormalizing = !normalize })
		for _, tc := range cases {
			if resp := rawHandshake(t, ts.addr, tc.req); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
				t.Fatalf("normalize=%v %s: %q", normalize, tc.name, resp)
			}
		}
	}
}

// A subprotocol name in AcceptOptions that is not an HTTP token is a server
// configuration error, answered with 500 before any negotiation.
func TestProbeAcceptRejectsNonTokenSubprotocolOption(t *testing.T) {
	ts := startServer(t, &AcceptOptions{Subprotocols: []string{"bad name"}}, echo)
	req := "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if resp := rawHandshake(t, ts.addr, req); !strings.HasPrefix(resp, "HTTP/1.1 500 ") {
		t.Fatalf("response: %q", resp)
	}
}

// Dial accepts list-valued Connection fields and mixed-case tokens, keeps frames
// behind the response, and refuses a closing or accept-repeating response.
func TestProbeDialResponseMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, response, payload string
		ok                      bool
	}{
		{"connection close list", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: close, Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", false},
		{"connection keep-alive list", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", true},
		{"mixed case tokens", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: WebSocket\r\nConnection: upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", true},
		{"accept repeated", okHead + "Sec-WebSocket-Accept: %s\r\nSec-WebSocket-Accept: %s\r\n\r\n", "", false},
		{"accept with tabs", okHead + "Sec-WebSocket-Accept:\t%s\t\r\n\r\n", "", true},
		{"frames behind the response", okHead + "Sec-WebSocket-Accept: %s\r\n\r\n" + string(appendFrame(nil, opText, true, false, []byte("hi"))), "hi", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := rawServer(t, func(accept string) string { return strings.ReplaceAll(tc.response, "%s", accept) })
			c, err := Dial(t.Context(), "ws://"+addr+"/", nil)
			if (err == nil) != tc.ok {
				t.Fatalf("dial: %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			if tc.payload == "" {
				return
			}
			if _, p, err := c.Read(t.Context()); err != nil || string(p) != tc.payload {
				t.Fatalf("read: %q %v", p, err)
			}
		})
	}
}

// The handshake request spells the fields as RFC 6455 does, sends one Host (the
// caller's override wins), joins the subprotocols and carries no body framing.
func TestProbeDialRequestWireFormat(t *testing.T) {
	for _, normalized := range []bool{true, false} {
		addr, request := rawServer(t, probeAccept)
		var h fasthttp.RequestHeader
		if !normalized {
			h.DisableNormalizing()
		}
		h.Set("host", "override.example")
		h.Set("x-token", "secret")
		c, err := Dial(t.Context(), "ws://"+addr+"/p?q=1", &DialOptions{Header: &h, Subprotocols: []string{"chat", "echo"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.CloseNow()
		head := <-request
		for _, want := range []string{
			"GET /p?q=1 HTTP/1.1\r\n", "Host: override.example\r\n", "Upgrade: websocket\r\n", "Connection: Upgrade\r\n",
			"Sec-WebSocket-Version: 13\r\n", "Sec-WebSocket-Key: ", "Sec-WebSocket-Protocol: chat, echo\r\n",
		} {
			if !strings.Contains(head, want) {
				t.Fatalf("normalized=%v: request lacks %q:\n%s", normalized, want, head)
			}
		}
		lower := strings.ToLower(head)
		if strings.Count(lower, "host:") != 1 || strings.Contains(lower, "content-length") || strings.Contains(lower, "transfer-encoding") {
			t.Fatalf("normalized=%v: request:\n%s", normalized, head)
		}
	}
}

// The request target is sent as given: a missing path becomes "/", a query
// without a path keeps the query, and escapes and dot segments are untouched.
func TestProbeDialRequestTargets(t *testing.T) {
	for _, tc := range []struct{ suffix, want string }{
		{"", "/"},
		{"?token=abc", "/?token=abc"},
		{"/a%2Fb//c?x=%2F", "/a%2Fb//c?x=%2F"},
		{"/a/../b//%2e?b=2&a=1", "/a/../b//%2e?b=2&a=1"},
	} {
		addr, request := rawServer(t, probeAccept)
		c, err := Dial(t.Context(), "ws://"+addr+tc.suffix, nil)
		if err != nil {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		_ = c.CloseNow()
		if head := <-request; !strings.HasPrefix(head, "GET "+tc.want+" HTTP/1.1\r\n") {
			t.Fatalf("%q: %q", tc.suffix, head)
		}
	}
}

// A streamed message goes out in 4 KiB fragments with FIN on the last one and
// never an extra empty frame, whatever the sizes of the writes that filled it.
func TestProbeWriterFrameBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, writeChunkSize - 1, writeChunkSize, writeChunkSize + 1, 2 * writeChunkSize, 3 * writeChunkSize} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{})
			w, err := c.Writer(t.Context(), MessageBinary)
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte{0x73}, n)
			for i := 0; i < n; i += 137 {
				if _, err := w.Write(want[i:min(i+137, n)]); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.Write(nil); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Write(t.Context(), MessageText, []byte("next")); err != nil {
				t.Fatal(err)
			}
			frames := max(1, (n+writeChunkSize-1)/writeChunkSize)
			got := make([]byte, 0, n)
			for i := range frames {
				h, p := peer.recv()
				wantOp := opContinuation
				if i == 0 {
					wantOp = opBinary
				}
				if i == frames-1 {
					wantOp |= 0x80
				}
				if h != wantOp || (n > 0 && len(p) == 0) {
					t.Fatalf("frame %d: %#x with %d bytes", i, h, len(p))
				}
				got = append(got, p...)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%d bytes delivered", len(got))
			}
			if h, p := peer.recv(); h != 0x80|opText || string(p) != "next" {
				t.Fatalf("after the message: %#x %q", h, p)
			}
		})
	}
}

// Read grows its buffer with the bytes that arrive, not with the announced
// frame length, so a header claiming 4 MiB and no data costs one chunk.
func TestProbeReadGrowsWithDataNotHeader(t *testing.T) {
	srv, cli := tcpPair(t)
	c := newConn(srv, srv, false, "", connConfig{readTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	header := binary.BigEndian.AppendUint64([]byte{0x80 | opBinary, 0x80 | 127}, 4<<20)
	header = append(header, 0, 0, 0, 0) // Masking key.
	if _, err := cli.Write(header); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Read(t.Context()); !isTimeout(err) {
		t.Fatalf("read: %v", err)
	}
	if c.readBuf == nil || len(c.readBuf.b) != readChunkSize || cap(c.readBuf.b) >= 4<<20 {
		t.Fatalf("buffer sized for the announced frame: %v", c.readBuf)
	}
}

// A Writer abandoned mid-message when the connection shuts down still buffers
// an empty write, fails its Close with net.ErrClosed and releases the message lock.
func TestProbeWriterAbandonedAfterShutdown(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	w, err := c.Writer(t.Context(), MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize+1)); err != nil {
		t.Fatal(err)
	}
	if h, _ := peer.recv(); h != opBinary {
		t.Fatalf("fragment %#x", h)
	}
	c.shutdown()
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("empty write: %d %v", n, err)
	}
	for range 2 {
		if err := w.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("writer close: %v", err)
		}
	}
	if len(c.msgMu) != 0 {
		t.Fatal("message lock retained")
	}
	if _, err := c.Writer(t.Context(), MessageText); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("new writer: %v", err)
	}
}

// A failed acquire, whatever ended it, never takes or disturbs the holder's
// token, and wait leaves the lock free.
func TestProbeAcquireFailureKeepsToken(t *testing.T) {
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
			if err := c.acquire(l, ctx, deadline); !errors.Is(err, want) {
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
func TestProbeWatchStaleAbortIgnored(t *testing.T) {
	nc := &deadlineConn{}
	w := ctxWatch{nc: nc}
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
	w.abort(oldGen) // The old context's callback runs late.
	if d, _ := nc.deadline(); !d.Equal(next) {
		t.Fatalf("a stale abort changed the deadline to %v", d)
	}
	w.abort(w.gen)
	if d, _ := nc.deadline(); !d.Equal(aLongTimeAgo) {
		t.Fatalf("the current abort left the deadline at %v", d)
	}
	w.end()
	w.stopWatch()
	_, n := nc.deadline()
	w.abort(w.gen)
	if _, err := w.begin(t.Context(), time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("begin after stop: %v", err)
	}
	if _, m := nc.deadline(); m != n {
		t.Fatal("the stopped watch touched the transport")
	}
}

// pastDeadlineContext reports a deadline that has passed while its timer has
// not fired yet.
type pastDeadlineContext struct {
	context.Context //nolint:containedctx // Overrides Deadline only.
}

func (pastDeadlineContext) Deadline() (time.Time, bool) { return aLongTimeAgo, true }

// fatal reports the context's error first, then an expired deadline, keeps a
// timeout, maps anything else to an abnormal closure; the first reason sticks.
func TestProbeFatalPrecedence(t *testing.T) {
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
func TestProbeCloseReadPongTimeoutClosesConnection(t *testing.T) {
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

// A ping answered while a Writer is between fragments goes out as a pong in
// the middle of the message, and the message then completes normally.
func TestProbePongBetweenWriterFragments(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	w, err := c.Writer(t.Context(), MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize+1)); err != nil {
		t.Fatal(err)
	}
	if h, _ := peer.recv(); h != opBinary {
		t.Fatalf("first fragment %#x", h)
	}
	c.CloseRead(t.Context())
	peer.send(frame(opPing, true, []byte("p")))
	if h, p := peer.recv(); h != 0x80|opPong || string(p) != "p" {
		t.Fatalf("pong %#x %q", h, p)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if h, _ := peer.recv(); h != 0x80|opContinuation {
		t.Fatalf("final fragment %#x", h)
	}
}

// When two Close calls wait for the peer, the one with the earlier deadline
// closes the connection at its deadline and both return nil.
func TestProbeCloseShorterDeadlineEndsBothWaits(t *testing.T) {
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
func TestProbeCloseContextDeadlineOutlivesReadTimeout(t *testing.T) {
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

// Handshake fields set on the response before Accept are replaced, whatever
// their spelling, while other preset headers are kept.
func TestProbeAcceptReplacesHandshakeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		supported, offered  []string
		want                string
		disableNormalizing  bool
	}{
		{name: "protocol without negotiation", header: "Sec-WebSocket-Protocol", value: "chat", offered: []string{"chat"}},
		{name: "protocol replaced by the selected one", header: "Sec-WebSocket-Protocol", value: "stale", supported: []string{"chat"}, offered: []string{"chat"}, want: "chat"},
		{name: "extension", header: "Sec-WebSocket-Extensions", value: "permessage-deflate"},
		{name: "accept spelled differently", header: "sec-websocket-accept", value: "stale", disableNormalizing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverProtocol := make(chan string, 1)
			ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
				s.DisableHeaderNamesNormalizing = tc.disableNormalizing
				s.Handler = func(ctx *fasthttp.RequestCtx) {
					ctx.Response.Header.Set(tc.header, tc.value)
					ctx.Response.Header.Set("X-App", "kept")
					_ = Accept(ctx, &AcceptOptions{Subprotocols: tc.supported}, func(c *Conn) { serverProtocol <- c.Subprotocol() })
				}
			})
			var resp fasthttp.Response
			c := ts.dial(t, &DialOptions{Response: &resp, Subprotocols: tc.offered})
			if got := <-serverProtocol; got != tc.want || c.Subprotocol() != tc.want {
				t.Fatalf("subprotocols: server %q, client %q, want %q", got, c.Subprotocol(), tc.want)
			}
			all := resp.Header.All()
			if _, n := fieldCount(all, fasthttp.HeaderSecWebSocketAccept); n != 1 {
				t.Fatalf("%d Sec-WebSocket-Accept fields", n)
			}
			if _, n := fieldCount(all, fasthttp.HeaderSecWebSocketExtensions); n != 0 {
				t.Fatal("an extension was announced")
			}
			if string(resp.Header.Peek("X-App")) != "kept" {
				t.Fatal("a preset header was dropped")
			}
		})
	}
}

// Dial skips interim 1xx responses before the final one and gives up after a
// few.
func TestProbeDialSkipsInterimResponses(t *testing.T) {
	const hints = "HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\n"
	for _, tc := range []struct {
		name, interim string
		ok            bool
	}{
		{"continue", "HTTP/1.1 100 Continue\r\n\r\n", true},
		{"early hints", hints, true},
		{"five hints", strings.Repeat(hints, 5), true},
		{"six hints", strings.Repeat(hints, 6), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := rawServer(t, func(accept string) string { return tc.interim + probeAccept(accept) })
			var resp fasthttp.Response
			c, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{Response: &resp})
			if (err == nil) != tc.ok {
				t.Fatalf("dial: %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				return
			}
			_ = c.CloseNow()
			if resp.StatusCode() != fasthttp.StatusSwitchingProtocols {
				t.Fatalf("status %d", resp.StatusCode())
			}
		})
	}
}

// A rejected method is answered with 405 and the Allow field.
func TestProbeAccept405CarriesAllow(t *testing.T) {
	ts := startServer(t, nil, echo)
	resp := rawHandshake(t, ts.addr, "POST /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nContent-Length: 0\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 405 ") || !strings.Contains(resp, "\r\nAllow: GET\r\n") {
		t.Fatalf("response: %q", resp)
	}
}

// A Sec-WebSocket-Protocol field with no name, or with a name twice, fails the
// handshake; empty list items between names are tolerated.
func TestProbeAcceptRejectsEmptyOrRepeatedSubprotocols(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	cases := []struct{ offer, status string }{
		{"Sec-WebSocket-Protocol:\r\n", "400"},
		{"Sec-WebSocket-Protocol: , ,\r\n", "400"},
		{"Sec-WebSocket-Protocol: chat, chat\r\n", "400"},
		{"Sec-WebSocket-Protocol: chat\r\nsec-websocket-protocol: chat\r\n", "400"},
		{"Sec-WebSocket-Protocol: , chat,\r\n", "101"},
		{"Sec-WebSocket-Protocol: chat\r\nSec-WebSocket-Protocol: echo\r\n", "101"},
	}
	for _, normalize := range []bool{true, false} {
		ts := startServerWith(t, &AcceptOptions{Subprotocols: []string{"chat"}}, echo,
			func(s *fasthttp.Server) { s.DisableHeaderNamesNormalizing = !normalize })
		for _, tc := range cases {
			if resp := rawHandshake(t, ts.addr, head+tc.offer+"\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
				t.Fatalf("normalize=%v %q: %q", normalize, tc.offer, resp)
			}
		}
	}
}

// Offered extensions must follow the RFC 6455 syntax, quoted values included,
// although none is selected.
func TestProbeAcceptChecksExtensionSyntax(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Extensions: "
	ts := startServer(t, nil, echo)
	for _, tc := range []struct{ offer, status string }{
		{"", "400"},
		{";", "400"},
		{"permessage-deflate;", "400"},
		{"permessage-deflate; client_max_window_bits=", "400"},
		{"permessage-deflate; p=\"open", "400"},
		{"permessage-deflate; =15", "400"},
		{"permessage-deflate; p=\"a\\\"", "400"},
		{"permessage-deflate; p=\"\x01\"", "400"},
		{"permessage-deflate; client_max_window_bits=15; server_no_context_takeover", "101"},
		{"permessage-deflate; client_max_window_bits = 15 , foo; bar=\"a\\b\"", "101"},
		{"foo; bar=\"a;b\"", "400"},
		{"foo; bar=\"a,b\"", "400"},
		{", permessage-deflate,", "101"},
	} {
		resp := rawHandshake(t, ts.addr, head+tc.offer+"\r\n\r\n")
		if !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") || strings.Contains(strings.ToLower(resp), "sec-websocket-extensions") {
			t.Fatalf("%q: %q", tc.offer, resp)
		}
	}
}

// A handshake field preset under a spelling Accept cannot remove is a server
// error rather than a 101 with two of them.
func TestProbeAcceptRefusesUnreachableHandshakeHeader(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.DisableNormalizing()
			h.Set("sec-websocket-accept", "stale")
			h.EnableNormalizing()
			if err := Accept(ctx, nil, func(*Conn) {}); err == nil {
				t.Error("Accept succeeded")
			}
		}
	})
	resp := rawHandshake(t, ts.addr, "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 500 ") {
		t.Fatalf("response: %q", resp)
	}
}

// A handshake field that deleting the Trailer declaration brings back into
// the headers is removed as well.
func TestProbeAcceptRemovesHeaderRestoredFromTrailer(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			_ = h.SetTrailer(fasthttp.HeaderSecWebSocketExtensions)
			h.Set(fasthttp.HeaderSecWebSocketExtensions, "permessage-deflate")
			_ = Accept(ctx, nil, func(*Conn) {})
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if _, n := fieldCount(resp.Header.All(), fasthttp.HeaderSecWebSocketExtensions); n != 0 {
		t.Fatal("an extension was announced")
	}
}

// With header-name normalization off, preset handshake fields are removed, a
// preset Connection: close does not stop Accept, and later header changes apply.
func TestProbeAcceptCleansPresetHeadersWithoutNormalization(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.DisableHeaderNamesNormalizing = true
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.Set("connection", "close, keep-alive")
			h.Set("sec-websocket-extensions", "permessage-deflate")
			h.Set("x-middleware", "before")
			if err := Accept(ctx, nil, func(*Conn) {}); err != nil {
				t.Error(err)
				return
			}
			h.Set("x-middleware", "after")
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if v, n := fieldCount(resp.Header.All(), "X-Middleware"); n != 1 || string(v) != "after" {
		t.Fatalf("X-Middleware: %d fields, last %q", n, v)
	}
}

// A Trailer declaration naming handshake fields would divert them into the
// trailer section; Accept drops it.
func TestProbeAcceptDropsPresetTrailer(t *testing.T) {
	serverProtocol := make(chan string, 1)
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set(fasthttp.HeaderTrailer, "Sec-WebSocket-Accept, Sec-WebSocket-Protocol")
			_ = Accept(ctx, &AcceptOptions{Subprotocols: []string{"chat"}}, func(c *Conn) { serverProtocol <- c.Subprotocol() })
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp, Subprotocols: []string{"chat"}})
	_ = c.CloseNow()
	if got := <-serverProtocol; got != "chat" || c.Subprotocol() != "chat" {
		t.Fatalf("subprotocols: server %q, client %q", got, c.Subprotocol())
	}
	if _, n := fieldCount(resp.Header.All(), fasthttp.HeaderTrailer); n != 0 {
		t.Fatal("Trailer was sent")
	}
}

// A quoted extension parameter value must unescape to a token.
func TestProbeAcceptQuotedExtensionValuesAreTokens(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits="
	ts := startServer(t, nil, echo)
	for _, tc := range []struct{ value, status string }{
		{`""`, "400"},
		{`"1 5"`, "400"},
		{`"1;5"`, "400"},
		{`"a\"b"`, "400"},
		{"\"\x80\"", "400"},
		{`"15"`, "101"},
		{`"\1\5"`, "101"},
	} {
		if resp := rawHandshake(t, ts.addr, head+tc.value+"\r\n\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
			t.Fatalf("%s: %q", tc.value, resp)
		}
	}
}

// The response's Upgrade field must name websocket and nothing else.
func TestProbeDialRejectsExtraUpgradeProtocol(t *testing.T) {
	for _, tc := range []struct {
		upgrade string
		ok      bool
	}{
		{"Upgrade: websocket, h2c\r\n", false},
		{"Upgrade: websocket\r\nUpgrade: h2c\r\n", false},
		{"Upgrade: websocket\r\nUpgrade: WebSocket\r\n", true},
		{"Upgrade: , websocket,\r\n", true},
	} {
		addr, _ := rawServer(t, func(accept string) string {
			return "HTTP/1.1 101 Switching Protocols\r\n" + tc.upgrade + "Connection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
		})
		c, err := Dial(t.Context(), "ws://"+addr+"/", nil)
		if (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.upgrade, err)
		}
		if c != nil {
			_ = c.CloseNow()
		}
	}
}

// A path or query that cannot be sent as given fails before the connection
// is made.
func TestProbeDialRejectsInvalidRequestTarget(t *testing.T) {
	for _, tc := range []struct {
		suffix string
		ok     bool
	}{
		{"/a b", false},
		{"/bad%zz", false},
		{"/short%4", false},
		{"/café", false},
		{"/a\"b", false},
		{"/a{b}", false},
		{"?x=\x7f", false}, // Refused by the URI parser already.
		{"/a%2Fb/~c!$&'()*+,;=:@?q[]=1&r=%41", true},
		{"", true},
	} {
		addr, _ := rawServer(t, probeAccept)
		c, err := Dial(t.Context(), "ws://"+addr+tc.suffix, nil)
		if tc.ok && err != nil {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(strings.ToLower(err.Error()), "invalid ur")) {
			t.Fatalf("%q: %v", tc.suffix, err)
		}
		if c != nil {
			_ = c.CloseNow()
		}
	}
}

// A writer handle that failed or was closed is inert: closing it again does
// not touch the message another goroutine's writer is sending.
func TestProbeStaleWriterCloseIsInert(t *testing.T) {
	for _, failed := range []bool{true, false} {
		t.Run("failed="+strconv.FormatBool(failed), func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{})
			ctxA, cancelA := context.WithCancel(t.Context())
			defer cancelA()
			wA, err := c.Writer(ctxA, MessageBinary)
			if err != nil {
				t.Fatal(err)
			}
			if failed {
				cancelA()
				if _, err := wA.Write(make([]byte, writeChunkSize+1)); !errors.Is(err, context.Canceled) {
					t.Fatalf("A.Write: %v", err)
				}
			} else {
				if _, err := wA.Write([]byte("A")); err != nil {
					t.Fatal(err)
				}
				if err := wA.Close(); err != nil {
					t.Fatal(err)
				}
				if h, p := peer.recv(); h != 0x80|opBinary || string(p) != "A" {
					t.Fatalf("A's message: %#x %q", h, p)
				}
			}
			wB, err := c.Writer(t.Context(), MessageText)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wB.Write([]byte("hello ")); err != nil {
				t.Fatal(err)
			}
			if err := wA.Close(); failed && !errors.Is(err, context.Canceled) || !failed && err != nil {
				t.Fatalf("stale Close: %v", err)
			}
			if _, err := wB.Write([]byte("world")); err != nil {
				t.Fatalf("B.Write: %v", err)
			}
			if err := wB.Close(); err != nil {
				t.Fatalf("B.Close: %v", err)
			}
			if h, p := peer.recv(); h != 0x80|opText || string(p) != "hello world" {
				t.Fatalf("B's message: %#x %q", h, p)
			}
		})
	}
}

// Accept leaves the response header's name normalization as it found it, so
// a handler that changes headers after Accept still replaces and removes them.
func TestProbeAcceptKeepsHeaderNormalization(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.Set("x-update", "before")
			h.Set("x-remove", "discard")
			if err := Accept(ctx, nil, func(*Conn) {}); err != nil {
				t.Error(err)
				return
			}
			h.Set("x-update", "after")
			h.Del("x-remove")
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if v, n := fieldCount(resp.Header.All(), "X-Update"); n != 1 || string(v) != "after" {
		t.Fatalf("X-Update: %d fields, last %q", n, v)
	}
	if _, n := fieldCount(resp.Header.All(), "X-Remove"); n != 0 {
		t.Fatal("X-Remove was sent")
	}
}

// writeAbortConn reports a write deadline set to the past, as an abort does.
type writeAbortConn struct {
	net.Conn

	aborted chan struct{}
}

func (c *writeAbortConn) SetWriteDeadline(d time.Time) error {
	err := c.Conn.SetWriteDeadline(d)
	if d.Equal(aLongTimeAgo) {
		signal(c.aborted)
	}
	return err
}

// A read cancelled while its pong goes out aborts the write too; the next write
// with the same deadline arms the socket again rather than trusting its cache.
func TestProbeAbortedReplyRearmsWriteDeadline(t *testing.T) {
	srv, peer, _ := pipePair(t)
	gate := newGatedConn(srv)
	nc := &writeAbortConn{Conn: gate, aborted: make(chan struct{}, 1)}
	input := append(frame(opPing, true, []byte("p")), frame(opBinary, true, []byte("data"))...)
	c := newConn(nc, bytes.NewReader(input), false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	deadline := time.Now().Add(4 * time.Second)
	writeCtx, cancelWrite := context.WithDeadline(t.Context(), deadline)
	defer cancelWrite()
	readCtx, cancelRead := context.WithCancel(writeCtx)
	defer cancelRead()
	readDone := make(chan error, 1)
	go func() {
		_, p, err := c.Read(readCtx)
		if err == nil && string(p) != "data" {
			err = errors.New("wrong message")
		}
		readDone <- err
	}()
	_ = peer.SetReadDeadline(deadline)
	var pong [3]byte
	if _, err := io.ReadFull(peer, pong[:]); err != nil || !bytes.Equal(pong[:], []byte{0x8a, 1, 'p'}) {
		t.Fatalf("pong: %x, %v", pong, err)
	}
	<-gate.writeReturned // The pong is sent; its Write has not returned yet.
	cancelRead()
	select {
	case <-nc.aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("the write deadline was not aborted")
	}
	close(gate.writeExit)
	if err := <-readDone; err != nil || c.isClosed() {
		t.Fatalf("buffered read: %v, closed=%v", err, c.isClosed())
	}
	peerDone := make(chan error, 1)
	go func() {
		var b [3]byte
		_, err := io.ReadFull(peer, b[:])
		peerDone <- err
	}()
	close(gate.readExit)
	if err := c.Write(writeCtx, MessageBinary, []byte("x")); err != nil || c.isClosed() {
		t.Fatalf("write after the mirrored abort: %v, closed=%v", err, c.isClosed())
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

// A vectored write leaves no reference to the caller's payload behind,
// whether it succeeded or failed.
func TestProbeVectoredWriteReleasesPayload(t *testing.T) {
	srv, peer := tcpPair(t)
	c := newConn(hijackLike{Conn: srv}, srv, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	if c.vw != srv {
		t.Fatal("the hijacked conn's socket is not the vectored writer")
	}
	payload := bytes.Repeat([]byte{'v'}, vectorMin)
	wire := make([]byte, 4+len(payload))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(peer, wire); done <- err }()
	if err := c.Write(t.Context(), MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire[:4], []byte{0x82, 126, 0x10, 0}) || !bytes.Equal(wire[4:], payload) {
		t.Fatal("wrong frame on the wire")
	}
	if c.iovBuf[0] != nil || c.iovBuf[1] != nil {
		t.Fatal("a successful vectored write retained a slice")
	}
	// An expired deadline fails the write before any byte goes out.
	err := c.Write(pastDeadlineContext{t.Context()}, MessageBinary, payload)
	if !errors.Is(err, context.DeadlineExceeded) || !c.isClosed() {
		t.Fatalf("write: %v, closed=%v", err, c.isClosed())
	}
	if c.iovBuf[1] != nil {
		t.Fatal("a failed vectored write retained the payload")
	}
}

// Read grows its buffer across every size class boundary, including beyond
// the largest pooled class, and the returned bytes survive CloseNow.
func TestProbeReadGrowthBoundaries(t *testing.T) {
	for _, n := range []int{0, 4095, 4096, 4097, 65535, 65536, 65537, (64 << 20) + 1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			var h []byte
			switch {
			case n < 126:
				h = []byte{0x82, byte(n)}
			case n < 65536:
				h = binary.BigEndian.AppendUint16([]byte{0x82, 126}, uint16(n)) // #nosec G115
			default:
				h = binary.BigEndian.AppendUint64([]byte{0x82, 127}, uint64(n)) // #nosec G115
			}
			r := io.MultiReader(bytes.NewReader(h), io.LimitReader(&repeatReader{b: bytes.Repeat([]byte{'r'}, 4096)}, int64(n)))
			c := newConn(&fuzzConn{}, r, true, "", connConfig{maxMessageSize: max(n, 1)})
			typ, p, err := c.Read(t.Context())
			if err != nil || typ != MessageBinary || len(p) != n || bytes.Count(p, []byte{'r'}) != n {
				t.Fatalf("read: type %v, %d bytes, %v", typ, len(p), err)
			}
			_ = c.CloseNow()
			if bytes.Count(p, []byte{'r'}) != n {
				t.Fatal("CloseNow invalidated the returned bytes")
			}
		})
	}
}

// A 426 names the protocol it requires, as RFC 9110 demands.
func TestProbeAccept426CarriesUpgrade(t *testing.T) {
	ts := startServer(t, nil, echo)
	resp := strings.ToLower(rawHandshake(t, ts.addr, "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 12\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	for _, want := range []string{"http/1.1 426 ", "\r\nupgrade: websocket\r\n", "\r\nconnection: upgrade\r\n", "\r\nsec-websocket-version: 13\r\n"} {
		if !strings.Contains(resp, want) {
			t.Fatalf("response lacks %q: %q", want, resp)
		}
	}
}

// Once the Close frame is out, a later Close with its own deadline ends the
// wait at that deadline even while a control reply holds the frame lock.
func TestProbeSecondCloseDeadlineEndsWait(t *testing.T) {
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

// A handshake request with a body is refused; a chunked body the server has
// already consumed as empty is no body.
func TestProbeAcceptRejectsBody(t *testing.T) {
	const head = "GET /ws HTTP/1.1\r\nHost: h\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for _, tc := range []struct {
		body, status string
		stream       bool
	}{
		{"Content-Length: 5\r\n\r\nhello", "400", false},
		{"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "101", false},
		{"Content-Length: 5\r\n\r\nhello", "400", true},
		{"Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "400", true},
		{"\r\n", "101", true},
		{"Content-Length: 0\r\n\r\n", "101", true},
		{"Content-Length: 0\r\n\r\n", "101", false},
	} {
		ts := startServerWith(t, nil, echo, func(s *fasthttp.Server) { s.StreamRequestBody = tc.stream })
		if resp := rawHandshake(t, ts.addr, head+tc.body); !strings.HasPrefix(resp, "HTTP/1.1 "+tc.status+" ") {
			t.Fatalf("stream=%v %q: %q", tc.stream, tc.body, resp)
		}
	}
}

// The 101 is an HTTP/1.1 response whatever protocol the response was set to
// before Accept.
func TestProbeAcceptSetsHTTP11(t *testing.T) {
	ts := startServerWith(t, nil, nil, func(s *fasthttp.Server) {
		s.Handler = func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.SetProtocol([]byte("HTTP/1.0"))
			_ = Accept(ctx, nil, func(*Conn) {})
		}
	})
	var resp fasthttp.Response
	c := ts.dial(t, &DialOptions{Response: &resp})
	_ = c.CloseNow()
	if p := string(resp.Header.Protocol()); p != "HTTP/1.1" {
		t.Fatalf("protocol %q", p)
	}
}

// After the peer closed the connection, Ping reports the close reason like
// Read and Write do.
func TestProbePingAfterPeerCloseReportsReason(t *testing.T) {
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

// readPlainFrame reads one frame a server sent, which carries neither RSV
// bits nor a mask, and returns its opcode, FIN flag and payload.
func readPlainFrame(r io.Reader) (op byte, fin bool, payload []byte, err error) {
	h, payload, err := readFrame(r)
	if err != nil {
		return 0, false, nil, err
	}
	if h[0]&0x70 != 0 {
		return 0, false, nil, fmt.Errorf("rsv bits set: %#x", h[0])
	}
	if h[1]&0x80 != 0 {
		return 0, false, nil, errors.New("server frame is masked")
	}
	return h[0] & 0x0f, h[0]&0x80 != 0, payload, nil
}

// probePattern fills p so that every byte follows from the first one.
func probePattern(p []byte, tag byte) {
	for i := range p {
		p[i] = tag + byte(i)
	}
}

func patternPayload(n int) []byte {
	b := make([]byte, n)
	probePattern(b, byte(n))
	return b
}

func checkPattern(p []byte) error {
	for i := range p {
		if p[i] != p[0]+byte(i) {
			return fmt.Errorf("byte %d of %d is %#x, want %#x", i, len(p), p[i], p[0]+byte(i))
		}
	}
	return nil
}

// Vectored writes, Writer fragments and pings in both directions share one
// socket: every frame is whole, fragments are in order, messages arrive intact.
func TestProbeVectoredWriteInterleavesControl(t *testing.T) {
	srv, cli := tcpPair(t)
	nc := hijackLike{Conn: srv}
	c := newConn(nc, nc, false, "", connConfig{})
	t.Cleanup(func() { _ = c.CloseNow() })
	if c.vw == nil {
		t.Fatal("no vectored writer for a TCP socket")
	}
	const vectored, streamed, pings = 40, 40, 40
	ctx := t.Context()

	// Reader loop so pongs and the peer's pings are handled.
	readDone := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				readDone <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Go(func() {
		buf := make([]byte, 8192)
		for i := range vectored {
			probePattern(buf, byte(i))
			if err := c.Write(ctx, MessageBinary, buf); err != nil {
				t.Errorf("vectored write %d: %v", i, err)
				return
			}
		}
	})
	wg.Go(func() {
		for i := range streamed {
			w, err := c.Writer(ctx, MessageBinary)
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			msg := make([]byte, 10000)
			probePattern(msg, byte(100+i))
			for _, chunk := range [][]byte{msg[:3000], msg[3000:7000], msg[7000:]} {
				if _, err := w.Write(chunk); err != nil {
					t.Errorf("writer %d write: %v", i, err)
					return
				}
			}
			if err := w.Close(); err != nil {
				t.Errorf("writer %d close: %v", i, err)
				return
			}
		}
	})
	wg.Go(func() {
		for i := range pings {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				t.Errorf("ping %d: %v", i, err)
				return
			}
		}
	})

	// The peer: answers pings, sends its own, verifies everything it sees.
	br := bufio.NewReaderSize(cli, 1<<16)
	var peerMu sync.Mutex
	peerWrite := func(b []byte) {
		peerMu.Lock()
		defer peerMu.Unlock()
		_, _ = cli.Write(b)
	}
	sentPings := make(chan []byte, pings)
	go func() {
		for i := range pings {
			p := fmt.Appendf(nil, "peer-ping-%d", i)
			sentPings <- p
			peerWrite(frame(opPing, true, p))
			time.Sleep(time.Millisecond)
		}
	}()
	var (
		messages, pongs, serverPings int
		inMsg                        bool
		msg                          []byte
	)
	_ = cli.SetReadDeadline(time.Now().Add(20 * time.Second))
	closed := false
	for !closed {
		op, fin, payload, err := readPlainFrame(br)
		if err != nil {
			t.Fatalf("after %d messages, %d pongs, %d pings: %v", messages, pongs, serverPings, err)
		}
		switch op {
		case opBinary, opText, opContinuation:
			if (op == opContinuation) != inMsg {
				t.Fatalf("frame op %d while inMsg=%v", op, inMsg)
			}
			msg = append(msg, payload...)
			inMsg = !fin
			if fin {
				if len(msg) != 8192 && len(msg) != 10000 {
					t.Fatalf("message of %d bytes", len(msg))
				}
				if err := checkPattern(msg); err != nil {
					t.Fatalf("message %d corrupted: %v", messages, err)
				}
				messages++
				msg = msg[:0]
			}
		case opPing:
			if !fin || len(payload) != 8 {
				t.Fatalf("bad server ping fin=%v len=%d", fin, len(payload))
			}
			serverPings++
			peerWrite(frame(opPong, true, payload))
		case opPong:
			want := <-sentPings
			if !bytes.Equal(payload, want) {
				t.Fatalf("pong %q, want %q", payload, want)
			}
			pongs++
		case opClose:
			closed = true
			peerWrite(frame(opClose, true, payload))
		default:
			t.Fatalf("reserved opcode %d", op)
		}
		if !closed && messages == vectored+streamed && pongs == pings && serverPings == pings {
			wg.Wait()
			go func() {
				cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = c.Close(cctx, StatusNormalClosure, "done")
			}()
		}
	}
	wg.Wait()
	select {
	case err := <-readDone:
		if CloseStatus(err) != StatusNormalClosure {
			t.Fatalf("reader ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not end")
	}
	if c.iovBuf[0] != nil || c.iovBuf[1] != nil {
		t.Fatal("iovBuf still references a payload")
	}
}

// A slice handed out by Read stays intact while other connections churn through
// the shared pools and while this connection's own Close drains the peer.
func TestProbePooledBufferIsolation(t *testing.T) {
	const n, size = 8, 3000
	type pair struct{ s, c *Conn }
	pairs := make([]pair, n)
	for i := range pairs {
		srv, cli := tcpPair(t)
		s := newConn(srv, srv, false, "", connConfig{})
		c := newConn(cli, bufio.NewReader(cli), true, "", connConfig{})
		t.Cleanup(func() { _ = s.CloseNow(); _ = c.CloseNow() })
		pairs[i] = pair{s, c}
	}
	ctx := t.Context()
	held := make([][]byte, n)
	for i, p := range pairs {
		if err := p.c.Write(ctx, MessageBinary, bytes.Repeat([]byte{byte(i + 1)}, size)); err != nil {
			t.Fatal(err)
		}
		_, b, err := p.s.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held[i] = b
	}
	check := func(stage string) {
		for i := range pairs {
			if want := bytes.Repeat([]byte{byte(i + 1)}, size); !bytes.Equal(held[i], want) {
				t.Fatalf("%s: slice of connection %d changed", stage, i)
			}
		}
	}
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Go(func() {
			for range 30 {
				if err := p.s.Write(ctx, MessageBinary, make([]byte, size)); err != nil {
					t.Error(err)
					return
				}
				w, err := p.s.Writer(ctx, MessageBinary)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = w.Write(make([]byte, 5000))
				if err := w.Close(); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Go(func() {
			for range 60 {
				if _, _, err := p.c.Read(ctx); err != nil {
					t.Error(err)
					return
				}
			}
			for range 30 {
				if err := p.c.Write(ctx, MessageBinary, make([]byte, size)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	check("after churn")
	// Close from another goroutine drains the 30 unread messages per
	// connection and waits for the Close frame the client answers with.
	for _, p := range pairs {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := p.s.Close(cctx, StatusNormalClosure, ""); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			for {
				if _, _, err := p.c.Read(ctx); err != nil {
					if CloseStatus(err) != StatusNormalClosure {
						t.Error(err)
					}
					return
				}
			}
		})
	}
	wg.Wait()
	check("after close")
}

// Nothing but the peer's reply may follow the Close frame; Ping is refused.
func TestProbePingAfterCloseFrame(t *testing.T) {
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

// A reader abandoned with a cancelled context does not close the connection;
// the next Read discards what is left of that message, still validating it.
func TestProbeReaderAbandonedThenRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		rest []byte
		code StatusCode
	}{
		{"valid", []byte("cd"), -1},
		{"invalid utf-8 in the discarded part", []byte{0xff}, StatusInvalidFramePayloadData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{})
			peer.send(frame(opText, false, []byte("ab")), frame(opContinuation, true, tc.rest), frame(opBinary, true, []byte("B")))
			rctx, cancel := context.WithCancel(t.Context())
			typ, r, err := c.Reader(rctx)
			if err != nil || typ != MessageText {
				t.Fatalf("reader: %v %v", typ, err)
			}
			var one [1]byte
			if n, err := r.Read(one[:]); n != 1 || err != nil || one[0] != 'a' {
				t.Fatalf("first byte: %d %v %q", n, err, one[:n])
			}
			cancel()
			if _, err := r.Read(one[:]); !errors.Is(err, context.Canceled) {
				t.Fatalf("read after cancel: %v", err)
			}
			if c.isClosed() {
				t.Fatal("the connection was closed by a call made with a done context")
			}
			typ, p, err := c.Read(t.Context())
			if tc.code >= 0 {
				if CloseStatus(err) != tc.code {
					t.Fatalf("next read: %v, want %d", err, tc.code)
				}
				peer.expectClose(tc.code)
				return
			}
			if err != nil || typ != MessageBinary || string(p) != "B" {
				t.Fatalf("next read: %v %q %v", typ, p, err)
			}
			if _, err := r.Read(one[:]); !errors.Is(err, errReaderStale) {
				t.Fatalf("stale reader: %v", err)
			}
		})
	}
}

// A multi-byte sequence split by the 64 KiB read chunk inside one frame.
func TestProbeUTF8AtReadChunkBoundary(t *testing.T) {
	prefix := bytes.Repeat([]byte("a"), readChunkSize-2)
	for _, tc := range []struct {
		name string
		tail []byte
		code StatusCode
	}{
		{"valid euro sign", []byte{0xe2, 0x82, 0xac}, -1},
		{"valid 4-byte", []byte{0xf0, 0x9f, 0x98, 0x80}, -1},
		{"broken after the boundary", []byte{0xe2, 0x82, 0x41}, StatusInvalidFramePayloadData},
		{"truncated", []byte{0xe2, 0x82}, StatusInvalidFramePayloadData},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{maxMessageSize: 1 << 20})
			payload := append(append([]byte{}, prefix...), tc.tail...)
			go func() { _, _ = peer.Write(frame(opText, true, payload)) }()
			_, p, err := c.Read(t.Context())
			if tc.code >= 0 {
				if CloseStatus(err) != tc.code {
					t.Fatalf("read: %v, want %d", err, tc.code)
				}
				return
			}
			if err != nil || !bytes.Equal(p, payload) {
				t.Fatalf("read: %d bytes, %v", len(p), err)
			}
		})
	}
}

// CloseNow while a vectored write is blocked on a peer that does not read:
// the deadline on the hijacked conn must reach the socket the writev uses.
func TestProbeCloseNowDuringVectoredWrite(t *testing.T) {
	srv, cli := tcpPair(t)
	// Small socket buffers keep the write from completing on platforms that
	// buffer generously, Windows among them.
	for _, conn := range []net.Conn{srv, cli} {
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetWriteBuffer(4096)
			_ = tc.SetReadBuffer(4096)
		}
	}
	nc := hijackLike{Conn: srv}
	c := newConn(nc, nc, false, "", connConfig{})
	if c.vw == nil {
		t.Fatal("no vectored writer")
	}
	done := make(chan error, 1)
	go func() { done <- c.Write(context.Background(), MessageBinary, make([]byte, 64<<20)) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Skipf("the write completed before CloseNow: %v", err)
	default:
	}
	start := time.Now()
	_ = c.CloseNow()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("write took %v to fail", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the vectored write did not return")
	}
	if c.iovBuf[0] != nil || c.iovBuf[1] != nil {
		t.Fatal("iovBuf still references the payload")
	}
}

// probeDeadlineConn records the read and write deadlines actually set.
type probeDeadlineConn struct {
	net.Conn

	mu          sync.Mutex
	read, write time.Time
}

func (c *probeDeadlineConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.read = d
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(d)
}

func (c *probeDeadlineConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.write = d
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(d)
}

func (c *probeDeadlineConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.read, c.write = d, d
	c.mu.Unlock()
	return c.Conn.SetDeadline(d)
}

// The deadline cache agrees with the socket after every call while the
// connection stays usable, contexts expiring between calls included.
func TestProbeDeadlineCacheAgreesWithSocket(t *testing.T) {
	srv, cli := tcpPair(t)
	dc := &probeDeadlineConn{Conn: srv}
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

// A fragmented message that crosses several pool classes arrives intact.
func TestProbeLargeFragmentedRead(t *testing.T) {
	c, peer := newRawServer(t, connConfig{maxMessageSize: 1 << 20})
	msg := patternPayload(300<<10 + 7)
	sizes := []int{1, 4095, 4096, 60000, 70000, 131072, 1}
	frames := make([][]byte, 0, len(sizes)+1)
	off := 0
	for i, n := range sizes {
		op := opContinuation
		if i == 0 {
			op = opBinary
		}
		frames = append(frames, frame(op, false, msg[off:off+n]))
		off += n
	}
	frames = append(frames, frame(opContinuation, true, msg[off:]))
	go func() { _, _ = peer.Write(bytes.Join(frames, nil)) }()
	typ, p, err := c.Read(t.Context())
	if err != nil || typ != MessageBinary || !bytes.Equal(p, msg) {
		t.Fatalf("read: %v %d bytes %v", typ, len(p), err)
	}
	if cap(p) != 512<<10 {
		t.Logf("final buffer capacity %d", cap(p))
	}
}

// A frame whose extended length is not minimal is a protocol error even when
// the peer stops right after it; one cut short before its key is an abnormal
// closure.
func TestProbeTruncatedExtendedLength(t *testing.T) {
	for _, tc := range []struct {
		name string
		head []byte
		code StatusCode
	}{
		{"16-bit length of 1", []byte{0x82, 0xfe, 0, 1}, StatusProtocolError},
		{"64-bit length of 256", []byte{0x82, 0xff, 0, 0, 0, 0, 0, 0, 1, 0}, StatusProtocolError},
		{"64-bit length with the top bit set", []byte{0x82, 0xff, 0x80, 0, 0, 0, 0, 0, 0, 0}, StatusProtocolError},
		{"16-bit length of 126 without a key", []byte{0x82, 0xfe, 0, 126}, StatusAbnormalClosure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := tcpPair(t)
			c := newConn(srv, srv, false, "", connConfig{})
			t.Cleanup(func() { _ = c.CloseNow() })
			if _, err := cli.Write(tc.head); err != nil {
				t.Fatal(err)
			}
			if hc, ok := cli.(interface{ CloseWrite() error }); ok {
				_ = hc.CloseWrite()
			}
			if _, _, err := c.Read(t.Context()); CloseStatus(err) != tc.code {
				t.Fatalf("read: %v", err)
			}
			if tc.code != StatusProtocolError {
				return
			}
			_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
			var h [2]byte
			if _, err := io.ReadFull(cli, h[:]); err != nil || h[0] != 0x80|opClose {
				t.Fatalf("close reply: %x, %v", h, err)
			}
		})
	}
}

// After the Close frame went out, a Write with a done context reports the
// context's error rather than the refusal, and leaves the handshake running.
func TestProbeCanceledWriteAfterCloseFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Write(ctx, MessageBinary, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write: %v", err)
	}
	if c.isClosed() {
		t.Fatal("the cancelled write closed the connection")
	}
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A Writer whose context ends after the Close frame went out is refused with
// net.ErrClosed, like any other write, and does not end the closing handshake.
func TestProbeCanceledWriterAfterCloseFrame(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w, err := c.Writer(ctx, MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, writeChunkSize+1)); err != nil {
		t.Fatal(err)
	}
	if h, _ := peer.recv(); h != opBinary {
		t.Fatalf("first fragment %#x", h)
	}
	done := make(chan error, 1)
	go func() { done <- c.Close(t.Context(), StatusNormalClosure, "") }()
	peer.expectClose(StatusNormalClosure)
	cancel()
	if err := w.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("writer close: %v", err)
	}
	if c.isClosed() {
		t.Fatal("the cancelled writer ended the closing handshake")
	}
	peer.send(frame(opClose, true, []byte{3, 0xe8}))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
		t.Fatalf("cause: %v", context.Cause(c.Context()))
	}
}
