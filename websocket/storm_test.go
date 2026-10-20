package websocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/flate"
	"github.com/valyala/fasthttp"
)

// wireTap parses one direction of a relayed connection after the handshake and
// records violations of the framing rules this package must uphold.
type wireTap struct {
	name       string
	mu         sync.Mutex
	buf        []byte
	msg        []byte // Payload of the data message being assembled, unmasked.
	window     []byte // What the receiver's decompressor remembers.
	pending    uint64 // Payload bytes of the current frame still to consume.
	frames     int
	closes     int
	off        int // Offset within the current frame's payload, for unmasking.
	sameKey    int // Consecutive frames that reused the masking key.
	violations []string
	key        [4]byte
	lastKey    [4]byte
	frameOp    byte // Opcode of the frame whose payload is pending.
	msgOp      byte // Opcode of the message being assembled.
	handshake  bool
	wantMasked bool
	frameFin   bool
	masked     bool
	inMsg      bool // A fragmented data message is in progress.
	dropMsg    bool // The message is too large to check.
	deflate    bool // permessage-deflate was negotiated with context takeover.
	msgRSV     bool // The message being assembled is compressed.
}

// inflate decompresses a message the way the receiver does: a final block
// followed by more data starts the decompressor over, and the tail is only
// read when the data does not end with a final block. The source is a
// bytes.Reader so that the decompressor does not read ahead.
func (d *wireTap) inflate(z []byte) ([]byte, error) {
	const limit = 4 << 20
	src := bytes.NewReader(append(bytes.Clone(z), inflateTail...))
	fr := flate.NewReaderDict(src, lastN(d.window, deflateWindow))
	var out []byte
	for {
		part, err := io.ReadAll(io.LimitReader(fr, limit+1-int64(len(out))))
		out = append(out, part...)
		if err != nil {
			return nil, err
		}
		if len(out) > limit {
			return nil, fmt.Errorf("inflates to more than %d bytes", limit)
		}
		if src.Len() <= len(inflateTail) {
			break
		}
		history := lastN(slide(bytes.Clone(d.window), out, deflateWindow), deflateWindow)
		if err := fr.(flate.Resetter).Reset(src, history); err != nil { //nolint:forcetypeassert // flate readers implement Resetter.
			return nil, err
		}
	}
	d.window = slide(d.window, out, deflateWindow)
	return out, nil
}

// stormPayload returns n bytes whose every byte follows from the first.
func stormPayload(n int) []byte {
	b := make([]byte, n)
	probePattern(b, byte(rand.IntN(256)))
	return b
}

func stormCheckPattern(p []byte) error {
	for i := range p {
		if p[i] != p[0]+byte(i) {
			return fmt.Errorf("byte %d of %d is %#x, want %#x", i, len(p), p[i], p[0]+byte(i))
		}
	}
	return nil
}

// consume takes payload bytes of the current frame.
func (d *wireTap) consume(p []byte) {
	if d.frameOp != opText && d.frameOp != opBinary && d.frameOp != opContinuation {
		return
	}
	if d.dropMsg || len(d.msg)+len(p) > 1<<20 {
		d.dropMsg = true
		return
	}
	start := len(d.msg)
	d.msg = append(d.msg, p...)
	if d.masked {
		for i := range p {
			d.msg[start+i] ^= d.key[(d.off+i)&3]
		}
	}
	d.off += len(p)
}

// endFrame closes the current frame and checks a completed message.
func (d *wireTap) endFrame() {
	switch d.frameOp {
	case opText, opBinary, opContinuation:
	default:
		return
	}
	if !d.frameFin {
		d.inMsg = true
		return
	}
	msg := d.msg
	var inflateErr error
	if d.msgRSV && !d.dropMsg {
		msg, inflateErr = d.inflate(msg)
	}
	switch {
	case d.dropMsg:
		d.violatef("frame %d: message larger than this storm sends", d.frames)
	case inflateErr != nil:
		d.violatef("frame %d: compressed message does not inflate: %v", d.frames, inflateErr)
	case d.msgOp == opText && !utf8.Valid(msg):
		d.violatef("frame %d: text message of %d bytes is not valid UTF-8", d.frames, len(msg))
	case d.msgOp == opBinary:
		if err := stormCheckPattern(msg); err != nil {
			d.violatef("frame %d: binary message corrupted: %v", d.frames, err)
		}
	}
	d.inMsg, d.dropMsg = false, false
	d.msg = d.msg[:0]
}

func (d *wireTap) violatef(format string, args ...any) {
	if len(d.violations) < 10 {
		d.violations = append(d.violations, fmt.Sprintf(format, args...))
	}
}

func (d *wireTap) feed(p []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, p...)
	rest := d.parse(d.buf)
	d.buf = d.buf[:copy(d.buf, rest)]
}

// parse consumes the complete frames in buf and returns what is left of it.
func (d *wireTap) parse(buf []byte) []byte {
	if !d.handshake {
		i := bytes.Index(buf, []byte("\r\n\r\n"))
		if i < 0 {
			return buf
		}
		buf = buf[i+4:]
		d.handshake = true
	}
	for {
		if d.pending > 0 {
			k := min(d.pending, uint64(len(buf)))
			d.consume(buf[:k])
			buf = buf[k:]
			d.pending -= k
			if d.pending > 0 {
				return buf
			}
			d.endFrame()
		}
		if len(buf) < 2 {
			return buf
		}
		h0, h1 := buf[0], buf[1]
		fin := h0&0x80 != 0
		op := h0 & 0x0f
		masked := h1&0x80 != 0
		n := uint64(h1 & 0x7f)
		hl := 2
		switch n {
		case 126:
			if len(buf) < 4 {
				return buf
			}
			n = uint64(binary.BigEndian.Uint16(buf[2:]))
			hl = 4
		case 127:
			if len(buf) < 10 {
				return buf
			}
			n = binary.BigEndian.Uint64(buf[2:])
			hl = 10
		}
		if masked {
			hl += 4
		}
		if len(buf) < hl {
			return buf
		}
		d.frames++
		if rsv := h0 & 0x70; rsv != 0 && (!d.deflate || rsv != rsv1 || (op != opText && op != opBinary)) {
			d.violatef("frame %d: RSV bits set (%#x)", d.frames, h0)
		}
		if masked != d.wantMasked {
			d.violatef("frame %d: mask bit %v, want %v", d.frames, masked, d.wantMasked)
		}
		if n > 1<<24 {
			d.violatef("frame %d: length %d is not something this package sends", d.frames, n)
			return nil
		}
		switch op {
		case opClose, opPing, opPong:
			if !fin || n > maxControlPayload || (op == opClose && n == 1) {
				d.violatef("frame %d: malformed control frame op %d fin %v len %d", d.frames, op, fin, n)
			}
			if op == opClose {
				d.closes++
				if d.closes > 1 {
					d.violatef("frame %d: second Close frame", d.frames)
				}
			}
		case opText, opBinary, opContinuation:
			if d.closes > 0 {
				d.violatef("frame %d: data frame op %d after the Close frame", d.frames, op)
			}
			if (op == opContinuation) != d.inMsg {
				d.violatef("frame %d: data frame op %d while a fragmented message in progress is %v", d.frames, op, d.inMsg)
				d.msg, d.dropMsg = d.msg[:0], false
			}
			if op != opContinuation {
				d.msgOp, d.msgRSV = op, h0&rsv1 != 0
			}
		default:
			d.violatef("frame %d: reserved opcode %d", d.frames, op)
		}
		if masked {
			copy(d.key[:], buf[hl-4:hl])
			if d.frames > 1 && d.key == d.lastKey {
				d.sameKey++
				if d.sameKey >= 3 {
					d.violatef("frame %d: masking key %x reused %d times in a row", d.frames, d.key, d.sameKey+1)
				}
			} else {
				d.sameKey = 0
			}
			d.lastKey = d.key
		}
		d.frameOp, d.frameFin, d.masked, d.off = op, fin, masked, 0
		buf = buf[hl:]
		d.pending = n
		if n == 0 {
			d.endFrame()
		}
	}
}

type tapSet struct {
	mu      sync.Mutex
	taps    []*wireTap
	deflate bool
}

func (s *tapSet) add(i int, dir string, masked bool) *wireTap {
	tp := &wireTap{name: fmt.Sprintf("conn %d %s", i, dir), wantMasked: masked, deflate: s.deflate}
	s.mu.Lock()
	s.taps = append(s.taps, tp)
	s.mu.Unlock()
	return tp
}

// relayConn forwards cli to addr, feeding each direction to its tap, and
// half-closes the other side when one side ends.
func relayConn(cli net.Conn, addr string, c2s, s2c *wireTap) {
	srv, err := net.Dial("tcp", addr)
	if err != nil {
		cli.Close()
		return
	}
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn, tap *wireTap) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				tap.feed(buf[:n])
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go pipe(srv, cli, c2s)
	go pipe(cli, srv, s2c)
	wg.Wait()
	cli.Close()
	srv.Close()
}

type stormReport struct {
	mu    sync.Mutex
	items []string
}

func (r *stormReport) add(s string) {
	r.mu.Lock()
	if len(r.items) < 50 {
		r.items = append(r.items, s)
	}
	r.mu.Unlock()
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func allStacks() string {
	buf := make([]byte, 1<<20)
	return string(buf[:runtime.Stack(buf, true)])
}

func allowedCause(err error) bool {
	return CloseStatus(err) != -1 || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) || isTimeout(err)
}

func stormReader(c *Conn, mode int) {
	if mode == 2 {
		c.CloseRead(c.Context())
		return
	}
	buf := make([]byte, 4096)
	for {
		ctx, cancel := c.Context(), context.CancelFunc(func() {})
		if rand.IntN(6) == 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(10+rand.IntN(200))*time.Millisecond)
		}
		var err error
		if mode == 0 {
			_, _, err = c.Read(ctx)
		} else {
			var r io.Reader
			_, r, err = c.Reader(ctx)
			if err == nil {
				if rand.IntN(3) == 0 {
					_, err = io.Copy(io.Discard, r)
				} else {
					for i := rand.IntN(3); i >= 0 && err == nil; i-- {
						_, err = r.Read(buf)
					}
				}
				if errors.Is(err, io.EOF) {
					err = nil
				}
			}
		}
		cancel()
		if err != nil {
			return
		}
	}
}

func stormWriter(c *Conn) {
	for c.Context().Err() == nil {
		ctx, cancel := context.WithTimeout(c.Context(), time.Duration(100+rand.IntN(400))*time.Millisecond)
		switch rand.IntN(10) {
		case 0, 1, 2, 3:
			_ = c.Write(ctx, MessageBinary, stormPayload(rand.IntN(70000)))
		case 4:
			_ = c.Write(ctx, MessageText, bytes.Repeat([]byte("é"), rand.IntN(3000)))
		case 5, 6:
			w, err := c.Writer(ctx, MessageBinary)
			if err == nil {
				var werr error
				msg := stormPayload(rand.IntN(45000))
				for len(msg) > 0 && werr == nil {
					k := min(len(msg), 1+rand.IntN(9000))
					_, werr = w.Write(msg[:k])
					msg = msg[k:]
				}
				if rand.IntN(8) == 0 {
					cancel()
				}
				_ = w.Close()
			}
		case 7, 8:
			_ = c.Ping(ctx)
		default:
			time.Sleep(time.Duration(rand.IntN(5)) * time.Millisecond)
		}
		cancel()
	}
}

func stormCloser(c *Conn, label string, rep *stormReport, mustClose bool) {
	time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
	k := rand.IntN(5)
	switch {
	case k == 0:
		_ = c.CloseNow()
	case k <= 3 || mustClose:
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(50+rand.IntN(500))*time.Millisecond)
		code := []StatusCode{StatusNormalClosure, StatusGoingAway, 4000}[rand.IntN(3)]
		err := c.Close(ctx, code, "bye")
		cancel()
		closed := c.isClosed()
		switch {
		case err == nil && !closed:
			rep.add(label + ": Close returned nil but the connection is still open")
		case err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, os.ErrDeadlineExceeded):
			rep.add(fmt.Sprintf("%s: Close returned %v", label, err))
		}
		if err != nil && mustClose {
			_ = c.CloseNow()
		}
	default:
		// Leave closing to the peer.
	}
}

// stormSide drives one end of a connection with a reader, several writers and
// a closer, then checks what a closed connection must report.
func stormSide(c *Conn, label string, rep *stormReport, mustClose bool) {
	var wg sync.WaitGroup
	mode := rand.IntN(3)
	wg.Go(func() { stormReader(c, mode) })
	for range 1 + rand.IntN(3) {
		wg.Go(func() { stormWriter(c) })
	}
	wg.Go(func() { stormCloser(c, label, rep, mustClose) })
	if !waitTimeout(&wg, 20*time.Second) {
		rep.add(label + ": goroutines did not finish within 20s\n" + allStacks())
		return
	}
	select {
	case <-c.Context().Done():
	case <-time.After(20 * time.Second):
		rep.add(label + ": connection never closed")
		return
	}
	cause := context.Cause(c.Context())
	if cause == nil || !allowedCause(cause) {
		rep.add(fmt.Sprintf("%s: unexpected close cause %T %v", label, cause, cause))
	}
	if err := c.Write(context.Background(), MessageText, []byte("late")); err == nil {
		rep.add(label + ": Write returned nil after the connection closed")
	}
	if _, err := c.Writer(context.Background(), MessageBinary); err == nil {
		rep.add(label + ": Writer returned nil after the connection closed")
	}
	if err := c.Ping(context.Background()); err == nil {
		rep.add(label + ": Ping returned nil after the connection closed")
	}
	if mode != 2 {
		if _, _, err := c.Read(context.Background()); err == nil {
			rep.add(label + ": Read returned nil after the connection closed")
		}
	}
	if err := c.Close(context.Background(), StatusNormalClosure, ""); err != nil {
		rep.add(fmt.Sprintf("%s: Close on a closed connection returned %v", label, err))
	}
}

// Random Read/Write/Close traffic on many relayed connections upholds the wire
// invariants and the closed-connection contract; runs for WS_STORM seconds.
// WS_STORM_DEFLATE adds permessage-deflate with context takeover on every
// connection, with every non-empty message compressed.
func TestStorm(t *testing.T) {
	s := os.Getenv("WS_STORM")
	if s == "" {
		t.Skip("set WS_STORM to the duration in seconds")
	}
	secs, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	duration := time.Duration(secs) * time.Second
	const parallel = 6
	var report stormReport
	serverOpts := []*AcceptOptions{
		nil,
		{ReadTimeout: 300 * time.Millisecond},
		{WriteTimeout: 200 * time.Millisecond},
		{ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond, MaxMessageSize: 20000},
	}
	clientOpts := []*DialOptions{
		nil,
		{ReadTimeout: 300 * time.Millisecond},
		{WriteTimeout: 200 * time.Millisecond, ReadBufferSize: 512},
		{ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond, MaxMessageSize: 20000},
	}
	if os.Getenv("WS_STORM_DEFLATE") != "" {
		for i := range serverOpts {
			if serverOpts[i] == nil {
				serverOpts[i] = &AcceptOptions{}
			}
			serverOpts[i].CompressionMode, serverOpts[i].CompressionThreshold = CompressionContextTakeover, 1
		}
		for i := range clientOpts {
			if clientOpts[i] == nil {
				clientOpts[i] = &DialOptions{}
			}
			clientOpts[i].CompressionMode, clientOpts[i].CompressionThreshold = CompressionContextTakeover, 1
		}
	}
	var serverSeq atomic.Int64
	serverDone := make(chan struct{}, 4096)
	configure := func(keep bool) func(*fasthttp.Server) {
		return func(s *fasthttp.Server) {
			s.KeepHijackedConns = keep
			s.Handler = func(ctx *fasthttp.RequestCtx) {
				opts := serverOpts[serverSeq.Add(1)%int64(len(serverOpts))]
				_ = Accept(ctx, opts, func(c *Conn) {
					done := make(chan struct{})
					go func() {
						stormSide(c, "server", &report, false)
						close(done)
						serverDone <- struct{}{}
					}()
					if rand.IntN(2) == 0 {
						<-done
					} else {
						time.Sleep(time.Duration(rand.IntN(60)) * time.Millisecond)
					}
				})
			}
		}
	}
	tsA := startServerWith(t, nil, nil, configure(false))
	tsB := startServerWith(t, nil, nil, configure(true))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	taps := tapSet{deflate: os.Getenv("WS_STORM_DEFLATE") != ""}
	go func() {
		for i := 0; ; i++ {
			cli, err := ln.Accept()
			if err != nil {
				return
			}
			target := tsA.addr
			if i%2 == 1 {
				target = tsB.addr
			}
			go relayConn(cli, target, taps.add(i, "client->server", true), taps.add(i, "server->client", false))
		}
	}()

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	var conns atomic.Int64
	for range parallel {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				c, err := Dial(dctx, "ws://"+ln.Addr().String()+"/ws", clientOpts[rand.IntN(len(clientOpts))])
				cancel()
				if err != nil {
					report.add("dial: " + err.Error())
					continue
				}
				conns.Add(1)
				stormSide(c, "client", &report, true)
			}
		})
	}
	wg.Wait()
	n := conns.Load()
	for i := range n {
		select {
		case <-serverDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("server side %d of %d did not finish\n%s", i, n, allStacks())
		}
	}
	taps.mu.Lock()
	var frames, closes, violations int
	for _, tp := range taps.taps {
		tp.mu.Lock()
		frames += tp.frames
		closes += tp.closes
		for _, v := range tp.violations {
			violations++
			if violations <= 20 {
				t.Errorf("%s: %s", tp.name, v)
			}
		}
		tp.mu.Unlock()
	}
	taps.mu.Unlock()
	t.Logf("%d connections, %d relayed streams, %d frames, %d Close frames, %d wire violations", n, len(taps.taps), frames, closes, violations)
	report.mu.Lock()
	for _, item := range report.items {
		t.Error(item)
	}
	report.mu.Unlock()
}
