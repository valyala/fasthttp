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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

type testServer struct {
	addr  string
	reads chan struct{}

	// preface is sent on every dialed connection ahead of the handshake.
	preface string
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
		if ts.preface != "" {
			if _, err := nc.Write([]byte(ts.preface)); err != nil {
				nc.Close()
				return nil, err
			}
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
	typ, p, err := c.Read(ctx)
	if err == nil {
		t.Fatal("expected read error")
	}
	if typ != 0 || p != nil {
		t.Fatalf("Read returned type %d and %d bytes with %v", typ, len(p), err)
	}
	return err
}

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

// okHead is a 101 response up to its Sec-WebSocket-Accept field.
const okHead = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"

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

// closeCountConn exposes its socket as fasthttp's hijacked conn does and
// counts its own Close calls.
type closeCountConn struct {
	net.Conn

	closed *atomic.Int32
}

func (c closeCountConn) UnsafeConn() net.Conn { return c.Conn }

func (c closeCountConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

// pastDeadlineContext reports a deadline that has passed while its timer has
// not fired yet.
type pastDeadlineContext struct {
	context.Context //nolint:containedctx // Overrides Deadline only.
}

func (pastDeadlineContext) Deadline() (time.Time, bool) { return aLongTimeAgo, true }

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

// fillPattern fills p so that every byte follows from the first one.
func fillPattern(p []byte, tag byte) {
	for i := range p {
		p[i] = tag + byte(i)
	}
}

func patternPayload(n int) []byte {
	b := make([]byte, n)
	fillPattern(b, byte(n))
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

func TestReadWriteAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops entries at random under the race detector")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the count covers the process, where another test's log line resolves the time zone")
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
