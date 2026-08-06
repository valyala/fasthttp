package http2

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

const defaultWriteQueueBatches = 16

// flushWriter is the narrow output contract used by x/net/http2's Framer.
// Production connections use asyncFrameWriter; focused protocol tests may use
// bufio.Writer directly.
type flushWriter interface {
	io.Writer
	Flush() error
}

type frameWriteFailure struct {
	err error
}

type frameWriteBatch struct {
	buffer  *frameWriteBuffer
	barrier chan error
}

type frameWriteBuffer struct {
	data []byte
}

// asyncFrameWriter separates connection-state ownership from socket writes: one
// producer batches ordered frame bytes, one goroutine owns net.Conn.Write. It
// knows nothing about streams, flow control, or HPACK.
type asyncFrameWriter struct {
	conn              net.Conn
	batchSize         int
	noProgressTimeout time.Duration
	queue             chan frameWriteBatch
	space             chan struct{}
	available         chan *frameWriteBuffer
	stop              chan struct{}
	done              chan struct{}
	stopOnce          sync.Once
	failure           atomic.Pointer[frameWriteFailure]
	closing           atomic.Bool

	// sendMu makes queue shutdown atomic with respect to batch transfer. A
	// producer waiting for space never holds it, and physical I/O never uses it.
	sendMu sync.Mutex
	active *frameWriteBuffer // owned exclusively by the producer

	// coalesced joins small batches that queued up during the previous write
	// syscall into the next one. Owned by writeLoop.
	coalesced []byte

	// deadline bounds how long the current producer waits for queue space. Like
	// active it is owned exclusively by whichever goroutine holds the
	// connection's write slot, so it needs no synchronisation.
	deadline time.Time
}

func newAsyncFrameWriter(
	conn net.Conn,
	batchSize int,
	queueBatches int,
	noProgressTimeout time.Duration,
) *asyncFrameWriter {
	w := &asyncFrameWriter{
		conn:              conn,
		batchSize:         batchSize,
		noProgressTimeout: noProgressTimeout,
		queue:             make(chan frameWriteBatch, queueBatches),
		space:             make(chan struct{}, 1),
		available:         make(chan *frameWriteBuffer, queueBatches+2),
		stop:              make(chan struct{}),
		done:              make(chan struct{}),
	}
	w.active = w.acquireBuffer()
	go w.writeLoop()
	return w
}

// setDeadline records the deadline of the goroutine that currently owns the
// write slot. A zero value waits indefinitely.
func (w *asyncFrameWriter) setDeadline(deadline time.Time) {
	w.deadline = deadline
}

func (w *asyncFrameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, w.err()
	}
	if err := w.err(); err != nil {
		return 0, err
	}
	if w.closing.Load() {
		return 0, net.ErrClosed
	}

	written := 0
	for len(p) != 0 {
		if w.active == nil {
			w.active = w.acquireBuffer()
		}
		available := cap(w.active.data) - len(w.active.data)
		if available == 0 {
			if err := w.enqueueActive(); err != nil {
				return written, err
			}
			continue
		}
		amount := min(len(p), available)
		w.active.data = append(w.active.data, p[:amount]...)
		p = p[amount:]
		written += amount
		if len(w.active.data) == cap(w.active.data) {
			if err := w.enqueueActive(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *asyncFrameWriter) Flush() error {
	if err := w.err(); err != nil {
		return err
	}
	if w.closing.Load() {
		return net.ErrClosed
	}
	return w.enqueueActive()
}

func (w *asyncFrameWriter) enqueueActive() error {
	if w.active == nil || len(w.active.data) == 0 {
		return w.err()
	}

	batch := frameWriteBatch{buffer: w.active}
	if err := w.enqueue(batch); err != nil {
		return err
	}
	w.active = nil
	return nil
}

func (w *asyncFrameWriter) enqueue(batch frameWriteBatch) error {
	var expired <-chan time.Time
	if !w.deadline.IsZero() {
		timer := fasthttp.AcquireTimer(time.Until(w.deadline))
		defer fasthttp.ReleaseTimer(timer)
		expired = timer.C
	}
	for {
		w.sendMu.Lock()
		if w.closing.Load() {
			w.sendMu.Unlock()
			if err := w.err(); err != nil {
				return err
			}
			return net.ErrClosed
		}
		select {
		case w.queue <- batch:
			w.sendMu.Unlock()
			return nil
		default:
			w.sendMu.Unlock()
		}
		select {
		case <-w.space:
		case <-w.stop:
			if err := w.err(); err != nil {
				return err
			}
			return net.ErrClosed
		case <-expired:
			// The producer copied part of a frame into this batch, so the byte
			// stream cannot be resumed by a later writer. Abandoning it is only
			// safe if the connection dies with it.
			w.fail(fasthttpWriteTimeoutError{})
			return fasthttpWriteTimeoutError{}
		}
	}
}

// closeAndWait transfers the producer's final batch and waits for all accepted
// bytes to reach the connection. timeout bounds graceful shutdown; zero waits
// indefinitely.
func (w *asyncFrameWriter) closeAndWait(timeout time.Duration) error {
	// Flush enqueues, and enqueue waits for queue space. Without a deadline it
	// would ignore the timeout this function promises to honour.
	if timeout > 0 {
		w.setDeadline(time.Now().Add(timeout))
		defer w.setDeadline(time.Time{})
	}
	if err := w.Flush(); err != nil && !errors.Is(err, net.ErrClosed) {
		w.abort(err)
		return err
	}

	barrier := make(chan error, 1)
	w.sendMu.Lock()
	if w.closing.Swap(true) {
		w.sendMu.Unlock()
		return w.waitDone(timeout)
	}
	w.sendMu.Unlock()
	if !sendWriteBarrier(w.queue, w.stop, barrier, timeout) {
		w.recordFailure(fasthttpWriteTimeoutError{})
		w.stopOnce.Do(func() { close(w.stop) })
		_ = w.conn.Close()
		_ = w.waitDone(timeout)
		return w.err()
	}

	if err := w.waitWriteBarrier(barrier, timeout); err != nil {
		w.abort(err)
		return err
	}
	w.stopOnce.Do(func() { close(w.stop) })
	return w.waitDone(timeout)
}

// waitWriteBarrier waits for the write loop to reach the barrier, or to exit.
// discardQueued answers the barriers it drains, but one can land in the queue
// after its final pass, and then only done ever fires.
func (w *asyncFrameWriter) waitWriteBarrier(barrier <-chan error, timeout time.Duration) error {
	if timeout <= 0 {
		select {
		case err := <-barrier:
			return err
		case <-w.done:
			return w.err()
		}
	}
	timer := fasthttp.AcquireTimer(timeout)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case err := <-barrier:
		return err
	case <-w.done:
		return w.err()
	case <-timer.C:
		return fasthttpWriteTimeoutError{}
	}
}

func (w *asyncFrameWriter) waitDone(timeout time.Duration) error {
	if timeout <= 0 {
		<-w.done
		return w.err()
	}
	timer := fasthttp.AcquireTimer(timeout)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case <-w.done:
		return w.err()
	case <-timer.C:
		return fasthttpWriteTimeoutError{}
	}
}

func (w *asyncFrameWriter) abort(cause error) {
	w.fail(cause)
	<-w.done
}

func (w *asyncFrameWriter) fail(cause error) {
	w.sendMu.Lock()
	w.recordFailure(cause)
	w.closing.Store(true)
	w.stopOnce.Do(func() { close(w.stop) })
	w.sendMu.Unlock()
	_ = w.conn.Close()
}

func (w *asyncFrameWriter) writeLoop() {
	defer func() {
		w.discardQueued()
		close(w.done)
	}()
	var pending []*frameWriteBuffer
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		var batch frameWriteBatch
		select {
		case <-w.stop:
			return
		case batch = <-w.queue:
		}
		// Batches that queued while the previous syscall was in flight go out
		// in one write with this one.
		pending = pending[:0]
		total := 0
		var barrier chan error
		for {
			select {
			case w.space <- struct{}{}:
			default:
			}
			if batch.barrier != nil {
				barrier = batch.barrier
				break
			}
			pending = append(pending, batch.buffer)
			total += len(batch.buffer.data)
			if total >= w.batchSize {
				break
			}
			received := false
			select {
			case batch = <-w.queue:
				received = true
			default:
			}
			if !received {
				break
			}
		}
		if err := w.writePending(pending); err != nil {
			w.fail(err)
			if barrier != nil {
				barrier <- w.err()
			}
			return
		}
		if barrier != nil {
			barrier <- w.err()
		}
	}
}

func (w *asyncFrameWriter) writePending(pending []*frameWriteBuffer) error {
	defer func() {
		for _, buffer := range pending {
			w.releaseBuffer(buffer)
		}
	}()
	switch len(pending) {
	case 0:
		return nil
	case 1:
		return w.writeAll(pending[0].data)
	}
	// One buffer per write costs a syscall each; under TLS, a record each.
	w.coalesced = w.coalesced[:0]
	for _, buffer := range pending {
		w.coalesced = append(w.coalesced, buffer.data...)
	}
	return w.writeAll(w.coalesced)
}

// writeAll arms the write deadline per attempt and leaves it armed: the next
// write re-arms before touching the connection, and an armed deadline on an
// idle connection does nothing.
func (w *asyncFrameWriter) writeAll(data []byte) error {
	for len(data) != 0 {
		if w.noProgressTimeout > 0 {
			if err := w.conn.SetWriteDeadline(time.Now().Add(w.noProgressTimeout)); err != nil {
				return err
			}
		}
		n, err := w.conn.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (w *asyncFrameWriter) discardQueued() {
	for {
		select {
		case batch := <-w.queue:
			if batch.buffer != nil {
				w.releaseBuffer(batch.buffer)
			}
			if batch.barrier != nil {
				batch.barrier <- w.err()
			}
		default:
			return
		}
	}
}

func (w *asyncFrameWriter) recordFailure(err error) {
	w.failure.CompareAndSwap(nil, &frameWriteFailure{err: err})
}

func (w *asyncFrameWriter) err() error {
	failure := w.failure.Load()
	if failure == nil {
		return nil
	}
	return failure.err
}

func (w *asyncFrameWriter) acquireBuffer() *frameWriteBuffer {
	select {
	case buffer := <-w.available:
		buffer.data = buffer.data[:0:w.batchSize]
		return buffer
	default:
		return &frameWriteBuffer{data: make([]byte, 0, w.batchSize)}
	}
}

func (w *asyncFrameWriter) releaseBuffer(buffer *frameWriteBuffer) {
	buffer.data = buffer.data[:0:w.batchSize]
	select {
	case w.available <- buffer:
	default:
	}
}

// fasthttpWriteTimeoutError is private so the writer remains independent of
// root-package timeout values while still satisfying net.Error.
type fasthttpWriteTimeoutError struct{}

func (fasthttpWriteTimeoutError) Error() string   { return "http2: write timeout" }
func (fasthttpWriteTimeoutError) Timeout() bool   { return true }
func (fasthttpWriteTimeoutError) Temporary() bool { return true }

func sendWriteBarrier(
	queue chan<- frameWriteBatch,
	stop <-chan struct{},
	barrier chan error,
	timeout time.Duration,
) bool {
	if timeout <= 0 {
		select {
		case queue <- frameWriteBatch{barrier: barrier}:
			return true
		case <-stop:
			return false
		}
	}
	timer := fasthttp.AcquireTimer(timeout)
	defer fasthttp.ReleaseTimer(timer)
	select {
	case queue <- frameWriteBatch{barrier: barrier}:
		return true
	case <-stop:
		return false
	case <-timer.C:
		return false
	}
}
