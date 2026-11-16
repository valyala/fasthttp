package websocket

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

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

func TestMaskBytes(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	for n := range 40 {
		for pos := range 4 {
			b := make([]byte, n)
			for i := range b {
				b[i] = byte(i)
			}
			want := make([]byte, n)
			for i := range want {
				want[i] = b[i] ^ key[(pos+i)&3]
			}
			got := maskBytes(key, pos, b)
			if !bytes.Equal(b, want) || got != (pos+n)&3 {
				t.Fatalf("n=%d pos=%d: %v (pos %d)", n, pos, b, got)
			}
		}
	}
}

// A Writer whose fragment went out before the Close frame is refused with
// net.ErrClosed when it flushes afterwards and must not end the handshake.
func TestWriterMidMessageRefusedAfterCloseFrame(t *testing.T) {
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

// A streamed message goes out in 4 KiB fragments with FIN on the last one and
// never an extra empty frame, whatever the sizes of the writes that filled it.
func TestWriterFrameBoundaries(t *testing.T) {
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

// A Writer abandoned mid-message when the connection shuts down still buffers
// an empty write, fails its Close with net.ErrClosed and releases the message lock.
func TestWriterAbandonedAfterShutdown(t *testing.T) {
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

// A ping answered while a Writer is between fragments goes out as a pong in
// the middle of the message, and the message then completes normally.
func TestPongBetweenWriterFragments(t *testing.T) {
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

// A writer handle that failed or was closed is inert: closing it again does
// not touch the message another goroutine's writer is sending.
func TestStaleWriterCloseIsInert(t *testing.T) {
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
func TestAbortedReplyRearmsWriteDeadline(t *testing.T) {
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
func TestVectoredWriteReleasesPayload(t *testing.T) {
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

// Vectored writes, Writer fragments and pings in both directions share one
// socket: every frame is whole, fragments are in order, messages arrive intact.
func TestVectoredWriteInterleavesControl(t *testing.T) {
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
			fillPattern(buf, byte(i))
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
			fillPattern(msg, byte(100+i))
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

// CloseNow while a vectored write is blocked on a peer that does not read:
// the deadline on the hijacked conn must reach the socket the writev uses.
func TestCloseNowDuringVectoredWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the write can complete with the peer not reading")
	}
	srv, cli := tcpPair(t)
	// Small socket buffers keep the write from completing on platforms that
	// buffer generously.
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

// After the Close frame went out, a Write with a done context reports the
// context's error rather than the refusal, and leaves the handshake running.
func TestCanceledWriteAfterCloseFrame(t *testing.T) {
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

// closeHoldConn holds the write of a Close frame once it reached the peer,
// keeping frameMu held until release is closed.
type closeHoldConn struct {
	net.Conn

	written, release chan struct{}
}

func (c *closeHoldConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if len(p) > 0 && p[0] == 0x80|opClose {
		close(c.written)
		<-c.release
	}
	return n, err
}

// A Writer whose context ends after the Close frame went out does not end the
// closing handshake. It reports net.ErrClosed, or the context's error when it
// gave up waiting for the Close frame's write to return.
func TestCanceledWriterAfterCloseFrame(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) {
			pc := fasthttputil.NewPipeConns()
			nc := &closeHoldConn{Conn: pc.Conn1(), written: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(nc.release) })
			c := newConn(nc, nc, false, "", connConfig{})
			t.Cleanup(func() { release(); _ = c.CloseNow(); pc.Conn2().Close() })
			peer := &rawPeer{Conn: pc.Conn2(), t: t}
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
			<-nc.written
			want := context.Canceled
			if !held {
				release()
				c.frameMu.wait()
				want = net.ErrClosed
			}
			cancel()
			if err := w.Close(); !errors.Is(err, want) {
				t.Fatalf("writer close: %v, want %v", err, want)
			}
			if c.isClosed() {
				t.Fatalf("the cancelled writer ended the closing handshake: %v", c.closedErr())
			}
			release()
			peer.send(frame(opClose, true, []byte{3, 0xe8}))
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if CloseStatus(context.Cause(c.Context())) != StatusNormalClosure {
				t.Fatalf("cause: %v", context.Cause(c.Context()))
			}
		})
	}
}
