package websocket

import (
	"context"
	"encoding/binary"
	"io"
	"time"
	"unicode/utf8"
)

// frameReader is the state of the frame being read and of the message it
// belongs to.
type frameReader struct {
	deadline   time.Time // Of the read call in progress; bounds its control replies.
	left       uint64
	msgLen     int
	inflated   int
	utf8       utf8State
	msgType    MessageType
	key        [4]byte
	keyPos     int
	fin        bool
	open       bool
	compressed bool
}

// Read reads one message. The returned bytes are owned by the connection and
// stay valid until the next Read or Reader.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	deadline := after(c.cfg.readTimeout)
	if err := c.acquire(c.readMu, ctx, deadline); err != nil {
		return 0, nil, err
	}
	defer c.readMu.release()
	if err := c.startRead(ctx, deadline); err != nil {
		return 0, nil, err
	}
	defer c.rdWatch.end()
	c.releaseReadBuf()
	typ, err := c.beginMessage(ctx)
	if err != nil {
		return 0, nil, c.fatal(ctx, err)
	}
	rd := &c.rd
	buf := getBuf(min(int(rd.left), readChunkSize)) // #nosec G115
	c.readBuf = buf
	if rd.compressed {
		buf, err = c.readInflated(ctx, buf)
		if err != nil {
			return 0, nil, err
		}
		return typ, buf.b, c.endMessage(ctx)
	}
	for {
		// Grow with the data, not the announced length; each step quadruples.
		for rd.left > 0 {
			n := len(buf.b)
			step := min(int(rd.left), readChunkSize) // #nosec G115
			if cap(buf.b)-n < step {
				buf = c.growReadBuf(buf, n+step)
			}
			buf.b = buf.b[:n+step]
			if err := c.readFrameInto(ctx, buf.b[n:]); err != nil {
				return 0, nil, c.fatal(ctx, err)
			}
		}
		if rd.fin {
			break
		}
		if err := c.nextFrame(ctx); err != nil {
			return 0, nil, c.fatal(ctx, err)
		}
	}
	if err := c.endMessage(ctx); err != nil {
		return 0, nil, err
	}
	return typ, buf.b, nil
}

// growReadBuf replaces buf with one holding at least n bytes, quadrupling so
// that growth follows the data rather than the announced length.
func (c *Conn) growReadBuf(buf *buffer, n int) *buffer {
	grown := getBuf(max(n, 4*cap(buf.b)))
	grown.b = append(grown.b, buf.b...)
	putBuf(buf)
	c.readBuf = grown
	return grown
}

// readInflated decompresses the current message into buf.
func (c *Conn) readInflated(ctx context.Context, buf *buffer) (*buffer, error) {
	for {
		if len(buf.b) == cap(buf.b) {
			buf = c.growReadBuf(buf, cap(buf.b)+1)
		}
		n, err := c.inflate(ctx, buf.b[len(buf.b):cap(buf.b)])
		buf.b = buf.b[:len(buf.b)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// Reader returns a reader for the next message. It is valid until the next
// Read or Reader, which discards whatever is left of the message; it must not
// be used afterwards.
func (c *Conn) Reader(ctx context.Context) (MessageType, io.Reader, error) {
	deadline := after(c.cfg.readTimeout)
	if err := c.acquire(c.readMu, ctx, deadline); err != nil {
		return 0, nil, err
	}
	defer c.readMu.release()
	if err := c.startRead(ctx, deadline); err != nil {
		return 0, nil, err
	}
	defer c.rdWatch.end()
	c.releaseReadBuf()
	typ, err := c.beginMessage(ctx)
	if err != nil {
		return 0, nil, c.fatal(ctx, err)
	}
	c.rd.open = true
	c.mr = msgReader{c: c, ctx: ctx}
	return typ, &c.mr, nil
}

type msgReader struct {
	c   *Conn
	ctx context.Context //nolint:containedctx // Deadline and cancellation for each Read.
	eof bool
}

func (r *msgReader) Read(p []byte) (int, error) {
	c := r.c
	if r.eof {
		return 0, io.EOF
	}
	deadline := after(c.cfg.readTimeout)
	if err := c.acquire(c.readMu, r.ctx, deadline); err != nil {
		return 0, err
	}
	defer c.readMu.release()
	rd := &c.rd
	if !rd.open {
		return 0, errReaderStale
	}
	if err := c.startRead(r.ctx, deadline); err != nil {
		return 0, err
	}
	defer c.rdWatch.end()
	if rd.compressed {
		n, err := c.inflate(r.ctx, p)
		if err == io.EOF {
			rd.open = false
			if err := c.endMessage(r.ctx); err != nil {
				return n, err
			}
			r.eof = true
		}
		return n, err
	}
	for rd.left == 0 {
		if rd.fin {
			rd.open = false
			if err := c.endMessage(r.ctx); err != nil {
				return 0, err
			}
			r.eof = true
			return 0, io.EOF
		}
		if err := c.nextFrame(r.ctx); err != nil {
			return 0, c.fatal(r.ctx, err)
		}
	}
	n, err := c.readPayload(r.ctx, p)
	if err != nil {
		return n, c.fatal(r.ctx, err)
	}
	return n, nil
}

// startRead arms the read watch for a call bounded by ctx and deadline. The
// caller holds readMu.
func (c *Conn) startRead(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, err := c.rdWatch.begin(ctx, deadline)
	if err != nil {
		return c.fatal(ctx, err)
	}
	c.rd.deadline = deadline
	return nil
}

// discardFrame reads the rest of the current frame through the normal
// accounting, keeping its checks.
func (c *Conn) discardFrame(ctx context.Context) error {
	rd := &c.rd
	buf := getBuf(min(int(rd.left), readChunkSize)) // #nosec G115
	defer putBuf(buf)
	for rd.left > 0 {
		n := min(int(rd.left), readChunkSize) // #nosec G115
		if err := c.readFrameInto(ctx, buf.b[:n]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) releaseReadBuf() {
	if c.readBuf != nil {
		putBuf(c.readBuf)
		c.readBuf = nil
	}
}

// beginMessage discards what is left of the previous message and reads the
// first frame header of the next one.
func (c *Conn) beginMessage(ctx context.Context) (MessageType, error) {
	rd := &c.rd
	rd.open = false
	if rd.msgType != 0 {
		if err := c.discardMessage(ctx); err != nil {
			return 0, err
		}
	}
	if err := c.nextFrame(ctx); err != nil {
		return 0, err
	}
	if rd.compressed {
		c.beginInflate(ctx)
	}
	return rd.msgType, nil
}

// discardMessage reads the rest of the current message with its checks; a
// compressed one is inflated so that the peer's context stays in step.
func (c *Conn) discardMessage(ctx context.Context) error {
	rd := &c.rd
	if rd.compressed {
		buf := getBuf(readChunkSize)
		defer putBuf(buf)
		for {
			if _, err := c.inflate(ctx, buf.b[:cap(buf.b)]); err != nil {
				if err == io.EOF {
					return c.endMessage(ctx)
				}
				return err
			}
		}
	}
	for {
		if rd.left > 0 {
			if err := c.discardFrame(ctx); err != nil {
				return err
			}
		}
		if rd.fin {
			return c.endMessage(ctx)
		}
		if err := c.nextFrame(ctx); err != nil {
			return err
		}
	}
}

func (c *Conn) endMessage(ctx context.Context) error {
	rd := &c.rd
	if rd.msgType == MessageText && !rd.utf8.done() {
		return c.fail(ctx, StatusInvalidFramePayloadData, "incomplete UTF-8 sequence")
	}
	rd.msgType = 0
	return nil
}

// nextFrame reads frame headers, serving control frames on the way, until a
// data frame of the current message starts. On a protocol violation it fails
// the connection and returns the resulting error.
func (c *Conn) nextFrame(ctx context.Context) error {
	rd := &c.rd
	h := c.hdr[:]
	for {
		if _, err := io.ReadFull(c.r, h[:2]); err != nil {
			return err
		}
		fin, compressed := h[0]&0x80 != 0, h[0]&rsv1 != 0
		op := h[0] & 0x0f
		if rsv := h[0] & 0x70; rsv != 0 && (!compressed || rsv != rsv1 || !c.cfg.deflate.enabled || (op != opText && op != opBinary)) {
			return c.fail(ctx, StatusProtocolError, "reserved bits set")
		}
		if masked := h[1]&0x80 != 0; masked == c.client {
			return c.fail(ctx, StatusProtocolError, "wrong mask bit")
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			if _, err := io.ReadFull(c.r, h[:2]); err != nil {
				return err
			}
			n = uint64(binary.BigEndian.Uint16(h[:2]))
			if n < 126 {
				return c.fail(ctx, StatusProtocolError, "non-minimal length encoding")
			}
		case 127:
			if _, err := io.ReadFull(c.r, h[:8]); err != nil {
				return err
			}
			n = binary.BigEndian.Uint64(h[:8])
			if n < 1<<16 || n>>63 != 0 {
				return c.fail(ctx, StatusProtocolError, "non-minimal length encoding")
			}
		}
		if !c.client {
			if _, err := io.ReadFull(c.r, rd.key[:]); err != nil {
				return err
			}
		}
		rd.keyPos = 0
		switch op {
		case opContinuation, opText, opBinary:
			if (op == opContinuation) == (rd.msgType == 0) {
				return c.fail(ctx, StatusProtocolError, "bad fragmentation")
			}
			if op != opContinuation {
				rd.msgType = MessageType(op)
				rd.msgLen = 0
				rd.utf8 = utf8State{}
				rd.compressed = compressed
			}
			if n > uint64(c.cfg.maxMessageSize-rd.msgLen) { // #nosec G115
				return c.fail(ctx, StatusMessageTooBig, "message too big")
			}
			rd.left, rd.fin = n, fin
			return nil
		case opClose, opPing, opPong:
			if !fin || n > maxControlPayload {
				return c.fail(ctx, StatusProtocolError, "bad control frame")
			}
			p := c.ctrlIn[:n]
			if _, err := io.ReadFull(c.r, p); err != nil {
				return err
			}
			if !c.client {
				maskBytes(rd.key, 0, p)
			}
			switch op {
			case opPing:
				if err := c.reply(ctx, opPong, p); err != nil {
					return err
				}
			case opPong:
				c.gotPong(p)
			default:
				return c.handleClose(ctx, p)
			}
		default:
			return c.fail(ctx, StatusProtocolError, "reserved opcode")
		}
	}
}

// readFrameInto reads the rest of the current frame, which must be exactly
// len(p) bytes.
func (c *Conn) readFrameInto(ctx context.Context, p []byte) error {
	if _, err := io.ReadFull(c.r, p); err != nil {
		return err
	}
	return c.consumed(ctx, p)
}

// readPayload reads up to len(p) bytes of the current frame's payload.
func (c *Conn) readPayload(ctx context.Context, p []byte) (int, error) {
	if uint64(len(p)) > c.rd.left { // #nosec G115
		p = p[:c.rd.left]
	}
	n, err := c.r.Read(p)
	if n > 0 {
		if cerr := c.consumed(ctx, p[:n]); cerr != nil {
			return n, cerr
		}
	}
	if err != nil && c.rd.left == 0 && c.rd.fin {
		// The message is complete; the transport's end is for the next read.
		err = nil
	}
	return n, err
}

func (c *Conn) consumed(ctx context.Context, p []byte) error {
	rd := &c.rd
	if !c.client {
		rd.keyPos = maskBytes(rd.key, rd.keyPos, p)
	}
	rd.left -= uint64(len(p)) // #nosec G115
	rd.msgLen += len(p)
	if rd.msgType == MessageText && !rd.compressed && !rd.utf8.feed(p) {
		return c.fail(ctx, StatusInvalidFramePayloadData, "invalid UTF-8")
	}
	return nil
}

func (c *Conn) handleClose(ctx context.Context, p []byte) error {
	ce := CloseError{Code: StatusNoStatusReceived}
	switch {
	case len(p) == 1:
		return c.fail(ctx, StatusProtocolError, "bad close payload")
	case len(p) >= 2:
		ce.Code = StatusCode(binary.BigEndian.Uint16(p))
		if !validReceivedCode(ce.Code) {
			return c.fail(ctx, StatusProtocolError, "bad close code")
		}
		if !utf8.Valid(p[2:]) {
			return c.fail(ctx, StatusInvalidFramePayloadData, "close reason is not UTF-8")
		}
		ce.Reason = string(p[2:])
		p = p[:2]
	}
	_ = c.reply(ctx, opClose, p)
	c.closeWith(ce)
	return c.closedErr()
}

// fail closes the connection after a violation by the peer, telling it why.
func (c *Conn) fail(ctx context.Context, code StatusCode, reason string) error {
	var buf [maxControlPayload]byte
	_ = c.reply(ctx, opClose, appendClosePayload(buf[:0], code, reason))
	c.closeWith(CloseError{Code: code, Reason: reason})
	return c.closedErr()
}

// utf8State validates UTF-8 across frame boundaries; pending holds the
// incomplete sequence carried over from the previous chunk.
type utf8State struct {
	pending [4]byte
	n       int
}

func (s *utf8State) feed(p []byte) bool {
	for s.n > 0 {
		// FullRune is also true for a sequence that can no longer be valid.
		if utf8.FullRune(s.pending[:s.n]) {
			if !utf8.Valid(s.pending[:s.n]) {
				return false
			}
			s.n = 0
			break
		}
		if len(p) == 0 {
			return true
		}
		s.pending[s.n] = p[0]
		s.n++
		p = p[1:]
	}
	// Hold back an incomplete trailing sequence.
	end := len(p)
	for i := 1; i <= 3 && i <= len(p); i++ {
		b := p[len(p)-i]
		if b < 0x80 {
			break
		}
		if b >= 0xC0 {
			if !utf8.FullRune(p[len(p)-i:]) {
				end = len(p) - i
				s.n = copy(s.pending[:], p[end:])
			}
			break
		}
	}
	return utf8.Valid(p[:end])
}

func (s *utf8State) done() bool {
	return s.n == 0
}
