package websocket

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"slices"
	"time"
)

// vectorMin is the payload size from which a server sends a header and its
// payload with one writev.
const vectorMin = 4096

// Write sends p as a single-frame message of type typ. The wait for another
// writer can end through ctx or WriteTimeout without closing the connection.
func (c *Conn) Write(ctx context.Context, typ MessageType, p []byte) error {
	if typ != MessageText && typ != MessageBinary {
		return errBadMessageType
	}
	if err := cmp.Or(c.closedErr(), ctx.Err()); err != nil {
		return err
	}
	deadline := after(c.cfg.writeTimeout)
	if err := c.acquire(c.msgMu, ctx, deadline); err != nil {
		return err
	}
	defer c.msgMu.release()
	return c.writeData(ctx, byte(typ), true, p, deadline) // #nosec G115
}

// writeData sends one data frame. The caller holds msgMu.
func (c *Conn) writeData(ctx context.Context, op byte, fin bool, p []byte, deadline time.Time) error {
	if c.vw != nil && len(p) >= vectorMin {
		h := c.wh[:frameHeaderLen(len(p), false)]
		putFrameHeader(h, op, fin, false, p)
		return c.writeRaw(ctx, h, p, deadline)
	}
	buf := getBuf(frameHeaderLen(len(p), c.client) + len(p))
	buf.b = appendFrame(buf.b, op, fin, c.client, p)
	err := c.writeRaw(ctx, buf.b, nil, deadline)
	putBuf(buf)
	return err
}

// Writer returns a writer for one message of type typ. Data is buffered and
// sent in fragments; a write larger than the buffer goes out as a fragment of
// its own, and Close sends what is left as the last one. Only one writer is
// open at a time: Write and further Writer calls block until it is closed,
// which a failed write also does.
func (c *Conn) Writer(ctx context.Context, typ MessageType) (io.WriteCloser, error) {
	if typ != MessageText && typ != MessageBinary {
		return nil, errBadMessageType
	}
	if err := c.acquire(c.msgMu, ctx, after(c.cfg.writeTimeout)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		c.msgMu.release()
		return nil, err
	}
	buf := getBuf(writerBufSize)
	buf.b = buf.b[:maxFrameHeader]
	return &msgWriter{c: c, ctx: ctx, buf: buf, op: byte(typ)}, nil // #nosec G115
}

const writerBufSize = maxFrameHeader + writeChunkSize

// msgWriter buffers a message as frame header space followed by payload; buf
// is nil once it is closed.
type msgWriter struct {
	c   *Conn
	ctx context.Context //nolint:containedctx // Deadline and cancellation for each flush.
	buf *buffer
	err error
	op  byte
}

// Write buffers p, flushing a full buffer only when more data arrives, so
// that a message of one chunk goes out in one frame.
func (w *msgWriter) Write(p []byte) (int, error) {
	if w.buf == nil {
		return 0, cmp.Or(w.err, errWriterClosed)
	}
	n := len(p)
	for len(p) > 0 {
		if len(w.buf.b) == writerBufSize {
			if err := w.flush(false); err != nil {
				w.finish(err)
				return n - len(p), err
			}
		}
		if len(w.buf.b) == maxFrameHeader && len(p) > writeChunkSize {
			if err := w.sent(w.c.writeData(w.ctx, w.op, false, p, after(w.c.cfg.writeTimeout))); err != nil {
				w.finish(err)
				return n - len(p), err
			}
			break
		}
		room := min(writerBufSize-len(w.buf.b), len(p))
		w.buf.b = append(w.buf.b, p[:room]...)
		p = p[room:]
	}
	return n, nil
}

func (w *msgWriter) Close() error {
	if w.buf == nil {
		return w.err
	}
	err := w.flush(true)
	w.finish(err)
	return err
}

// flush sends the buffered payload as a fragment.
func (w *msgWriter) flush(fin bool) error {
	payload := w.buf.b[maxFrameHeader:]
	start := maxFrameHeader - frameHeaderLen(len(payload), w.c.client)
	putFrameHeader(w.buf.b[start:maxFrameHeader], w.op, fin, w.c.client, payload)
	err := w.sent(w.c.writeRaw(w.ctx, w.buf.b[start:], nil, after(w.c.cfg.writeTimeout)))
	w.buf.b = w.buf.b[:maxFrameHeader]
	return err
}

// sent accounts for a fragment. A failed continuation leaves the peer
// mid-message, so it ends the connection; net.ErrClosed means the Close frame
// went out first.
func (w *msgWriter) sent(err error) error {
	if err != nil && w.op == opContinuation && !errors.Is(err, net.ErrClosed) {
		w.c.closeWith(err)
	}
	w.op = opContinuation
	return err
}

func (w *msgWriter) finish(err error) {
	w.err = err
	putBuf(w.buf)
	w.buf = nil
	w.c.msgMu.release()
}

// writeControl sends a control frame, bounded by WriteTimeout.
func (c *Conn) writeControl(ctx context.Context, op byte, p []byte) error {
	deadline := after(c.cfg.writeTimeout)
	if err := c.acquire(c.frameMu, ctx, deadline); err != nil {
		return err
	}
	defer c.frameMu.release()
	return c.writeFrame(ctx, appendFrame(c.ctrlBuf[:0], op, true, c.client, p), nil, deadline, false)
}

// reply sends a control frame from the reading goroutine, bounded by the
// earliest of five seconds, WriteTimeout and the read call's deadline and
// aborted with the read. It does not close the connection.
func (c *Conn) reply(ctx context.Context, op byte, p []byte) error {
	now := time.Now()
	deadline := earliest(now.Add(closeTimeout), after(c.cfg.writeTimeout), c.rd.deadline)
	if !deadline.After(now) {
		return os.ErrDeadlineExceeded
	}
	if err := c.acquire(c.frameMu, ctx, deadline); err != nil {
		return err
	}
	defer c.frameMu.release()
	return c.writeFrame(ctx, appendFrame(c.ctrlBuf[:0], op, true, c.client, p), nil, deadline, true)
}

// writeRaw writes one data frame, given whole or as a header and a payload
// for a vectored write.
func (c *Conn) writeRaw(ctx context.Context, frame, payload []byte, deadline time.Time) error {
	if err := c.acquire(c.frameMu, ctx, deadline); err != nil {
		return err
	}
	defer c.frameMu.release()
	return c.writeFrame(ctx, frame, payload, deadline, false)
}

// writeFrame writes one frame under frameMu. Nothing but a reply follows the
// Close frame, and a failure on the transport closes the connection unless
// the frame is a reply.
func (c *Conn) writeFrame(ctx context.Context, frame, payload []byte, deadline time.Time, reply bool) error {
	op := frame[0] & 0x0f
	if c.closeSent.Load() {
		if op == opClose {
			return nil
		}
		if !reply {
			return cmp.Or(c.closedErr(), net.ErrClosed)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := c.wrWatch.begin(ctx, deadline)
	if err == nil {
		if payload == nil {
			_, err = c.nc.Write(frame)
		} else {
			c.iovBuf = [2][]byte{frame, payload}
			c.iov = c.iovBuf[:]
			_, err = c.iov.WriteTo(c.vw)
			c.iovBuf = [2][]byte{}
		}
	}
	c.wrWatch.end()
	switch {
	case err == nil:
		if op == opClose {
			c.closeSent.Store(true)
		}
		return nil
	case reply:
		return err
	default:
		return c.fatal(ctx, err)
	}
}

func frameHeaderLen(n int, mask bool) int {
	l := 2
	switch {
	case n >= 1<<16:
		l += 8
	case n >= 126:
		l += 2
	}
	if mask {
		l += 4
	}
	return l
}

// putFrameHeader writes the header for payload into h, which must be
// frameHeaderLen bytes long, and masks payload in place when mask is set.
func putFrameHeader(h []byte, op byte, fin, mask bool, payload []byte) {
	h[0] = op
	if fin {
		h[0] |= 0x80
	}
	n := len(payload)
	switch {
	case n >= 1<<16:
		h[1] = 127
		binary.BigEndian.PutUint64(h[2:], uint64(n)) // #nosec G115
	case n >= 126:
		h[1] = 126
		binary.BigEndian.PutUint16(h[2:], uint16(n)) // #nosec G115
	default:
		h[1] = byte(n)
	}
	if mask {
		h[1] |= 0x80
		key := h[len(h)-4:]
		// ChaCha8 seeded by the OS is unpredictable, as RFC 6455 requires.
		binary.LittleEndian.PutUint32(key, rand.Uint32()) // #nosec G404
		maskBytes([4]byte(key), 0, payload)
	}
}

// appendFrame appends a complete frame carrying payload to dst.
func appendFrame(dst []byte, op byte, fin, mask bool, payload []byte) []byte {
	hl := frameHeaderLen(len(payload), mask)
	start := len(dst)
	dst = slices.Grow(dst, hl+len(payload))[:start+hl+len(payload)]
	copy(dst[start+hl:], payload)
	putFrameHeader(dst[start:start+hl], op, fin, mask, dst[start+hl:])
	return dst
}

func appendClosePayload(dst []byte, code StatusCode, reason string) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(code)) // #nosec G115
	return append(dst, reason...)
}

// maskBytes XORs b with key, starting at key offset pos, and returns the
// offset to continue from.
func maskBytes(key [4]byte, pos int, b []byte) int {
	if len(b) >= 8 {
		var k [8]byte
		for i := range k {
			k[i] = key[(pos+i)&3]
		}
		k64 := binary.LittleEndian.Uint64(k[:])
		for len(b) >= 32 {
			binary.LittleEndian.PutUint64(b, binary.LittleEndian.Uint64(b)^k64)
			binary.LittleEndian.PutUint64(b[8:], binary.LittleEndian.Uint64(b[8:])^k64)
			binary.LittleEndian.PutUint64(b[16:], binary.LittleEndian.Uint64(b[16:])^k64)
			binary.LittleEndian.PutUint64(b[24:], binary.LittleEndian.Uint64(b[24:])^k64)
			b = b[32:]
		}
		for len(b) >= 8 {
			binary.LittleEndian.PutUint64(b, binary.LittleEndian.Uint64(b)^k64)
			b = b[8:]
		}
	}
	for i := range b {
		b[i] ^= key[(pos+i)&3]
	}
	return (pos + len(b)) & 3
}
