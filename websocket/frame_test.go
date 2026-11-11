package websocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/valyala/fasthttp/fasthttputil"
)

// rawPeer is the raw side of a pipe whose other end is a server Conn.
type rawPeer struct {
	net.Conn

	t *testing.T
}

func newRawServer(t *testing.T, cfg connConfig) (*Conn, *rawPeer) {
	t.Helper()
	pc := fasthttputil.NewPipeConns()
	c := newConn(pc.Conn1(), pc.Conn1(), false, "", cfg)
	t.Cleanup(func() { _ = c.CloseNow(); pc.Conn2().Close() })
	return c, &rawPeer{Conn: pc.Conn2(), t: t}
}

func (p *rawPeer) send(frames ...[]byte) {
	p.t.Helper()
	if _, err := p.Write(bytes.Join(frames, nil)); err != nil {
		p.t.Fatalf("raw write: %v", err)
	}
}

// readFrame reads one unmasked frame: its two header bytes and its payload.
func readFrame(r io.Reader) (hdr [2]byte, payload []byte, err error) {
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return hdr, nil, err
	}
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return hdr, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return hdr, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(r, payload)
	return hdr, payload, err
}

// recv reads one unmasked frame and returns its first header byte and payload.
func (p *rawPeer) recv() (byte, []byte) {
	p.t.Helper()
	_ = p.SetReadDeadline(time.Now().Add(5 * time.Second))
	h, payload, err := readFrame(p)
	if err != nil {
		p.t.Fatalf("raw read: %v", err)
	}
	return h[0], payload
}

func (p *rawPeer) expectClose(code StatusCode) {
	p.t.Helper()
	h, payload := p.recv()
	if h != 0x80|opClose || len(payload) < 2 || StatusCode(binary.BigEndian.Uint16(payload)) != code {
		p.t.Fatalf("got frame %#x %q, want close %d", h, payload, code)
	}
}

func frame(op byte, fin bool, payload []byte) []byte {
	return appendFrame(nil, op, fin, true, payload)
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

func TestCtxWatch(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	w := ctxWatch{nc: c1}
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
