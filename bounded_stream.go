package fasthttp

import (
	"bufio"
	"io"
	"sync"

	"github.com/valyala/fasthttp/fasthttputil"
)

// NewBoundedStreamReader returns a reader for sw with bounded copied payload.
//
// Writes and nonempty Flush calls may block until queued space is available.
// Four queued/filling blocks and one reader-held block each have at most
// 32 KiB capacity, so active copied payload is at most
// 160 KiB. This excludes the caller's input, bufio, compression state and idle
// pooled buffers. HTTP compression of this exact reader uses a second bounded
// bridge, for at most 320 KiB of active bridge payload.
// Wrapping the returned reader changes its dynamic type, so HTTP compression
// uses the legacy stream path and no longer has the combined 320 KiB bound,
// even if the wrapper forwards BodyWriterTo.
//
// Use Response.SetBodyStream(r, -1) to opt in. NewStreamReader and
// Response.SetBodyStreamWriter retain their existing behavior.
// The HTTP write path may batch ready data, flushing after writing at least
// 32 KiB in a batch or before waiting for more input.
// A successful nonempty Flush means the bridge has accepted the buffered data;
// it does not guarantee that a reader has consumed it or a remote client has
// received it. Accepted input may be reused after Write returns. Close leaves
// accepted data readable before EOF.
//
// As with NewStreamReader, sw must return on write errors, and the caller must
// close the returned reader when it no longer needs the data.
// Read and WriteTo must not be called concurrently; Close may run concurrently
// with either method.
func NewBoundedStreamReader(sw StreamWriter) io.ReadCloser {
	pr := newStreamPipe()
	var bw *bufio.Writer
	if v := boundedStreamWriterBufPool.Get(); v == nil {
		bw = bufio.NewWriter(pr)
	} else {
		bw = v.(*bufio.Writer) //nolint:forcetypeassert
		bw.Reset(pr)
	}
	go func() {
		sw(bw)
		bw.Flush()
		pr.Close()
		bw.Reset(nil)
		boundedStreamWriterBufPool.Put(bw)
	}()
	return pr
}

var boundedStreamWriterBufPool sync.Pool

// Each stream can queue four blocks and lend one more to its reader. The
// dedicated pool cannot retain oversized buffers from unrelated pipe writes.
const streamBufferSize = 32 * 1024

type streamBuffer struct {
	data []byte
	n    int
}

// Small flushed messages should not reserve a maximum-sized block. None of
// these pools can acquire an oversized backing array from another use case.
var streamBufferPools [3]sync.Pool

func streamBufferClass(n int) (int, int) {
	if n <= 1024 {
		return 0, 1024
	}
	if n <= 4096 {
		return 1, 4096
	}
	return 2, streamBufferSize
}

func acquireStreamBuffer(n int) *streamBuffer {
	i, size := streamBufferClass(n)
	if v := streamBufferPools[i].Get(); v != nil {
		return v.(*streamBuffer) //nolint:forcetypeassert
	}
	return &streamBuffer{data: make([]byte, size)}
}

func releaseStreamBuffer(b *streamBuffer) {
	i, _ := streamBufferClass(len(b.data))
	streamBufferPools[i].Put(b)
}

// streamPipe has one writer and one reader. Close may run concurrently with
// either, and leaves accepted data available to the reader before EOF.
type streamPipe struct {
	mu     sync.Mutex
	ready  sync.Cond
	queue  [4]*streamBuffer
	head   int
	count  int
	closed bool

	// Only the reader accesses the current block, including during WriteTo.
	current *streamBuffer
	offset  int
}

func newStreamPipe() *streamPipe {
	p := new(streamPipe)
	p.ready.L = &p.mu
	return p
}

// writableTail requires mu. Only queued blocks may grow: the reader can lend
// current.data to its destination until Write returns.
func (p *streamPipe) writableTail() *streamBuffer {
	if p.count == 0 {
		return nil
	}
	b := p.queue[(p.head+p.count-1)%len(p.queue)]
	if b.n == len(b.data) {
		return nil
	}
	return b
}

func (p *streamPipe) Write(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		p.mu.Lock()
		buf := p.writableTail()
		for buf == nil && p.count == len(p.queue) && !p.closed {
			p.ready.Wait()
			buf = p.writableTail()
		}
		if p.closed {
			p.mu.Unlock()
			return n, fasthttputil.ErrConnectionClosed
		}
		var written int
		if buf == nil {
			buf = acquireStreamBuffer(min(len(b), streamBufferSize))
			buf.n = copy(buf.data, b)
			written = buf.n
			p.queue[(p.head+p.count)%len(p.queue)] = buf
			p.count++
			p.ready.Signal()
		} else {
			written = copy(buf.data[buf.n:], b)
			buf.n += written
		}
		p.mu.Unlock()
		n += written
		b = b[written:]
	}
	return n, nil
}

// With mayBlock false, EOF also means that no block is ready yet.
func (p *streamPipe) next(mayBlock bool) error {
	if p.current != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.count == 0 && !p.closed {
		if !mayBlock {
			return io.EOF
		}
		p.ready.Wait()
	}
	if p.count == 0 {
		return io.EOF
	}
	p.current = p.queue[p.head]
	p.queue[p.head] = nil
	p.head = (p.head + 1) % len(p.queue)
	p.count--
	p.offset = 0
	p.ready.Signal()
	return nil
}

func (p *streamPipe) consumed(n int) {
	p.offset += n
	if p.offset == p.current.n {
		releaseStreamBuffer(p.current)
		p.current = nil
		p.offset = 0
	}
}

func (p *streamPipe) Read(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		if err := p.next(total == 0); err != nil {
			if total > 0 {
				return total, nil
			}
			return 0, err
		}
		n := copy(b, p.current.data[p.offset:p.current.n])
		p.consumed(n)
		total += n
		b = b[n:]
	}
	return total, nil
}

// HTTP framing and compression implement this internal batching contract.
// WriteTo flushes ready data before waiting for more input.
// Callers must pass only nonempty slices to writeUnflushed: an empty HTTP chunk
// terminates the body. streamPipe only queues nonempty data, and consumed
// blocks are released before the next call.
type streamBatchWriter interface {
	writeUnflushed([]byte) (int, error)
	flush() error
}

// WriteTo lets the HTTP chunk writer consume each block without copying it
// through copyBufPool. Ownership stays here until the destination Write returns.
func (p *streamPipe) WriteTo(w io.Writer) (int64, error) {
	var total int64
	bw, batched := w.(streamBatchWriter)
	batchBytes := 0
	for {
		if err := p.next(true); err != nil {
			if err == io.EOF {
				err = nil
			}
			return total, err
		}
		b := p.current.data[p.offset:p.current.n]
		var n int
		var err error
		if batched {
			n, err = bw.writeUnflushed(b)
		} else {
			n, err = w.Write(b)
		}
		p.consumed(n)
		total += int64(n)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return total, err
		}
		if batched {
			batchBytes += n
			if batchBytes >= streamBufferSize || p.next(false) != nil {
				if err := bw.flush(); err != nil {
					return total, err
				}
				batchBytes = 0
			}
		}
	}
}

func (*streamPipe) SupportsBodyWriteTo() bool { return true }

func (p *streamPipe) Close() error {
	p.mu.Lock()
	p.closed = true
	p.ready.Broadcast()
	p.mu.Unlock()
	return nil
}

// Only the exact private bounded source selects this wrapper. BodyWriterTo is
// a Read/WriteTo substitution contract, not evidence of a byte capacity bound.
type boundedCompressedBodyStream struct {
	compressedBodyStream
}

func newBoundedCompressedBodyStream(bodyStream io.Reader, level int, compress compressBodyStream) io.ReadCloser {
	return &boundedCompressedBodyStream{compressedBodyStream: compressedBodyStream{
		bodyStream: bodyStream,
		level:      level,
		compress:   compress,
		done:       make(chan struct{}),
	}}
}

func (s *boundedCompressedBodyStream) Read(p []byte) (int, error) {
	s.startOnce.Do(func() { s.r = NewBoundedStreamReader(s.write) })
	if s.r == nil {
		return 0, io.ErrClosedPipe
	}
	return s.r.Read(p)
}

func (s *boundedCompressedBodyStream) WriteTo(w io.Writer) (int64, error) {
	s.startOnce.Do(func() { s.r = NewBoundedStreamReader(s.write) })
	if s.r == nil {
		return 0, io.ErrClosedPipe
	}
	return io.Copy(w, s.r)
}

func (*boundedCompressedBodyStream) SupportsBodyWriteTo() bool { return true }

// These hooks are used only by the private bounded bridge's WriteTo. The
// ordinary chunkedBodyWriter.Write and flushWriter.Write paths stay unchanged.
func (cw *chunkedBodyWriter) writeUnflushed(p []byte) (int, error) {
	if err := writeChunkNoFlush(cw.w, p); err != nil {
		cw.err = err
		return 0, err
	}
	return len(p), nil
}

func (cw *chunkedBodyWriter) flush() error {
	if err := cw.w.Flush(); err != nil {
		cw.err = err
		return err
	}
	return nil
}

func (w *flushWriter) writeUnflushed(p []byte) (int, error) {
	n, err := w.wf.Write(p)
	if err != nil {
		// Match flushWriter.Write's codec-error normalization. A rejected
		// stackless dispatch can return a stale n. Compression aborts on this
		// error; the reported count cannot be used to retry the codec input.
		return 0, err
	}
	return n, nil
}

func (w *flushWriter) flush() error {
	if err := w.wf.Flush(); err != nil {
		return err
	}
	return w.bw.Flush()
}
