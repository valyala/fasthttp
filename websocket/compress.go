package websocket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/klauspost/compress/flate"
)

// CompressionMode selects how permessage-deflate (RFC 7692) is used.
type CompressionMode int

const (
	// CompressionDisabled neither offers nor accepts compression.
	CompressionDisabled CompressionMode = iota

	// CompressionNoContextTakeover compresses each message on its own, with
	// compressors and decompressors taken from pools, so a connection holds
	// no compression state between messages. Small messages gain little.
	CompressionNoContextTakeover

	// CompressionContextTakeover lets messages refer back to earlier ones,
	// which compresses streams of similar small messages well. A connection
	// keeps the last 4 KiB it compressed and the last 32 KiB it decompressed
	// as dictionaries, in buffers of twice that size; the compressors
	// themselves stay pooled. The peer may still decline context takeover for
	// either direction.
	CompressionContextTakeover
)

const (
	rsv1 = 0x40

	deflateThreshold        = 512
	deflateContextThreshold = 128
	deflateDict             = 4 << 10  // Our context: what a message may refer back to.
	deflateWindow           = 32 << 10 // The peer's context.
	deflateReadAhead        = 4 << 10
)

var (
	// deflateTail is the sync marker a message's compressed data ends with on
	// the wire removed; inflateTail restores it and ends the stream.
	deflateTail = []byte{0, 0, 0xff, 0xff}
	inflateTail = []byte{0, 0, 0xff, 0xff, 1, 0, 0, 0xff, 0xff}
)

// deflateConfig is the negotiated use of permessage-deflate.
type deflateConfig struct {
	threshold    int
	writeBits    int // The window bits the peer limited our compressor to; 0 means the default.
	enabled      bool
	readContext  bool // The peer keeps its window across messages.
	writeContext bool // We keep ours.
}

func newDeflateConfig(readContext, writeContext bool, writeBits, threshold int) deflateConfig {
	cfg := deflateConfig{enabled: true, readContext: readContext, writeContext: writeContext, threshold: threshold}
	if threshold <= 0 {
		cfg.threshold = deflateThreshold
		if writeContext {
			cfg.threshold = deflateContextThreshold
		}
	}
	if writeBits != 0 && writeBits < 15 {
		cfg.writeBits = writeBits
	}
	return cfg
}

// deflateParams are the parameters of one permessage-deflate item.
type deflateParams struct {
	serverBits, clientBits   int // 0 when absent; clientBits is -1 when present without a value.
	serverNoCtx, clientNoCtx bool
}

// parseDeflate parses the parameters of a permessage-deflate item, which must
// not repeat. client_max_window_bits needs a value in a response.
func parseDeflate(params []byte, response bool) (p deflateParams, ok bool) {
	for len(params) > 0 {
		var param []byte
		param, params, _ = bytes.Cut(params, []byte{';'})
		key, val, hasVal := bytes.Cut(param, []byte{'='})
		key, val = trimHTTPSpace(key), unquote(trimHTTPSpace(val))
		switch {
		case equalFoldASCII(key, "server_no_context_takeover") && !hasVal && !p.serverNoCtx:
			p.serverNoCtx = true
		case equalFoldASCII(key, "client_no_context_takeover") && !hasVal && !p.clientNoCtx:
			p.clientNoCtx = true
		case equalFoldASCII(key, "server_max_window_bits") && p.serverBits == 0 && windowBits(val) != 0:
			p.serverBits = windowBits(val)
		case equalFoldASCII(key, "client_max_window_bits") && p.clientBits == 0 && !hasVal && !response:
			p.clientBits = -1
		case equalFoldASCII(key, "client_max_window_bits") && p.clientBits == 0 && windowBits(val) != 0:
			p.clientBits = windowBits(val)
		default:
			return p, false
		}
	}
	return p, true
}

// windowBits parses a window size of 8 to 15, or returns 0.
func windowBits(v []byte) int {
	switch {
	case len(v) == 1 && (v[0] == '8' || v[0] == '9'):
		return int(v[0] - '0')
	case len(v) == 2 && v[0] == '1' && '0' <= v[1] && v[1] <= '5':
		return 10 + int(v[1]-'0')
	}
	return 0
}

// unquote returns the value of a quoted-string, resolving quoted pairs.
func unquote(b []byte) []byte {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return b
	}
	b = b[1 : len(b)-1]
	if bytes.IndexByte(b, '\\') < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) {
			i++
		}
		out = append(out, b[i])
	}
	return out
}

// deflateItems calls fn with the parameters of each item of the extension
// lists in values until it returns true, and reports whether one did.
func deflateItems(values [][]byte, fn func(name, params []byte) bool) bool {
	for _, v := range values {
		for item, rest, ok := nextToken(v); ok; item, rest, ok = nextToken(rest) {
			name, params, _ := bytes.Cut(item, []byte{';'})
			if fn(trimHTTPSpace(name), params) {
				return true
			}
		}
	}
	return false
}

// negotiateDeflate accepts the first usable permessage-deflate offer: it
// appends the response value to dst and returns the configuration.
func negotiateDeflate(mode CompressionMode, threshold int, offers [][]byte, dst []byte) ([]byte, deflateConfig) {
	var cfg deflateConfig
	if mode == CompressionDisabled {
		return nil, cfg
	}
	deflateItems(offers, func(name, params []byte) bool {
		p, ok := parseDeflate(params, false)
		if !equalFoldASCII(name, "permessage-deflate") || !ok {
			return false
		}
		noCtx := mode == CompressionNoContextTakeover
		cfg = newDeflateConfig(!p.clientNoCtx && !noCtx, !p.serverNoCtx && !noCtx, p.serverBits, threshold)
		dst = append(dst, "permessage-deflate"...)
		if !cfg.writeContext {
			dst = append(dst, "; server_no_context_takeover"...)
		}
		if !cfg.readContext {
			dst = append(dst, "; client_no_context_takeover"...)
		}
		if p.serverBits != 0 {
			dst = strconv.AppendInt(append(dst, "; server_max_window_bits="...), int64(p.serverBits), 10)
		}
		return true
	})
	if !cfg.enabled {
		return nil, cfg
	}
	return dst, cfg
}

// deflateOffer returns the extension a client offers for mode.
func deflateOffer(mode CompressionMode) string {
	switch mode {
	case CompressionNoContextTakeover:
		return "permessage-deflate; client_no_context_takeover; server_no_context_takeover; client_max_window_bits"
	case CompressionContextTakeover:
		return "permessage-deflate; client_max_window_bits"
	}
	return ""
}

var errBadExtension = errors.New("websocket: server selected an unacceptable extension")

// acceptDeflate interprets the extensions a server selected in response to
// deflateOffer(mode).
func acceptDeflate(mode CompressionMode, threshold int, values [][]byte) (deflateConfig, error) {
	var cfg deflateConfig
	n := 0
	bad := deflateItems(values, func(name, params []byte) bool {
		n++
		p, ok := parseDeflate(params, true)
		noCtx := mode == CompressionNoContextTakeover
		if n > 1 || mode == CompressionDisabled || !equalFoldASCII(name, "permessage-deflate") || !ok || (noCtx && !p.serverNoCtx) {
			return true
		}
		cfg = newDeflateConfig(!p.serverNoCtx, !p.clientNoCtx && !noCtx, p.clientBits, threshold)
		return false
	})
	if bad || (n == 0 && len(values) > 0) {
		return deflateConfig{}, errBadExtension
	}
	return cfg, nil
}

var (
	flateWriters [8]sync.Pool // *flate.Writer by window bits 8 to 14; the last pool holds the default window.
	flateReaders sync.Pool    // io.ReadCloser from flate.NewReader.

	noSource = bytes.NewReader(nil)
)

// deflater compresses outgoing messages; it is the compressor's destination.
// The compressor is held from the first chunk of a message to its last; the
// dictionary only grows by what was sent, under frameMu.
type deflater struct {
	fw   *flate.Writer
	dst  *buffer
	dict []byte // Our context, the last deflateDict bytes sent, with room to slide.
}

func (d *deflater) Write(p []byte) (int, error) {
	d.dst.b = append(d.dst.b, p...)
	return len(p), nil
}

// compress appends payload compressed to out. The last chunk of a message
// ends with a sync marker, which is dropped (RFC 7692 section 7.2.1).
func (c *Conn) compress(out *buffer, payload []byte, final bool) {
	d := &c.def
	cfg := &c.cfg.deflate
	if d.fw == nil {
		d.fw = getFlateWriter(cfg.writeBits)
		var dict []byte
		if cfg.writeContext && cfg.writeBits == 0 {
			dict = lastN(d.dict, deflateDict)
		}
		d.fw.ResetDict(d, dict)
	}
	d.dst = out
	_, _ = d.fw.Write(payload)
	_ = d.fw.Flush()
	d.dst = nil
	if final {
		out.b = out.b[:len(out.b)-len(deflateTail)]
	}
}

// compressed accounts for a compressed chunk: a sent one enters the
// dictionary, and the compressor is released after the last chunk or a
// failure.
func (c *Conn) compressed(payload []byte, final, sent bool) {
	d := &c.def
	cfg := &c.cfg.deflate
	if sent && cfg.writeContext && cfg.writeBits == 0 {
		d.dict = slide(d.dict, payload, deflateDict)
	}
	if sent && !final {
		return
	}
	d.fw.ResetDict(io.Discard, nil) // Reset alone keeps the dictionary.
	flateWriters[flatePool(cfg.writeBits)].Put(d.fw)
	d.fw = nil
}

func flatePool(bits int) int {
	if bits == 0 {
		return len(flateWriters) - 1
	}
	return bits - 8
}

// getFlateWriter returns a pooled compressor for a window of 1<<bits bytes,
// bits being 8 to 14, or for the default window when bits is 0.
func getFlateWriter(bits int) *flate.Writer {
	if w, ok := flateWriters[flatePool(bits)].Get().(*flate.Writer); ok {
		return w
	}
	if bits != 0 {
		w, _ := flate.NewWriterWindow(io.Discard, 1<<bits)
		return w
	}
	w, _ := flate.NewWriter(io.Discard, flate.BestSpeed)
	return w
}

// slide appends p to window, which keeps at least its last size bytes and is
// compacted once it holds twice that, so that bytes are copied twice at most.
func slide(window, p []byte, size int) []byte {
	if len(p) >= size {
		return append(window[:0], p[len(p)-size:]...)
	}
	if len(window)+len(p) > 2*size {
		window = window[:copy(window, window[len(window)+len(p)-size:])]
	}
	return append(window, p...)
}

func lastN(b []byte, n int) []byte {
	return b[max(0, len(b)-n):]
}

// inflater decompresses incoming messages. It is the decompressor's source,
// serving the frames of the current message and then inflateTail.
type inflater struct {
	c    *Conn
	ctx  context.Context //nolint:containedctx // Of the read call feeding the decompressor.
	err  error           // The transport error that ended fill, if any.
	fr   io.ReadCloser   // Pooled; nil between messages.
	buf  *buffer         // Payload read ahead within the current frame.
	win  *buffer         // The peer's context: the last deflateWindow bytes inflated, with room to slide.
	pos  int
	tail int
}

func (s *inflater) fill() error {
	c := s.c
	rd := &c.rd
	s.buf.b, s.pos = s.buf.b[:0], 0
	for rd.left == 0 {
		if rd.fin {
			if s.tail == 0 {
				return io.EOF
			}
			s.buf.b = append(s.buf.b, inflateTail[len(inflateTail)-s.tail:]...)
			s.tail = 0
			return nil
		}
		if err := c.nextFrame(s.ctx); err != nil {
			s.err = err
			return err
		}
	}
	s.buf.b = s.buf.b[:min(int(rd.left), cap(s.buf.b))] // #nosec G115
	if err := c.readFrameInto(s.ctx, s.buf.b); err != nil {
		s.err = err
		return err
	}
	return nil
}

// more reports whether the message has bytes the decompressor has not read.
// After a final block they are further blocks, or what is left of inflateTail,
// which decodes to nothing; the tail is not served to a message that ends
// with a final block.
func (s *inflater) more() bool {
	rd := &s.c.rd
	return s.pos < len(s.buf.b) || rd.left > 0 || !rd.fin
}

func (s *inflater) Read(p []byte) (int, error) {
	if s.pos == len(s.buf.b) {
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, s.buf.b[s.pos:])
	s.pos += n
	return n, nil
}

func (s *inflater) ReadByte() (byte, error) {
	if s.pos == len(s.buf.b) {
		if err := s.fill(); err != nil {
			return 0, err
		}
	}
	b := s.buf.b[s.pos]
	s.pos++
	return b, nil
}

// beginInflate sets up the decompressor for the compressed message whose
// first frame header was just read. The window is kept across messages only
// with context takeover.
func (c *Conn) beginInflate(ctx context.Context) {
	s := &c.inf
	s.c, s.ctx, s.err, s.tail = c, ctx, nil, len(inflateTail)
	s.buf = getBuf(deflateReadAhead)
	s.pos = 0
	if s.win == nil {
		s.win = getBuf(2 * deflateWindow)
	}
	if fr, ok := flateReaders.Get().(io.ReadCloser); ok {
		s.fr = fr
		s.reset()
	} else {
		s.fr = flate.NewReaderDict(s, lastN(s.win.b, deflateWindow))
	}
	c.rd.inflated = 0
}

// reset starts the decompressor over on the same source with the context so
// far, as after a final block inside a message.
func (s *inflater) reset() {
	_ = s.fr.(flate.Resetter).Reset(s, lastN(s.win.b, deflateWindow)) //nolint:forcetypeassert // flate readers implement Resetter.
}

func (c *Conn) endInflate() {
	s := &c.inf
	_ = s.fr.(flate.Resetter).Reset(noSource, nil) //nolint:forcetypeassert // flate readers implement Resetter.
	flateReaders.Put(s.fr)
	s.fr, s.ctx = nil, nil
	putBuf(s.buf)
	s.buf = nil
	if !c.cfg.deflate.readContext {
		putBuf(s.win)
		s.win = nil
	}
}

// inflate reads decompressed bytes of the current message into p, checking
// them as consumed does. io.EOF ends the message; any other error has closed
// the connection.
func (c *Conn) inflate(ctx context.Context, p []byte) (int, error) {
	s := &c.inf
	rd := &c.rd
	s.ctx = ctx
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, err := s.fr.Read(p)
		if n > 0 {
			rd.inflated += n
			if rd.inflated > c.cfg.maxMessageSize {
				return 0, c.fail(ctx, StatusMessageTooBig, "message too big")
			}
			s.win.b = slide(s.win.b, p[:n], deflateWindow)
			if rd.msgType == MessageText && !rd.utf8.feed(p[:n]) {
				return 0, c.fail(ctx, StatusInvalidFramePayloadData, "invalid UTF-8")
			}
			return n, nil
		}
		switch {
		case err == nil:
			continue
		case s.err != nil || c.isClosed():
			if c.isClosed() {
				return 0, c.closedErr()
			}
			return 0, c.fatal(ctx, s.err)
		case errors.Is(err, io.EOF) && s.more(): // A final block inside the message.
			s.reset()
		case errors.Is(err, io.EOF):
			c.endInflate()
			return 0, io.EOF
		default:
			return 0, c.fail(ctx, StatusInvalidFramePayloadData, "bad DEFLATE data")
		}
	}
}
