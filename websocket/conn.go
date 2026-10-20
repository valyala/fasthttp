package websocket

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// DefaultMaxMessageSize is the message size limit used when MaxMessageSize is zero.
const DefaultMaxMessageSize = 4 << 20

const (
	opContinuation byte = 0
	opText         byte = 1
	opBinary       byte = 2
	opClose        byte = 8
	opPing         byte = 9
	opPong         byte = 10

	maxControlPayload = 125
	maxFrameHeader    = 14
	readChunkSize     = 64 << 10
	writeChunkSize    = 4096 - maxFrameHeader // A Writer's buffer fills a pool class.
	closeTimeout      = 5 * time.Second
)

var (
	errBadMessageType = errors.New("websocket: invalid message type")
	errBadCloseCode   = errors.New("websocket: invalid close code or reason")
	errWriterClosed   = errors.New("websocket: writer is closed")
	errReaderStale    = errors.New("websocket: reader invalidated by a later Read or Reader")

	aLongTimeAgo = time.Unix(1, 0)
)

// buffer is a pooled byte slice; the pointer keeps Put from allocating.
type buffer struct{ b []byte }

const (
	minBufBits = 12 // 4 KiB
	bufClasses = 15 // Up to 64 MiB.
)

// bufPools holds buffers in power-of-two size classes.
var bufPools [bufClasses]sync.Pool

// bufClass returns the class whose buffers hold n bytes; bufClasses means n
// is too large to pool.
func bufClass(n int) int {
	if n <= 1<<minBufBits {
		return 0
	}
	return bits.Len(uint(n-1)) - minBufBits // #nosec G115
}

// getBuf returns an empty buffer with room for n bytes.
func getBuf(n int) *buffer {
	i := bufClass(n)
	if i >= bufClasses {
		return &buffer{b: make([]byte, 0, n)}
	}
	if b, ok := bufPools[i].Get().(*buffer); ok {
		return b
	}
	return &buffer{b: make([]byte, 0, 1<<(minBufBits+i))}
}

// putBuf returns b to its class; buffers of other sizes are dropped.
func putBuf(b *buffer) {
	n := cap(b.b)
	if i := bufClass(n); i < bufClasses && n == 1<<(minBufBits+i) {
		b.b = b.b[:0]
		bufPools[i].Put(b)
	}
}

type connConfig struct {
	maxMessageSize int
	readTimeout    time.Duration
	writeTimeout   time.Duration
	deflate        deflateConfig
}

// Conn is a WebSocket connection.
//
// Read, Reader and CloseRead must not be called concurrently with each other,
// and the readers and writers they hand out are not safe for concurrent use
// either; all other methods may be called concurrently. See the package
// documentation for the errors a closed connection reports.
type Conn struct {
	ctx    context.Context //nolint:containedctx // Exposed through Context.
	cancel context.CancelCauseFunc

	r  io.Reader
	nc net.Conn
	vw io.Writer // nc unwrapped to a transport with vectored writes, or nil.

	readBuf *buffer
	iov     net.Buffers // Over iovBuf; WriteTo consumes it.
	iovBuf  [2][]byte

	pongCh chan struct{}

	subprotocol string

	rdWatch ctxWatch
	wrWatch ctxWatch

	mr  msgReader
	def deflater
	inf inflater

	cfg connConfig

	rd frameReader

	readMu  lock
	msgMu   lock
	frameMu lock
	pingMu  lock

	closeMu sync.Mutex
	ncMu    sync.Mutex
	pongMu  sync.Mutex

	ctrlBuf [maxFrameHeader + maxControlPayload]byte
	ctrlIn  [maxControlPayload]byte
	hdr     [8]byte
	wh      [maxFrameHeader]byte // Header of a vectored write.

	pingPayload [8]byte

	closeSent atomic.Bool // Set under frameMu; no data frame may follow the Close frame.

	client bool
}

func newConn(nc net.Conn, r io.Reader, client bool, subprotocol string, cfg connConfig) *Conn {
	c := &Conn{
		r:           r,
		nc:          nc,
		pongCh:      make(chan struct{}, 1),
		subprotocol: subprotocol,
		readMu:      newLock(),
		msgMu:       newLock(),
		frameMu:     newLock(),
		pingMu:      newLock(),
		cfg:         cfg,
		client:      client,
	}
	if c.cfg.maxMessageSize <= 0 {
		c.cfg.maxMessageSize = DefaultMaxMessageSize
	}
	c.ctx, c.cancel = context.WithCancelCause(context.Background())
	c.rdWatch.nc = nc
	c.wrWatch.nc = nc
	c.wrWatch.write = true
	if !client {
		c.vw = vectorWriter(nc)
	}
	return c
}

// unwrapHijacked returns the connection under fasthttp's hijacked conn.
func unwrapHijacked(nc net.Conn) net.Conn {
	if u, ok := nc.(interface{ UnsafeConn() net.Conn }); ok {
		return u.UnsafeConn()
	}
	return nc
}

// vectorWriter returns the TCP or Unix socket under nc, whose writev sends a
// header and payload at once, or nil.
func vectorWriter(nc net.Conn) io.Writer {
	switch raw := unwrapHijacked(nc); raw.(type) {
	case *net.TCPConn, *net.UnixConn:
		return raw
	}
	return nil
}

// Context returns a context that is cancelled when the connection is closed.
// context.Cause reports the error that closed it.
func (c *Conn) Context() context.Context {
	return c.ctx
}

// Subprotocol returns the negotiated subprotocol, or "" if none was.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

func (c *Conn) isClosed() bool {
	return c.closedErr() != nil
}

func (c *Conn) closedErr() error {
	return context.Cause(c.ctx)
}

// closeWith records err as the reason and closes the transport once; ncMu is
// held meanwhile so shutdown can wait for it.
func (c *Conn) closeWith(err error) {
	c.closeMu.Lock()
	if c.isClosed() {
		c.closeMu.Unlock()
		return
	}
	c.cancel(err)
	c.ncMu.Lock()
	c.closeMu.Unlock()
	c.rdWatch.stopWatch()
	c.wrWatch.stopWatch()
	// A hijacked conn's Close is a no-op until the handler returns; the
	// deadline unblocks reads and writes meanwhile.
	_ = c.nc.SetDeadline(aLongTimeAgo)
	closeTransport(c.nc)
	c.ncMu.Unlock()
}

// closeTransport closes the socket under a TLS connection before nc itself, so
// close_notify cannot stall on a peer that no longer reads.
func closeTransport(nc net.Conn) {
	if tc, ok := unwrapHijacked(nc).(interface{ NetConn() net.Conn }); ok {
		_ = tc.NetConn().Close()
	}
	_ = nc.Close()
}

// shutdown closes the connection and waits until no goroutine of this package
// still uses the transport, after which fasthttp may recycle it.
func (c *Conn) shutdown() {
	c.closeWith(net.ErrClosed)
	c.ncMu.Lock()
	c.ncMu.Unlock() //nolint:gocritic,staticcheck // Waits for a concurrent closeWith.
	c.readMu.wait()
	c.frameMu.wait()
}

// fatal closes the connection after a failed read or write and returns the
// error callers see: ctx's error, a timeout, or StatusAbnormalClosure.
func (c *Conn) fatal(ctx context.Context, err error) error {
	if cerr := ctxCause(ctx); cerr != nil {
		err = cerr
	} else if !isTimeout(err) {
		err = CloseError{Code: StatusAbnormalClosure, Reason: err.Error()}
	}
	c.closeWith(err)
	return c.closedErr()
}

// ctxCause returns ctx's error, or DeadlineExceeded once its deadline passed.
func ctxCause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
		return context.DeadlineExceeded
	}
	return nil
}

func isTimeout(err error) bool {
	te, ok := errors.AsType[interface {
		error
		Timeout() bool
	}](err)
	return ok && te.Timeout()
}

// after returns the absolute deadline for timeout, or zero for no bound.
func after(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

// earliest returns the earliest non-zero deadline.
func earliest(deadlines ...time.Time) time.Time {
	var d time.Time
	for _, t := range deadlines {
		if !t.IsZero() && (d.IsZero() || t.Before(d)) {
			d = t
		}
	}
	return d
}

// CloseNow closes the connection without a closing handshake.
func (c *Conn) CloseNow() error {
	c.closeWith(net.ErrClosed)
	return nil
}

// Close performs the closing handshake: it sends a Close frame carrying code
// and reason, waits for the peer's Close frame and closes the connection.
// The wait ends at the ctx deadline; without one it lasts at most ReadTimeout,
// or five seconds when that is zero too. Close returns an error only for an
// invalid code or reason, or when the Close frame could not be sent and the
// connection is still usable. Otherwise it returns nil once the connection is
// closed, whether or not the peer answered; context.Cause(c.Context()) tells
// why.
func (c *Conn) Close(ctx context.Context, code StatusCode, reason string) error {
	if !validReceivedCode(code) || len(reason) > maxControlPayload-2 || !utf8.ValidString(reason) {
		return errBadCloseCode
	}
	if !c.closeSent.Load() {
		var buf [maxControlPayload]byte
		sctx, cancel := context.WithTimeout(ctx, closeTimeout)
		err := c.writeControl(sctx, opClose, appendClosePayload(buf[:0], code, reason))
		cancel()
		// Another Close may have sent the frame meanwhile.
		if err != nil && !c.closeSent.Load() {
			if c.isClosed() {
				return nil
			}
			return err
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		timeout := c.cfg.readTimeout
		if timeout <= 0 {
			timeout = closeTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c.readUntilClose(ctx, true)
	return nil
}

// CloseRead starts a goroutine that reads and handles control frames, so a
// connection used only for writing still answers pings and sees the peer's
// Close frame. ReadTimeout does not apply to it. A data message closes the
// connection with StatusPolicyViolation. Read and Reader must not be called
// afterwards; use Context to learn when the connection is closed.
func (c *Conn) CloseRead(ctx context.Context) {
	go c.readUntilClose(ctx, false)
}

// readUntilClose handles control frames until the connection ends, discarding
// or refusing data messages, and closes the connection with the result.
func (c *Conn) readUntilClose(ctx context.Context, discard bool) {
	err := c.acquire(c.readMu, ctx, time.Time{})
	if err == nil {
		err = c.startRead(ctx, time.Time{})
		for err == nil {
			if _, err = c.beginMessage(ctx); err == nil && !discard {
				err = c.fail(ctx, StatusPolicyViolation, "unexpected data message")
			}
		}
		c.rdWatch.end()
		c.readMu.release()
	}
	_ = c.fatal(ctx, err)
}

// Ping sends a ping and waits for the matching pong. Pongs are only seen while
// another goroutine is in Read or Reader, or after CloseRead. Only one Ping
// is in flight at a time.
func (c *Conn) Ping(ctx context.Context) error {
	if err := c.acquire(c.pingMu, ctx, time.Time{}); err != nil {
		return err
	}
	defer c.pingMu.release()
	c.pongMu.Lock()
	binary.BigEndian.PutUint64(c.pingPayload[:], binary.BigEndian.Uint64(c.pingPayload[:])+1)
	select {
	case <-c.pongCh:
	default:
	}
	c.pongMu.Unlock()
	if err := c.writeControl(ctx, opPing, c.pingPayload[:]); err != nil {
		return err
	}
	select {
	case <-c.pongCh:
		return nil
	case <-ctx.Done():
		return c.ctxErr(ctx)
	case <-c.ctx.Done():
		return c.closedErr()
	}
}

// ctxErr reports why a wait on ctx ended; a closed connection's reason wins.
func (c *Conn) ctxErr(ctx context.Context) error {
	return cmp.Or(c.closedErr(), ctx.Err())
}

func (c *Conn) gotPong(p []byte) {
	c.pongMu.Lock()
	if bytes.Equal(p, c.pingPayload[:]) {
		select {
		case c.pongCh <- struct{}{}:
		default:
		}
	}
	c.pongMu.Unlock()
}

// ctxWatch applies a call's deadline to one direction of nc and aborts the
// call when its context is cancelled. The AfterFunc is reused while calls
// share a Done channel.
type ctxWatch struct {
	nc     net.Conn
	done   <-chan struct{}
	stop   func() bool
	last   time.Time // The deadline set last, once set is true.
	mu     sync.Mutex
	gen    uint64
	active bool // A call that cancellations may abort is in progress.
	closed bool
	write  bool
	set    bool
}

// setDeadline sets the deadline of its direction, unless it is set already.
func (w *ctxWatch) setDeadline(t time.Time) error {
	if w.set && t.Equal(w.last) {
		return nil
	}
	w.last, w.set = t, true
	if w.write {
		return w.nc.SetWriteDeadline(t)
	}
	return w.nc.SetReadDeadline(t)
}

// begin arms the watch for a call bounded by ctx and deadline and returns the
// deadline it set; it fails on a closed connection or a done ctx.
func (w *ctxWatch) begin(ctx context.Context, deadline time.Time) (time.Time, error) {
	ctxDeadline, _ := ctx.Deadline()
	deadline = earliest(deadline, ctxDeadline)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return deadline, net.ErrClosed
	}
	if done := ctx.Done(); done != w.done {
		if w.stop != nil {
			w.stop()
			w.stop = nil
		}
		w.done = done
		w.gen++
		if done != nil {
			gen := w.gen
			w.stop = context.AfterFunc(ctx, func() { w.abort(gen) })
		}
	}
	if err := ctx.Err(); err != nil {
		return deadline, err
	}
	w.active = true
	return deadline, w.setDeadline(deadline)
}

func (w *ctxWatch) abort(gen uint64) {
	w.mu.Lock()
	if w.active && gen == w.gen && w.setDeadline(aLongTimeAgo) != nil {
		_ = w.nc.Close()
	}
	w.mu.Unlock()
}

func (w *ctxWatch) end() {
	w.mu.Lock()
	w.active = false
	w.mu.Unlock()
}

func (w *ctxWatch) stopWatch() {
	w.mu.Lock()
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.gen++
	w.closed = true
	w.mu.Unlock()
}

// lock is a mutex whose acquisition can be abandoned when a context ends or
// the connection closes.
type lock chan struct{}

func newLock() lock {
	return make(lock, 1)
}

// acquire takes l, giving up when the connection is closed, ctx ends or
// deadline passes.
func (c *Conn) acquire(l lock, ctx context.Context, deadline time.Time) error {
	if err := c.closedErr(); err != nil {
		return err
	}
	select {
	case l <- struct{}{}:
		return nil
	default:
	}
	var expired <-chan time.Time
	if !deadline.IsZero() {
		wait := time.Until(deadline)
		if wait <= 0 {
			return os.ErrDeadlineExceeded
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		expired = t.C
	}
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return c.ctxErr(ctx)
	case <-c.ctx.Done():
		return c.closedErr()
	case <-expired:
		return os.ErrDeadlineExceeded
	}
}

func (l lock) release() {
	<-l
}

// wait blocks until the holder, if any, releases l.
func (l lock) wait() {
	l <- struct{}{}
	<-l
}
