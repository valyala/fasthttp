package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
)

// This is the only structural quota check: accepted-byte counts cannot prove
// that pooled backing arrays stay bounded. Block counts and pool classes may
// change as long as the documented capacity limits and behavior hold.
func TestBoundedStreamBackpressureCapacityAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		input := bytes.Repeat([]byte("b"), 10_000_000)
		r := NewBoundedStreamReader(func(w *bufio.Writer) {
			if _, err := w.Write(bytes.Repeat([]byte("a"), 64)); err != nil {
				done <- result{err: err}
				return
			}
			if err := w.Flush(); err != nil {
				done <- result{err: err}
				return
			}
			n, err := w.Write(input)
			done <- result{n, err}
		})
		defer streamClose(t, r)
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("large Write completed without a consumer: %v", got)
		default:
		}
		first := make([]byte, 1)
		if _, err := io.ReadFull(r, first); err != nil || first[0] != 'a' {
			t.Fatal(first, err)
		}
		synctest.Wait()
		p, ok := r.(*streamPipe)
		if !ok {
			t.Fatalf("unexpected source %T", r)
		}
		p.mu.Lock()
		blocks := append([]*streamBuffer{p.current}, p.queue[:]...)
		capacity := 0
		for _, block := range blocks {
			if block == nil {
				continue
			}
			capacity += cap(block.data)
			if cap(block.data) > 32*1024 {
				t.Errorf("block backing capacity=%d, exceeds 32 KiB", cap(block.data))
			}
		}
		p.mu.Unlock()
		if capacity > 160*1024 {
			t.Fatalf("active backing capacity=%d, exceeds 160 KiB", capacity)
		}
		streamClose(t, r)
		streamClose(t, r)
		got := <-done
		if got.err == nil || got.n <= 0 || got.n > 160*1024 {
			t.Fatalf("partial Write=%d,%v", got.n, got.err)
		}
		clear(input)
		var tail bytes.Buffer
		// Small uneven Read calls also exercise Close tails without WriterTo.
		if _, err := io.CopyBuffer(struct{ io.Writer }{&tail}, struct{ io.Reader }{r}, make([]byte, 257)); err != nil {
			t.Fatal(err)
		}
		want := append(bytes.Repeat([]byte("a"), 63), bytes.Repeat([]byte("b"), got.n)...)
		if !bytes.Equal(tail.Bytes(), want) {
			t.Fatal("accepted prefix changed after Close/input reuse")
		}
	})
}

func TestStreamPipeWriteCopiesAndReadThenWriteTo(t *testing.T) {
	p := newStreamPipe()
	src := []byte("first-second")
	if n, err := p.Write(src); n != len(src) || err != nil {
		t.Fatal(n, err)
	}
	clear(src)
	streamClose(t, p)
	first := make([]byte, 3)
	if _, err := io.ReadFull(p, first); err != nil {
		t.Fatal(err)
	}
	var tail bytes.Buffer
	n, err := p.WriteTo(&tail)
	if err != nil || n != 9 || string(first)+tail.String() != "first-second" {
		t.Fatal(n, err, string(first), tail.String())
	}
}

func TestStreamPipeConcurrentStreams(t *testing.T) {
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			data := bytes.Repeat([]byte("0123456789"), 100_000)
			r := NewBoundedStreamReader(func(w *bufio.Writer) {
				if _, err := w.Write(data); err != nil {
					t.Error(err)
				}
			})
			got, err := io.ReadAll(r)
			streamClose(t, r)
			if err != nil || !bytes.Equal(got, data) {
				t.Error("stream corrupted", err)
			}
		})
	}
	wg.Wait()
}

func TestStreamPipeWriterBoundariesAndResume(t *testing.T) {
	sentinel := errors.New("audit destination failed")
	for _, tc := range []struct {
		name   string
		failAt int
		n      int
		full   bool
		err    error
	}{
		{"zeroNil", 1, 0, false, nil},
		{"partialNil", 1, 3, false, nil},
		{"zeroError", 1, 0, false, sentinel},
		{"fullError", 1, 0, true, sentinel},
		{"multiBlockPartialError", 2, 17, false, sentinel},
		{"multiBlockFullError", 2, 0, true, sentinel},
	} {
		for _, resume := range []string{"Read", "WriteTo"} {
			t.Run(tc.name+"/"+resume, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					source := make([]byte, 100_000)
					for i := range source {
						source[i] = byte(i * 31)
					}
					sw, producerDone := streamCompatProducer(source, 64*1024, 1)
					r := NewBoundedStreamReader(sw)
					defer streamClose(t, r)
					wt, ok := r.(io.WriterTo)
					if !ok {
						t.Fatal("bounded reader lacks WriterTo")
					}
					w := streamBoundaryWriter{failAt: tc.failAt, n: tc.n, full: tc.full, err: tc.err}
					n, err := wt.WriteTo(&w)
					wantErr := tc.err
					if wantErr == nil {
						wantErr = io.ErrShortWrite
					}
					if !errors.Is(err, wantErr) || n != int64(w.dst.Len()) || w.calls != tc.failAt {
						t.Fatalf("WriteTo = %d, %v; accepted = %d; calls = %d", n, err, w.dst.Len(), w.calls)
					}
					if !bytes.Equal(w.dst.Bytes(), source[:n]) {
						t.Fatal("accepted prefix differs from source")
					}
					var tail []byte
					if resume == "Read" {
						tail, err = io.ReadAll(r)
					} else {
						var dst bytes.Buffer
						var copied int64
						copied, err = wt.WriteTo(&dst)
						tail = dst.Bytes()
						if copied != int64(len(tail)) {
							t.Fatalf("resumed WriteTo count = %d, bytes = %d", copied, len(tail))
						}
					}
					if err != nil || !bytes.Equal(tail, source[n:]) {
						t.Fatalf("resumed %s: %d bytes, %v; want %d bytes", resume, len(tail), err, len(source)-int(n))
					}
					if err := <-producerDone; err != nil {
						t.Fatal(err)
					}
				})
			})
		}
	}
}

type streamBoundaryWriter struct {
	dst    bytes.Buffer
	failAt int
	n      int
	full   bool
	err    error
	calls  int
}

func (w *streamBoundaryWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls != w.failAt {
		return w.dst.Write(p)
	}
	n := min(w.n, len(p))
	if w.full {
		n = len(p)
	}
	w.dst.Write(p[:n])
	return n, w.err
}

func TestStreamPipeEmptyReadCloseWakeup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finishProducer := make(chan struct{})
		r := NewBoundedStreamReader(func(w *bufio.Writer) { <-finishProducer })
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := r.Read(make([]byte, 1))
			done <- result{n, err}
		}()
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("empty live reader did not block: %d, %v", got.n, got.err)
		default:
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.n != 0 || !errors.Is(got.err, io.EOF) {
			t.Fatalf("empty closed reader = %d, %v", got.n, got.err)
		}
		close(finishProducer)
	})
}

func TestStreamPipeReadyReadDoesNotWaitForFutureData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready, finish := make(chan error, 1), make(chan struct{})
		r := streamGatedBurst(ready, finish)
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		first := make([]byte, 512)
		if _, err := io.ReadFull(r, first); err != nil || !bytes.Equal(first, bytes.Repeat([]byte("v"), 512)) {
			t.Fatalf("ready data = %q, %v", first, err)
		}
		close(finish)
		tail, err := io.ReadAll(r)
		if err != nil || len(tail) != 0 {
			t.Fatalf("tail = %d, %v", len(tail), err)
		}
		streamClose(t, r)
	})
}

func streamGatedBurst(ready chan<- error, finish <-chan struct{}) io.ReadCloser {
	return NewBoundedStreamReader(func(w *bufio.Writer) {
		for range 4 {
			if _, err := w.Write(bytes.Repeat([]byte("v"), 128)); err != nil {
				ready <- err
				return
			}
			if err := w.Flush(); err != nil {
				ready <- err
				return
			}
		}
		ready <- nil
		<-finish
	})
}

func TestStreamPipeFullQueueAcceptsTailSpace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		// Fill the queue, leaving room in its final block. Derive the fixture from
		// the queue length, without requiring a particular number of slots/classes.
		for range len(p.queue) - 1 {
			if _, err := p.Write(make([]byte, streamBufferSize)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := p.Write(make([]byte, streamBufferSize-1)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := p.Write(make([]byte, 1)); done <- err }()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			streamClose(t, p)
			<-done
			t.Fatal("free tail space blocked behind queue slot limit")
		}
		streamClose(t, p)
		got, err := io.ReadAll(p)
		if err != nil || len(got) != len(p.queue)*streamBufferSize {
			t.Fatal(len(got), err)
		}
	})
}

func TestStreamPipeDestinationOwnershipAndConcurrentClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		first := bytes.Repeat([]byte("a"), 64)
		if _, err := p.Write(first); err != nil {
			t.Fatal(err)
		}
		clear(first)
		entered, release := make(chan struct{}), make(chan struct{})
		w := streamHoldingWriter{entered: entered, release: release, t: t}
		done := make(chan error, 1)
		go func() { _, err := p.WriteTo(&w); done <- err }()
		<-entered
		for range 4 {
			input := bytes.Repeat([]byte("b"), 64)
			if _, err := p.Write(input); err != nil {
				t.Fatal(err)
			}
			clear(input)
		}
		// Close must return even while the destination holds its borrowed slice.
		streamClose(t, p)
		streamClose(t, p)
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		want := append(bytes.Repeat([]byte("a"), 64), bytes.Repeat([]byte("b"), 256)...)
		if !bytes.Equal(w.dst.Bytes(), want) {
			t.Fatal("borrowed data or accepted Close tail changed")
		}
	})
}

type streamHoldingWriter struct {
	entered, release chan struct{}
	dst              bytes.Buffer
	t                *testing.T
}

func (w *streamHoldingWriter) Write(b []byte) (int, error) {
	if w.entered != nil {
		snapshot := bytes.Clone(b)
		close(w.entered)
		w.entered = nil
		<-w.release
		if !bytes.Equal(b, snapshot) {
			w.t.Error("borrowed data changed while destination held it")
		}
	}
	return w.dst.Write(b)
}

func TestStreamBatchReadyFlushBeforeWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		for range 4 {
			if _, err := p.Write(bytes.Repeat([]byte("x"), 64)); err != nil {
				t.Fatal(err)
			}
		}
		var dst bytes.Buffer
		bw := bufio.NewWriter(&dst)
		codec := streamBatchTestCodec{w: bw}
		fw := flushWriter{wf: &codec, bw: bw}
		done := make(chan error, 1)
		go func() {
			n, err := p.WriteTo(&fw)
			if n != 256 {
				t.Errorf("copied %d bytes, want 256", n)
			}
			done <- err
		}()
		synctest.Wait()
		if codec.flushes != 1 || !bytes.Equal(dst.Bytes(), bytes.Repeat([]byte("x"), 256)) {
			t.Fatalf("flushes=%d, output=%d; ready data must flush before waiting", codec.flushes, dst.Len())
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestStreamBatchFlushError(t *testing.T) {
	p := newStreamPipe()
	if _, err := p.Write([]byte("event")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("codec flush failed")
	var dst bytes.Buffer
	bw := bufio.NewWriter(&dst)
	codec := streamBatchTestCodec{w: bw, flushErr: sentinel}
	fw := flushWriter{wf: &codec, bw: bw}
	n, err := p.WriteTo(&fw)
	if n != 5 || !errors.Is(err, sentinel) || codec.flushes != 1 {
		t.Fatal(n, err, codec.flushes)
	}
}

func TestStreamBatchWriteErrorNormalization(t *testing.T) {
	p := newStreamPipe()
	if _, err := p.Write([]byte("event")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("codec write rejected")
	var dst bytes.Buffer
	bw := bufio.NewWriter(&dst)
	codec := streamBatchTestRejectedCodec{err: sentinel}
	fw := flushWriter{wf: &codec, bw: bw}
	n, err := p.WriteTo(&fw)
	if n != 0 || !errors.Is(err, sentinel) || codec.writes != 1 || codec.flushes != 0 {
		t.Fatal(n, err, codec.writes, codec.flushes)
	}
}

// A stackless writer can return its previous count when high load rejects
// dispatch. Preserve flushWriter's existing normalization of codec errors.
type streamBatchTestRejectedCodec struct {
	err             error
	writes, flushes int
}

func (c *streamBatchTestRejectedCodec) Write([]byte) (int, error) { c.writes++; return 64, c.err }
func (c *streamBatchTestRejectedCodec) Flush() error              { c.flushes++; return c.err }

type streamBatchTestCodec struct {
	w        io.Writer
	flushes  int
	flushErr error
}

func (c *streamBatchTestCodec) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *streamBatchTestCodec) Flush() error {
	c.flushes++
	return c.flushErr
}

func TestBoundedStreamDefaultDynamicType(t *testing.T) {
	for _, entry := range []string{"constructor", "response", "request"} {
		t.Run(entry, func(t *testing.T) {
			sw := func(w *bufio.Writer) {
				if _, err := w.WriteString("legacy"); err != nil {
					t.Error(err)
				}
			}
			var r io.Reader
			switch entry {
			case "constructor":
				r = NewStreamReader(sw)
			case "response":
				resp := new(Response)
				resp.SetBodyStreamWriter(sw)
				r = resp.BodyStream()
			case "request":
				req := new(Request)
				req.SetBodyStreamWriter(sw)
				r = req.BodyStream()
			}
			// Legacy entry points retain the PipeConns endpoint and do not opt in.
			conn, ok := r.(net.Conn)
			if !ok {
				t.Fatalf("legacy stream type changed: %T", r)
			}
			defer streamClose(t, conn)
			if _, ok := r.(BodyWriterTo); ok {
				t.Fatalf("legacy stream opted into BodyWriterTo: %T", r)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != "legacy" {
				t.Fatal(string(got), err)
			}
		})
	}
}

type boundedSelectionExternal struct{ *bytes.Reader }

func (*boundedSelectionExternal) SupportsBodyWriteTo() bool { return false }

type boundedSelectionAllowed struct{ *bytes.Reader }

func (*boundedSelectionAllowed) SupportsBodyWriteTo() bool { return true }

type boundedSelectionWrapped struct {
	io.ReadCloser
	BodyWriterTo
}

func TestBoundedStreamCompressionSelection(t *testing.T) {
	// This tests the selector added by this patch, not existing codec dispatch.
	// In particular, SupportsBodyWriteTo says nothing about a byte-capacity bound.
	for _, kind := range []string{"bounded", "legacy", "external-opt-out", "external-opt-in", "wrapped-bounded"} {
		t.Run(kind, func(t *testing.T) {
			var source io.Reader
			switch kind {
			case "bounded":
				source = NewBoundedStreamReader(func(*bufio.Writer) {})
			case "legacy":
				source = NewStreamReader(func(*bufio.Writer) {})
			case "external-opt-out":
				source = &boundedSelectionExternal{bytes.NewReader(nil)}
			case "external-opt-in":
				source = &boundedSelectionAllowed{bytes.NewReader(nil)}
			case "wrapped-bounded":
				r := NewBoundedStreamReader(func(*bufio.Writer) {})
				wt, ok := r.(BodyWriterTo)
				if !ok {
					t.Fatalf("bounded reader lacks BodyWriterTo: %T", r)
				}
				source = boundedSelectionWrapped{r, wt}
			}
			started := false
			reader := newCompressedBodyStream(source, 0, func(*bufio.Writer, io.Reader, int) error { started = true; return nil })
			_, bounded := reader.(*boundedCompressedBodyStream)
			if bounded != (kind == "bounded") {
				t.Fatalf("%s selected %T", kind, reader)
			}
			if !bounded {
				if _, ok := reader.(*compressedBodyStream); !ok {
					t.Fatalf("legacy compression type changed: %T", reader)
				}
			}
			streamClose(t, reader)
			if started {
				t.Fatal("closing unread stream started compression")
			}
			if n, err := reader.Read(nil); n != 0 || err != io.ErrClosedPipe {
				t.Fatal(n, err)
			}
		})
	}
}

func BenchmarkBoundedStreamReader(b *testing.B) {
	for _, size := range []int{64, 4096, 1 << 20} {
		for _, constructor := range []struct {
			name string
			new  func(StreamWriter) io.ReadCloser
		}{
			{"legacy", NewStreamReader},
			{"bounded", NewBoundedStreamReader},
		} {
			for _, mode := range []string{"Read", "Copy"} {
				b.Run(fmt.Sprintf("%s/bytes=%d/%s", constructor.name, size, mode), func(b *testing.B) {
					writes := 3
					if size < 1<<20 {
						writes = 128
					}
					source := streamCompatBody(size)
					want := bytes.Repeat(source, writes)
					readBuf := make([]byte, streamBufferSize)
					b.SetBytes(int64(len(want)))
					b.ReportAllocs()
					for b.Loop() {
						sw, done := streamCompatProducer(want, size, 1)
						r := constructor.new(sw)
						var input io.Reader = r
						if mode == "Read" {
							input = struct{ io.Reader }{r}
						}
						dst := streamBenchmarkWriter{want: want}
						n, err := io.CopyBuffer(&dst, input, readBuf)
						streamClose(b, r)
						producerErr := <-done
						if err != nil || producerErr != nil || n != int64(len(want)) {
							b.Fatalf("copy=%d,%v producer=%v", n, err, producerErr)
						}
					}
				})
			}
		}
	}
}

type streamBenchmarkWriter struct {
	want []byte
	off  int
}

func (w *streamBenchmarkWriter) Write(p []byte) (int, error) {
	if len(p) > len(w.want)-w.off || !bytes.Equal(p, w.want[w.off:w.off+len(p)]) {
		return 0, fmt.Errorf("stream bytes differ at %d", w.off)
	}
	w.off += len(p)
	return len(p), nil
}
