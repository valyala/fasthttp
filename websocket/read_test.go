package websocket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/valyala/fasthttp/fasthttputil"
)

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

func TestProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames [][]byte
		code   StatusCode
	}{
		{"rsv bit", [][]byte{{0x80 | 0x40 | opText, 0x80, 0, 0, 0, 0}}, StatusProtocolError},
		{"reserved data opcode", [][]byte{frame(3, true, nil)}, StatusProtocolError},
		{"reserved control opcode", [][]byte{frame(11, true, nil)}, StatusProtocolError},
		{"unmasked", [][]byte{appendFrame(nil, opText, true, false, []byte("x"))}, StatusProtocolError},
		{"fragmented ping", [][]byte{frame(opPing, false, nil)}, StatusProtocolError},
		{"long control", [][]byte{appendFrame(nil, opPing, true, true, make([]byte, 126))}, StatusProtocolError},
		{"continuation first", [][]byte{frame(opContinuation, true, []byte("x"))}, StatusProtocolError},
		{"text during fragments", [][]byte{frame(opText, false, []byte("a")), frame(opText, true, []byte("b"))}, StatusProtocolError},
		{"non-minimal 16-bit length", [][]byte{{0x80 | opText, 0x80 | 126, 0, 10, 0, 0, 0, 0}}, StatusProtocolError},
		{"non-minimal 64-bit length", [][]byte{{0x80 | opText, 0x80 | 127, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0}}, StatusProtocolError},
		{"64-bit length msb", [][]byte{{0x80 | opText, 0x80 | 127, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}}, StatusProtocolError},
		{"invalid utf-8", [][]byte{frame(opText, true, []byte{0xff})}, StatusInvalidFramePayloadData},
		{"utf-8 split invalid", [][]byte{frame(opText, false, []byte{0xce}), frame(opContinuation, true, []byte{0x41})}, StatusInvalidFramePayloadData},
		{"utf-8 incomplete", [][]byte{frame(opText, true, []byte{0xe2, 0x82})}, StatusInvalidFramePayloadData},
		{"utf-8 surrogate", [][]byte{frame(opText, true, []byte{0xed, 0xa0, 0x80})}, StatusInvalidFramePayloadData},
		{"close length 1", [][]byte{frame(opClose, true, []byte{3})}, StatusProtocolError},
		{"close code 1004", [][]byte{frame(opClose, true, []byte{3, 0xec})}, StatusProtocolError},
		{"close code 1005", [][]byte{frame(opClose, true, []byte{3, 0xed})}, StatusProtocolError},
		{"close code 2999", [][]byte{frame(opClose, true, []byte{0x0b, 0xb7})}, StatusProtocolError},
		{"close bad reason", [][]byte{frame(opClose, true, []byte{3, 0xe8, 0xff})}, StatusInvalidFramePayloadData},
		{"too big", [][]byte{frame(opBinary, true, make([]byte, 65))}, StatusMessageTooBig},
		{"too big fragmented", [][]byte{frame(opBinary, false, make([]byte, 40)), frame(opContinuation, true, make([]byte, 30))}, StatusMessageTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{maxMessageSize: 64})
			peer.send(tc.frames...)
			_, _, err := c.Read(t.Context())
			if CloseStatus(err) != tc.code {
				t.Fatalf("read: %v, want close %d", err, tc.code)
			}
			peer.expectClose(tc.code)
			if _, _, err := c.Read(t.Context()); CloseStatus(err) != tc.code {
				t.Fatalf("read after close: %v", err)
			}
		})
	}
}

func TestControlFramesInterleaved(t *testing.T) {
	c, peer := newRawServer(t, connConfig{})
	peer.send(
		frame(opText, false, []byte("hel")),
		frame(opPing, true, []byte("p1")),
		frame(opPong, true, []byte("unsolicited")),
		frame(opContinuation, false, nil),
		frame(opContinuation, true, []byte("lo")),
	)
	typ, p, err := c.Read(t.Context())
	if err != nil || typ != MessageText || string(p) != "hello" {
		t.Fatalf("read: %d %q %v", typ, p, err)
	}
	if h, p := peer.recv(); h != 0x80|opPong || string(p) != "p1" {
		t.Fatalf("pong %#x %q", h, p)
	}
	// Control frames between fragments are seen by a streaming reader too.
	peer.send(frame(opBinary, false, []byte{1}), frame(opPing, true, nil), frame(opContinuation, true, []byte{2}))
	_, r, err := c.Reader(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("stream: %v %v", got, err)
	}
	if h, p := peer.recv(); h != 0x80|opPong || len(p) != 0 {
		t.Fatalf("pong %#x %q", h, p)
	}
}

func TestLengthEncodings(t *testing.T) {
	for _, n := range []int{0, 125, 126, 65535, 65536} {
		for _, mask := range []bool{false, true} {
			f := appendFrame(nil, opBinary, true, mask, make([]byte, n))
			if len(f) != frameHeaderLen(n, mask)+n {
				t.Fatalf("n=%d mask=%v: frame length %d", n, mask, len(f))
			}
			switch {
			case n < 126:
				if f[1]&0x7f != byte(n) {
					t.Fatalf("n=%d: len7 %d", n, f[1]&0x7f)
				}
			case n < 65536:
				if f[1]&0x7f != 126 || int(binary.BigEndian.Uint16(f[2:])) != n {
					t.Fatalf("n=%d: bad 16-bit length", n)
				}
			default:
				if f[1]&0x7f != 127 || int(binary.BigEndian.Uint64(f[2:])) != n {
					t.Fatalf("n=%d: bad 64-bit length", n)
				}
			}
		}
	}
}

func TestUTF8State(t *testing.T) {
	inputs := [][]byte{
		[]byte("plain ascii"),
		[]byte("héllo wörld ✓ 𝄞"),
		{0xf0, 0x9f, 0x98, 0x80},
		{0xe2, 0x82, 0xac, 0xe2, 0x82},
		{0xc0, 0x80},
		{0xed, 0xa0, 0x80},
		{0xf4, 0x90, 0x80, 0x80},
		{0x80},
		{0xe2, 0x41},
		{0xf8, 0x80, 0x80, 0x80, 0x80},
	}
	for _, in := range inputs {
		want := utf8.Valid(in)
		for chunk := 1; chunk <= len(in)+1; chunk++ {
			var s utf8State
			ok := true
			for i := 0; i < len(in) && ok; i += chunk {
				ok = s.feed(in[i:min(i+chunk, len(in))])
			}
			if got := ok && s.done(); got != want {
				t.Fatalf("%q chunk %d: got %v, want %v", in, chunk, got, want)
			}
		}
	}
}

// A Read bounded by its context ends at the deadline although the peer keeps
// the connection busy with pings, whether or not it reads the pongs.
func TestReadDeadlineBoundsPingStream(t *testing.T) {
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

// A Read whose pong waits behind a blocked write until ReadTimeout passes
// returns the timeout and closes the connection.
func TestReadTimeoutWithPongBehindWrite(t *testing.T) {
	srv, _, writes := pipePair(t)
	ping := frame(opPing, true, []byte("p"))
	c := newConn(srv, bytes.NewReader(ping), false, "", connConfig{readTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = c.CloseNow() })
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.Write(t.Context(), MessageBinary, []byte("held")) }() // The peer never reads it.
	<-writes
	if err := readErr(t, c); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read: %v", err)
	}
	if cause := context.Cause(c.Context()); !errors.Is(cause, os.ErrDeadlineExceeded) {
		t.Fatalf("cause: %v", cause)
	}
	if err := <-writeDone; err == nil {
		t.Fatal("the blocked write succeeded")
	}
	if len(writes) != 0 {
		t.Fatal("the pong reached the transport")
	}
}

// Discarding the rest of a half-read message validates UTF-8 and the size limit
// across empty fragments and pings; a Close frame in it ends with its status.
func TestDiscardMatrix(t *testing.T) {
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
func TestReaderUTF8ByteAtATime(t *testing.T) {
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
func TestMaxMessageSizeBoundary(t *testing.T) {
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
func TestReaderTruncatedByEOF(t *testing.T) {
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

// Read grows its buffer with the bytes that arrive, not with the announced
// frame length, so a header claiming 4 MiB and no data costs one chunk.
func TestReadGrowsWithDataNotHeader(t *testing.T) {
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

// Read sizes its buffer for the frame once enough of it has arrived, and for
// all the frames of a fragmented message.
func TestReadBufferFitsMessage(t *testing.T) {
	pc := fasthttputil.NewPipeConns()
	srv, cli := newConn(pc.Conn1(), pc.Conn1(), false, "", connConfig{}), newConn(pc.Conn2(), pc.Conn2(), true, "", connConfig{})
	t.Cleanup(func() { _, _ = srv.CloseNow(), cli.CloseNow() })
	for _, tc := range []struct{ frames, want int }{{1, 128 << 10}, {2, 256 << 10}} {
		done := make(chan error, 1)
		go func() {
			w, err := cli.Writer(context.Background(), MessageBinary)
			for range tc.frames {
				if err == nil {
					_, err = w.Write(make([]byte, 70000))
				}
			}
			if err == nil {
				err = w.Close()
			}
			done <- err
		}()
		_, p, err := srv.Read(t.Context())
		if err != nil || len(p) != tc.frames*70000 || <-done != nil {
			t.Fatalf("%d frames: read %d, %v", tc.frames, len(p), err)
		}
		if cap(srv.readBuf.b) != tc.want {
			t.Fatalf("%d frames of 70000 bytes read into a buffer of %d, want %d", tc.frames, cap(srv.readBuf.b), tc.want)
		}
	}
}

func TestReadBufSize(t *testing.T) {
	const limit = 1 << 30
	for _, tc := range []struct{ have, need, want, size int }{
		{64 << 10, 64<<10 + 1, 70000, 70000},                     // The frame's end is known.
		{64 << 10, 128 << 10, 4 << 20, 256 << 10},                // No more than four times what arrived.
		{4096, 4097, 100, 4097},                                  // At least what is needed now.
		{maxPooled, maxPooled + 1, maxPooled + 1, 2 * maxPooled}, // Geometric past the pool.
		{limit / 2, limit/2 + 1, limit/2 + 1, limit},             // Up to the limit.
	} {
		if got := readBufSize(tc.have, tc.need, tc.want, limit); got != tc.size {
			t.Fatalf("readBufSize(%d, %d, %d) = %d, want %d", tc.have, tc.need, tc.want, got, tc.size)
		}
	}
}

// Read grows its buffer across every size class boundary, including beyond
// the largest pooled class, and the returned bytes survive CloseNow.
func TestReadGrowthBoundaries(t *testing.T) {
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

// A slice handed out by Read stays intact while other connections churn through
// the shared pools and while this connection's own Close drains the peer.
func TestPooledBufferIsolation(t *testing.T) {
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

// A reader abandoned with a cancelled context does not close the connection;
// the next Read discards what is left of that message, still validating it.
func TestReaderAbandonedThenRead(t *testing.T) {
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
func TestUTF8AtReadChunkBoundary(t *testing.T) {
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

// A fragmented message that crosses several pool classes arrives intact.
func TestLargeFragmentedRead(t *testing.T) {
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
func TestTruncatedExtendedLength(t *testing.T) {
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
