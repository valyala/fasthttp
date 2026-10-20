package websocket

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/flate"
	"github.com/valyala/fasthttp"
)

func TestDeflateNegotiate(t *testing.T) {
	ctx, noCtx := CompressionContextTakeover, CompressionNoContextTakeover
	for _, tc := range []struct {
		name, want                string
		offers                    []string
		mode                      CompressionMode
		threshold                 int
		readContext, writeContext bool
		writeBits, wantThresh     int
	}{
		{name: "disabled", mode: CompressionDisabled, offers: []string{"permessage-deflate"}},
		{
			name: "no context", mode: noCtx, offers: []string{"permessage-deflate"},
			want: "permessage-deflate; server_no_context_takeover; client_no_context_takeover", wantThresh: 512,
		},
		{name: "context", mode: ctx, offers: []string{"permessage-deflate"}, want: "permessage-deflate", readContext: true, writeContext: true, wantThresh: 128},
		{name: "client bits hint", mode: ctx, offers: []string{"permessage-deflate; client_max_window_bits"}, want: "permessage-deflate", readContext: true, writeContext: true, wantThresh: 128},
		{name: "client bits value", mode: ctx, offers: []string{"permessage-deflate; client_max_window_bits=12"}, want: "permessage-deflate", readContext: true, writeContext: true, wantThresh: 128},
		{
			name: "server no context", mode: ctx, offers: []string{"permessage-deflate; server_no_context_takeover"},
			want: "permessage-deflate; server_no_context_takeover", readContext: true, wantThresh: 512,
		},
		{
			name: "client no context", mode: ctx, offers: []string{"permessage-deflate; client_no_context_takeover"},
			want: "permessage-deflate; client_no_context_takeover", writeContext: true, wantThresh: 128,
		},
		{
			name: "server bits", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits=10"},
			want: "permessage-deflate; server_max_window_bits=10", readContext: true, writeContext: true, writeBits: 10, wantThresh: 128,
		},
		{
			name: "server bits 15", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits=15"},
			want: "permessage-deflate; server_max_window_bits=15", readContext: true, writeContext: true, wantThresh: 128,
		},
		{
			name: "quoted server bits", mode: ctx, offers: []string{`permessage-deflate; server_max_window_bits="12"`},
			want: "permessage-deflate; server_max_window_bits=12", readContext: true, writeContext: true, writeBits: 12, wantThresh: 128,
		},
		{name: "server bits no value", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits"}},
		{name: "server bits 7", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits=7"}},
		{name: "server bits 16", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits=16"}},
		{name: "server bits 08", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits=08"}},
		{name: "unknown parameter", mode: ctx, offers: []string{"permessage-deflate; foo"}},
		{name: "duplicate parameter", mode: ctx, offers: []string{"permessage-deflate; server_no_context_takeover; server_no_context_takeover"}},
		{name: "flag with value", mode: ctx, offers: []string{"permessage-deflate; server_no_context_takeover=1"}},
		{name: "other extension", mode: ctx, offers: []string{"x-webkit-deflate-frame"}},
		{
			name: "second offer", mode: ctx, offers: []string{"permessage-deflate; server_max_window_bits, permessage-deflate; client_no_context_takeover"},
			want: "permessage-deflate; client_no_context_takeover", writeContext: true, wantThresh: 128,
		},
		{name: "second header", mode: ctx, offers: []string{"x-foo", "permessage-deflate"}, want: "permessage-deflate", readContext: true, writeContext: true, wantThresh: 128},
		{
			name: "case", mode: ctx, offers: []string{"Permessage-Deflate; Server_No_Context_Takeover"},
			want: "permessage-deflate; server_no_context_takeover", readContext: true, wantThresh: 512,
		},
		{
			name: "no context with bits", mode: noCtx, offers: []string{"permessage-deflate; server_max_window_bits=9"},
			want: "permessage-deflate; server_no_context_takeover; client_no_context_takeover; server_max_window_bits=9", writeBits: 9, wantThresh: 512,
		},
		{name: "threshold", mode: ctx, threshold: 1000, offers: []string{"permessage-deflate"}, want: "permessage-deflate", readContext: true, writeContext: true, wantThresh: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var offers [][]byte
			for _, o := range tc.offers {
				offers = append(offers, []byte(o))
			}
			if !validExtensions(offers) {
				t.Fatal("offer fails the syntax check")
			}
			got, cfg := negotiateDeflate(tc.mode, tc.threshold, offers, nil)
			if string(got) != tc.want {
				t.Fatalf("response %q, want %q", got, tc.want)
			}
			want := deflateConfig{enabled: tc.want != "", readContext: tc.readContext, writeContext: tc.writeContext, writeBits: tc.writeBits, threshold: tc.wantThresh}
			if cfg != want {
				t.Fatalf("config %+v, want %+v", cfg, want)
			}
		})
	}
}

func TestDeflateAccept(t *testing.T) {
	ctx, noCtx := CompressionContextTakeover, CompressionNoContextTakeover
	for _, tc := range []struct {
		name                      string
		values                    []string
		mode                      CompressionMode
		enabled, err              bool
		readContext, writeContext bool
		writeBits                 int
	}{
		{name: "none offered none selected", mode: CompressionDisabled},
		{name: "none offered", mode: CompressionDisabled, values: []string{"permessage-deflate"}, err: true},
		{name: "declined", mode: ctx},
		{name: "context", mode: ctx, values: []string{"permessage-deflate"}, enabled: true, readContext: true, writeContext: true},
		{name: "server no context", mode: ctx, values: []string{"permessage-deflate; server_no_context_takeover"}, enabled: true, writeContext: true},
		{name: "client no context", mode: ctx, values: []string{"permessage-deflate; client_no_context_takeover"}, enabled: true, readContext: true},
		{name: "client bits", mode: ctx, values: []string{"permessage-deflate; client_max_window_bits=10"}, enabled: true, readContext: true, writeContext: true, writeBits: 10},
		{name: "client bits no value", mode: ctx, values: []string{"permessage-deflate; client_max_window_bits"}, err: true},
		{name: "server bits", mode: ctx, values: []string{"permessage-deflate; server_max_window_bits=12"}, enabled: true, readContext: true, writeContext: true},
		{name: "server bits no value", mode: ctx, values: []string{"permessage-deflate; server_max_window_bits"}, err: true},
		{name: "two in one", mode: ctx, values: []string{"permessage-deflate, permessage-deflate"}, err: true},
		{name: "two headers", mode: ctx, values: []string{"permessage-deflate", "permessage-deflate"}, err: true},
		{name: "other", mode: ctx, values: []string{"x-foo"}, err: true},
		{name: "unknown parameter", mode: ctx, values: []string{"permessage-deflate; bogus"}, err: true},
		{name: "empty", mode: ctx, values: []string{""}, err: true},
		{name: "no context ignored", mode: noCtx, values: []string{"permessage-deflate"}, err: true},
		{name: "no context", mode: noCtx, values: []string{"permessage-deflate; server_no_context_takeover"}, enabled: true},
		{name: "no context both", mode: noCtx, values: []string{"permessage-deflate; server_no_context_takeover; client_no_context_takeover"}, enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values [][]byte
			for _, v := range tc.values {
				values = append(values, []byte(v))
			}
			cfg, err := acceptDeflate(tc.mode, 0, values)
			if (err != nil) != tc.err {
				t.Fatalf("error %v, want error %v", err, tc.err)
			}
			if err != nil {
				return
			}
			want := newDeflateConfig(tc.readContext, tc.writeContext, 0, 0)
			want.enabled, want.writeBits = tc.enabled, tc.writeBits
			if !tc.enabled {
				want = deflateConfig{}
			}
			if cfg != want {
				t.Fatalf("config %+v, want %+v", cfg, want)
			}
		})
	}
}

// deflateMessages holds the payloads an echo test sends.
func deflateMessages() []struct {
	typ MessageType
	p   []byte
} {
	random := make([]byte, 3000)
	_, _ = rand.Read(random)
	return []struct {
		typ MessageType
		p   []byte
	}{
		{MessageText, []byte("tiny")},
		{MessageText, []byte(strings.Repeat("compressible text ", 300))},
		{MessageBinary, random},
		{MessageBinary, bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 20<<10)},
		{MessageText, []byte(strings.Repeat("ü", 600))},
		{MessageBinary, nil},
	}
}

func TestDeflateEcho(t *testing.T) {
	modes := []CompressionMode{CompressionDisabled, CompressionNoContextTakeover, CompressionContextTakeover}
	for _, sm := range modes {
		for _, cm := range modes {
			t.Run(fmt.Sprintf("server%d/client%d", sm, cm), func(t *testing.T) {
				srv := startServer(t, &AcceptOptions{CompressionMode: sm}, echo)
				c, err := Dial(t.Context(), "ws://"+srv.addr+"/", &DialOptions{CompressionMode: cm})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.CloseNow() }()
				enabled := sm != CompressionDisabled && cm != CompressionDisabled
				context := sm == CompressionContextTakeover && cm == CompressionContextTakeover
				if cfg := c.cfg.deflate; cfg.enabled != enabled || cfg.readContext != context || cfg.writeContext != context {
					t.Fatalf("negotiated %+v", cfg)
				}
				ctx := t.Context()
				for round := range 3 {
					for i, m := range deflateMessages() {
						if err := c.Write(ctx, m.typ, m.p); err != nil {
							t.Fatalf("write %d: %v", i, err)
						}
						typ, got, err := c.Read(ctx)
						if err != nil || typ != m.typ || !bytes.Equal(got, m.p) {
							t.Fatalf("round %d message %d: type %d, %d bytes, %v", round, i, typ, len(got), err)
						}
					}
				}
				// Many small similar messages, where context takeover matters.
				for i := range 300 {
					msg := fmt.Appendf(nil, `{"seq":%d,"event":"tick","price":%d}`, i, 1000+i%7)
					if err := c.Write(ctx, MessageText, msg); err != nil {
						t.Fatal(err)
					}
					if _, got, err := c.Read(ctx); err != nil || !bytes.Equal(got, msg) {
						t.Fatalf("message %d: %q, %v", i, got, err)
					}
				}
				// Streamed in fragments, including a large first write and an
				// empty final fragment, and read back in pieces.
				big := bytes.Repeat([]byte("0123456789"), 2000)
				for _, chunks := range [][]int{{100, 100}, {5000, 5000, 5000, 5000}, {20000}, {1, 1, 1}} {
					w, err := c.Writer(ctx, MessageBinary)
					if err != nil {
						t.Fatal(err)
					}
					var sent []byte
					for _, n := range chunks {
						if _, err := w.Write(big[len(sent) : len(sent)+n]); err != nil {
							t.Fatal(err)
						}
						sent = big[:len(sent)+n]
					}
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}
					_, r, err := c.Reader(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var got bytes.Buffer
					if _, err := io.CopyBuffer(&got, r, make([]byte, 777)); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got.Bytes(), sent) {
						t.Fatalf("chunks %v: got %d bytes, want %d", chunks, got.Len(), len(sent))
					}
				}
				if err := c.Close(ctx, StatusNormalClosure, ""); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// testDeflater compresses messages the way a peer with context takeover does.
type testDeflater struct {
	w   *flate.Writer
	out bytes.Buffer
}

func newTestDeflater() *testDeflater {
	d := &testDeflater{}
	d.w, _ = flate.NewWriter(&d.out, flate.BestSpeed)
	return d
}

// message returns p compressed, without the trailing sync marker.
func (d *testDeflater) message(t *testing.T, p []byte) []byte {
	t.Helper()
	d.out.Reset()
	_, _ = d.w.Write(p)
	_ = d.w.Flush()
	b := d.out.Bytes()
	if !bytes.HasSuffix(b, deflateTail) {
		t.Fatalf("compressed data does not end with the sync marker: %x", b)
	}
	return bytes.Clone(b[:len(b)-len(deflateTail)])
}

// testInflater decompresses messages the way a peer with context takeover does.
type testInflater struct{ window []byte }

func (d *testInflater) message(t *testing.T, payload []byte) []byte {
	t.Helper()
	src := io.MultiReader(bytes.NewReader(payload), bytes.NewReader(inflateTail))
	got, err := io.ReadAll(flate.NewReaderDict(src, d.window))
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	d.window = slide(d.window, got, deflateWindow)
	return got
}

func deflateServer(t *testing.T, readContext, writeContext bool, threshold int) (*Conn, *rawPeer) {
	t.Helper()
	return newRawServer(t, connConfig{deflate: newDeflateConfig(readContext, writeContext, 0, threshold)})
}

func TestDeflateWriteWire(t *testing.T) {
	c, peer := deflateServer(t, true, true, 0)
	ctx := t.Context()
	inf := &testInflater{}
	msgs := [][]byte{[]byte(strings.Repeat("hello ", 100)), []byte(strings.Repeat("hello ", 100)), bytes.Repeat([]byte{7}, 70000)}
	for i, msg := range msgs {
		if err := c.Write(ctx, MessageText, msg); err != nil {
			t.Fatal(err)
		}
		h, payload := peer.recv()
		if h != 0x80|rsv1|opText {
			t.Fatalf("message %d: header %#x", i, h)
		}
		if i == 1 && len(payload) > 20 {
			t.Fatalf("repeated message compressed to %d bytes; the context was not used", len(payload))
		}
		if got := inf.message(t, payload); !bytes.Equal(got, msg) {
			t.Fatalf("message %d: %d bytes, want %d", i, len(got), len(msg))
		}
	}
	// Below the threshold the message goes out as is.
	if err := c.Write(ctx, MessageText, []byte("small")); err != nil {
		t.Fatal(err)
	}
	if h, payload := peer.recv(); h != 0x80|opText || string(payload) != "small" {
		t.Fatalf("got %#x %q", h, payload)
	}
	// Fragments: RSV1 on the first only; the payloads form one DEFLATE stream.
	w, err := c.Writer(ctx, MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("fragment data "), 3000)
	for _, part := range [][]byte{big[:10000], big[10000:20000], big[20000:]} {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var stream []byte
	for i := 0; ; i++ {
		h, payload := peer.recv()
		want := opContinuation
		if i == 0 {
			want = rsv1 | opBinary
		}
		if h&0x7f != want {
			t.Fatalf("fragment %d: header %#x, want %#x", i, h, want)
		}
		stream = append(stream, payload...)
		if h&0x80 != 0 {
			break
		}
	}
	if got := inf.message(t, stream); !bytes.Equal(got, big) {
		t.Fatalf("fragmented message: %d bytes, want %d", len(got), len(big))
	}
}

func TestDeflateReadWire(t *testing.T) {
	c, peer := deflateServer(t, true, true, 0)
	ctx := t.Context()
	def := newTestDeflater()
	msgs := [][]byte{[]byte(strings.Repeat("hello ", 100)), []byte(strings.Repeat("hello ", 100)), []byte("ünïcödé"), bytes.Repeat([]byte{9}, 100000)}
	for i, msg := range msgs {
		peer.send(frame(opText|rsv1, true, def.message(t, msg)))
		typ, got, err := c.Read(ctx)
		if err != nil || typ != MessageText || !bytes.Equal(got, msg) {
			t.Fatalf("message %d: type %d, %d bytes, %v", i, typ, len(got), err)
		}
	}
	// A compressed message split into fragments, read through a Reader.
	msg := bytes.Repeat([]byte("piece "), 5000)
	z := def.message(t, msg)
	peer.send(frame(opBinary|rsv1, false, z[:len(z)/3]), frame(opContinuation, false, z[len(z)/3:2*len(z)/3]), frame(opContinuation, true, z[2*len(z)/3:]))
	_, r, err := c.Reader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("fragmented: %d bytes, %v", len(got), err)
	}
	// A message abandoned half-way keeps the context for the next one.
	peer.send(frame(opText|rsv1, true, def.message(t, msg)), frame(opText|rsv1, true, def.message(t, msg)))
	if _, r, err = c.Reader(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.Read(ctx); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("after discard: %d bytes, %v", len(got), err)
	}
	// Uncompressed messages do not enter the context.
	peer.send(frame(opText, true, []byte("plain")))
	if _, got, err := c.Read(ctx); err != nil || string(got) != "plain" {
		t.Fatalf("plain: %q, %v", got, err)
	}
	peer.send(frame(opText|rsv1, true, def.message(t, msg)))
	if _, got, err := c.Read(ctx); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("after plain: %d bytes, %v", len(got), err)
	}
}

func TestDeflateReadErrors(t *testing.T) {
	bomb := newTestDeflater().message(t, make([]byte, 1<<20))
	if len(bomb) > 4096 {
		t.Fatalf("bomb is %d bytes", len(bomb))
	}
	for _, tc := range []struct {
		name    string
		frames  [][]byte
		cfg     connConfig
		code    StatusCode
		enabled bool
	}{
		{"not negotiated", [][]byte{frame(opText|rsv1, true, newTestDeflater().message(t, []byte("x")))}, connConfig{}, StatusProtocolError, false},
		{"rsv1 on continuation", [][]byte{frame(opText, false, []byte("a")), frame(opContinuation|rsv1, true, []byte("b"))}, connConfig{}, StatusProtocolError, true},
		{"rsv1 on ping", [][]byte{frame(opPing|rsv1, true, nil)}, connConfig{}, StatusProtocolError, true},
		{"rsv2", [][]byte{frame(opText|0x20, true, nil)}, connConfig{}, StatusProtocolError, true},
		{"corrupt", [][]byte{frame(opText|rsv1, true, []byte{0xff, 0xff, 0xff, 0xff, 0xff})}, connConfig{}, StatusInvalidFramePayloadData, true},
		{"truncated", [][]byte{frame(opText|rsv1, true, newTestDeflater().message(t, bytes.Repeat([]byte("abc"), 1000))[:5])}, connConfig{}, StatusInvalidFramePayloadData, true},
		{"invalid utf-8", [][]byte{frame(opText|rsv1, true, newTestDeflater().message(t, []byte{'a', 0xff, 'b'}))}, connConfig{}, StatusInvalidFramePayloadData, true},
		{"incomplete utf-8", [][]byte{frame(opText|rsv1, true, newTestDeflater().message(t, []byte{0xe2, 0x82}))}, connConfig{}, StatusInvalidFramePayloadData, true},
		{"too big after inflating", [][]byte{frame(opBinary|rsv1, true, bomb)}, connConfig{maxMessageSize: 4096}, StatusMessageTooBig, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if tc.enabled {
				cfg.deflate = newDeflateConfig(true, true, 0, 0)
			}
			c, peer := newRawServer(t, cfg)
			peer.send(tc.frames...)
			err := readErr(t, c)
			if CloseStatus(err) != tc.code {
				t.Fatalf("got %v, want close %d", err, tc.code)
			}
			peer.expectClose(tc.code)
		})
	}
}

func TestDeflateNoContextAndWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window int
	}{{"no context", 0}, {"window 1024", 10}} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := newRawServer(t, connConfig{deflate: newDeflateConfig(false, false, tc.window, 0)})
			ctx := t.Context()
			msg := []byte(strings.Repeat("some text that repeats ", 200))
			for range 3 {
				if err := c.Write(ctx, MessageText, msg); err != nil {
					t.Fatal(err)
				}
				h, payload := peer.recv()
				// Every message is decoded on its own, with no dictionary.
				if got := (&testInflater{}).message(t, payload); h != 0x80|rsv1|opText || !bytes.Equal(got, msg) {
					t.Fatalf("header %#x, %d bytes", h, len(got))
				}
			}
			// Incoming messages are decoded without a dictionary too.
			for range 2 {
				peer.send(frame(opText|rsv1, true, newTestDeflater().message(t, msg)))
				if _, got, err := c.Read(ctx); err != nil || !bytes.Equal(got, msg) {
					t.Fatalf("read: %d bytes, %v", len(got), err)
				}
			}
			if len(c.def.dict) != 0 || c.inf.win != nil || c.def.fw != nil {
				t.Fatalf("state kept: %d bytes, %v, %v", len(c.def.dict), c.inf.win, c.def.fw)
			}
		})
	}
}

func TestDeflateThreshold(t *testing.T) {
	c, peer := deflateServer(t, true, true, 2000)
	ctx := t.Context()
	for _, n := range []int{1999, 2000} {
		if err := c.Write(ctx, MessageBinary, make([]byte, n)); err != nil {
			t.Fatal(err)
		}
		h, _ := peer.recv()
		if compressed := h&rsv1 != 0; compressed != (n >= 2000) {
			t.Fatalf("%d bytes: header %#x", n, h)
		}
	}
}

func TestSlide(t *testing.T) {
	var w []byte
	for i, tc := range []struct {
		p, want string
	}{{"abc", "abc"}, {"de", "abcde"}, {"fg", "cdefg"}, {"hijklmnop", "lmnop"}, {"", "lmnop"}, {"qrstuvw", "stuvw"}, {"xy", "uvwxy"}, {"z", "vwxyz"}} {
		if w = slide(w, []byte(tc.p), 5); string(lastN(w, 5)) != tc.want || len(w) > 10 {
			t.Fatalf("step %d: %q, want %q", i, w, tc.want)
		}
	}
}

func TestDeflateHandshakeHeaders(t *testing.T) {
	// The server answers the client's offer and the client refuses what it
	// did not offer.
	srv := startServer(t, &AcceptOptions{CompressionMode: CompressionNoContextTakeover}, echo)
	var resp fasthttp.Response
	c, err := Dial(t.Context(), "ws://"+srv.addr+"/", &DialOptions{CompressionMode: CompressionContextTakeover, Response: &resp})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if got := string(resp.Header.Peek("Sec-WebSocket-Extensions")); got != "permessage-deflate; server_no_context_takeover; client_no_context_takeover" {
		t.Fatalf("extensions %q", got)
	}
	for _, mode := range []CompressionMode{CompressionNoContextTakeover, CompressionDisabled} {
		addr, _ := rawServer(t, func(accept string) string {
			return okHead + "Sec-WebSocket-Accept: " + accept + "\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n"
		})
		if _, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{CompressionMode: mode}); err == nil || !strings.Contains(err.Error(), "extension") {
			t.Fatalf("mode %d: got %v", mode, err)
		}
	}
}

func TestDeflatePooledWriterIsolation(t *testing.T) {
	// A connection with context takeover hands its compressor back; the next
	// connection must not inherit its dictionary.
	ctx := t.Context()
	msg := []byte(strings.Repeat("shared history ", 300))
	withCtx, peer1 := deflateServer(t, true, true, 0)
	for range 3 {
		if err := withCtx.Write(ctx, MessageText, msg); err != nil {
			t.Fatal(err)
		}
		peer1.recv()
	}
	plain, peer2 := newRawServer(t, connConfig{deflate: newDeflateConfig(false, false, 0, 0)})
	if err := plain.Write(ctx, MessageText, msg); err != nil {
		t.Fatal(err)
	}
	_, payload := peer2.recv()
	if got := (&testInflater{}).message(t, payload); !bytes.Equal(got, msg) {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestDeflateUnsentWriteKeepsContext(t *testing.T) {
	// A compressed fragment that cannot be sent must not enter the dictionary.
	c, peer := deflateServer(t, true, true, 0)
	msg := []byte(strings.Repeat("context ", 1000))
	wctx, cancel := context.WithCancel(t.Context())
	w, err := c.Writer(wctx, MessageText)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := w.Write(msg); err == nil {
		t.Fatal("write with a cancelled context succeeded")
	}
	_ = w.Close()
	if c.isClosed() {
		t.Fatal("connection closed")
	}
	inf := &testInflater{}
	for range 2 {
		if err := c.Write(t.Context(), MessageText, msg); err != nil {
			t.Fatal(err)
		}
		if _, payload := peer.recv(); !bytes.Equal(inf.message(t, payload), msg) {
			t.Fatal("message does not decode with the peer's context")
		}
	}
}

// finalBlocks compresses each part as a complete DEFLATE stream, so that the
// message holds final blocks followed by more blocks.
func finalBlocks(parts ...string) []byte {
	var out bytes.Buffer
	for _, p := range parts {
		w, _ := flate.NewWriter(&out, flate.BestSpeed)
		_, _ = w.Write([]byte(p))
		_ = w.Close()
	}
	return out.Bytes()
}

func TestDeflateFinalBlocksInMessage(t *testing.T) {
	c, peer := deflateServer(t, true, true, 0)
	ctx := t.Context()
	z := finalBlocks("hello ", "world")
	peer.send(frame(opText|rsv1, true, z))
	if _, got, err := c.Read(ctx); err != nil || string(got) != "hello world" {
		t.Fatalf("got %q, %v", got, err)
	}
	peer.send(frame(opText|rsv1, false, z[:len(z)/2]), frame(opContinuation, true, z[len(z)/2:]))
	if _, got, err := c.Read(ctx); err != nil || string(got) != "hello world" {
		t.Fatalf("fragmented: got %q, %v", got, err)
	}
	// Blocks after a final one still see the message's earlier bytes.
	peer.send(frame(opText|rsv1, true, finalBlocks(strings.Repeat("abc", 100), strings.Repeat("abc", 100))))
	if _, got, err := c.Read(ctx); err != nil || string(got) != strings.Repeat("abc", 200) {
		t.Fatalf("history: %d bytes, %v", len(got), err)
	}
	// The form RFC 7692 prescribes: a sync flush after the final block, with
	// its marker removed, leaves the block header byte.
	canonical := append(finalBlocks("x", "y"), 0)
	peer.send(frame(opText|rsv1, true, canonical), frame(opText|rsv1, false, canonical[:2]), frame(opContinuation, true, canonical[2:]))
	for i := range 2 {
		if _, got, err := c.Read(ctx); err != nil || string(got) != "xy" {
			t.Fatalf("canonical %d: got %q, %v", i, got, err)
		}
	}
	// A zero-length read returns at once.
	peer.send(frame(opText|rsv1, true, newTestDeflater().message(t, []byte("pending"))))
	_, r, err := c.Reader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty read: %d, %v", n, err)
	}
	if got, err := io.ReadAll(r); err != nil || string(got) != "pending" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDeflateDiscardUsesCurrentContext(t *testing.T) {
	c, peer := deflateServer(t, true, true, 0)
	def := newTestDeflater()
	msg := []byte(strings.Repeat("abandoned ", 1000))
	z := def.message(t, msg)
	// A ping between the fragments is answered while the message is discarded.
	peer.send(frame(opText|rsv1, false, z[:len(z)/2]), frame(opPing, true, []byte("p")), frame(opContinuation, true, z[len(z)/2:]),
		frame(opText|rsv1, true, def.message(t, msg)))
	rctx, cancel := context.WithCancel(t.Context())
	_, r, err := c.Reader(rctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, got, err := c.Read(t.Context()); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("after abandoned reader: %d bytes, %v", len(got), err)
	}
	if h, payload := peer.recv(); h != 0x80|opPong || string(payload) != "p" {
		t.Fatalf("got %#x %q, want pong", h, payload)
	}
}

func TestDeflateTransportEOF(t *testing.T) {
	c, peer := deflateServer(t, true, true, 0)
	z := newTestDeflater().message(t, bytes.Repeat([]byte("data"), 5000))
	f := frame(opText|rsv1, true, z)
	peer.send(f[:len(f)/2])
	_ = peer.Close()
	if err := readErr(t, c); CloseStatus(err) != StatusAbnormalClosure {
		t.Fatalf("got %v, want close %d", err, StatusAbnormalClosure)
	}
}

func TestDeflateResponseSyntax(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"permessage-deflate;", false}, {"permessage-deflate; client_max_window_bits=", false}, {`permessage-deflate; client_max_window_bits="\1\0"`, true}, {"permessage-deflate ; client_no_context_takeover", true}} {
		addr, _ := rawServer(t, func(accept string) string {
			return okHead + "Sec-WebSocket-Accept: " + accept + "\r\nSec-WebSocket-Extensions: " + tc.value + "\r\n\r\n"
		})
		c, err := Dial(t.Context(), "ws://"+addr+"/", &DialOptions{CompressionMode: CompressionContextTakeover})
		if (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.value, err)
		}
		if err == nil {
			if strings.Contains(tc.value, `"\1\0"`) && c.cfg.deflate.writeBits != 10 {
				t.Fatalf("%q: window bits %d", tc.value, c.cfg.deflate.writeBits)
			}
			_ = c.CloseNow()
		}
	}
}
