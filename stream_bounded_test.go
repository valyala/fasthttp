package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
)

func TestStreamPipeBoundedCloseTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		src := bytes.Repeat([]byte("x"), 10_000_000)
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() { n, err := p.Write(src); done <- result{n, err} }()
		synctest.Wait()
		p.mu.Lock()
		if p.count != len(p.queue) {
			t.Fatalf("queued %d", p.count)
		}
		for _, b := range p.queue {
			if len(b.data) != streamBufferSize {
				t.Fatal("oversized block")
			}
		}
		p.mu.Unlock()
		select {
		case <-done:
			t.Fatal("large write completed without a reader")
		default:
		}
		first := make([]byte, 1)
		if _, err := p.Read(first); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		p.mu.Lock()
		if p.count != 4 {
			t.Fatalf("queued %d after partial read", p.count)
		}
		p.mu.Unlock()
		if p.current == nil {
			t.Fatal("missing reader-owned block")
		}
		p.Close()
		got := <-done
		if got.n != 5*streamBufferSize || got.err == nil {
			t.Fatalf("Write = %d, %v", got.n, got.err)
		}
		tail, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(tail)+1 != got.n || !bytes.Equal(tail, src[1:got.n]) {
			t.Fatal("accepted tail was lost")
		}
		if p.current != nil || p.count != 0 {
			t.Fatal("drained blocks retained")
		}
		p.Close()
	})
}

func TestStreamPipeWriteCopiesAndReadThenWriteTo(t *testing.T) {
	p := newStreamPipe()
	src := []byte("first-second")
	if n, err := p.Write(src); n != len(src) || err != nil {
		t.Fatal(n, err)
	}
	clear(src)
	p.Close()
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

type streamPartialWriter struct {
	n   int
	err error
}

func (w streamPartialWriter) Write(p []byte) (int, error) { return min(w.n, len(p)), w.err }

func TestStreamPipeWriteToFailurePreservesTail(t *testing.T) {
	sentinel := errors.New("destination failed")
	for _, writeErr := range []error{sentinel, nil} {
		p := newStreamPipe()
		if _, err := p.Write([]byte("abcdefghij")); err != nil {
			t.Fatal(err)
		}
		p.Close()
		n, err := p.WriteTo(streamPartialWriter{3, writeErr})
		want := writeErr
		if want == nil {
			want = io.ErrShortWrite
		}
		if n != 3 || !errors.Is(err, want) {
			t.Fatal(n, err)
		}
		tail, err := io.ReadAll(p)
		if err != nil || string(tail) != "defghij" {
			t.Fatal(string(tail), err)
		}
	}
}

func TestStreamPipeCloseDoesNotWaitForDestination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		if _, err := p.Write([]byte("body")); err != nil {
			t.Fatal(err)
		}
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			_, err := p.WriteTo(streamGateWriter{entered, release})
			if err != nil {
				t.Error(err)
			}
		}()
		<-entered
		p.Close()
		close(release)
		<-done
	})
}

type streamGateWriter struct{ entered, release chan struct{} }

func (w streamGateWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

func TestBoundedStreamWriterFlushSmallMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flushed, finish := make(chan struct{}), make(chan struct{})
		r := NewBoundedStreamReader(func(w *bufio.Writer) {
			if _, err := w.WriteString("event"); err != nil {
				t.Error(err)
				return
			}
			if err := w.Flush(); err != nil {
				t.Error(err)
			}
			close(flushed)
			<-finish
		})
		<-flushed
		b := make([]byte, 5)
		if _, err := io.ReadFull(r, b); err != nil || string(b) != "event" {
			t.Fatal(string(b), err)
		}
		close(finish)
		if tail, err := io.ReadAll(r); err != nil || len(tail) != 0 {
			t.Fatal(string(tail), err)
		}
		r.Close()
	})
}

func TestBoundedStreamWriterClosedConsumerStopsProducer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan error, 1)
		r := NewBoundedStreamReader(func(w *bufio.Writer) {
			for {
				if _, err := w.Write(make([]byte, 2*streamBufferSize)); err != nil {
					done <- err
					return
				}
			}
		})
		synctest.Wait()
		r.Close()
		if err := <-done; err == nil {
			t.Fatal("missing close error")
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatal(err)
		}
	})
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
			r.Close()
			if err != nil || !bytes.Equal(got, data) {
				t.Error("stream corrupted", err)
			}
		})
	}
	wg.Wait()
}

func TestStreamBufferClassesStayBounded(t *testing.T) {
	for _, tc := range []struct{ n, capacity int }{{1, 1024}, {1024, 1024}, {1025, 4096}, {4096, 4096}, {4097, streamBufferSize}, {streamBufferSize, streamBufferSize}} {
		large := acquireStreamBuffer(streamBufferSize)
		releaseStreamBuffer(large)
		b := acquireStreamBuffer(tc.n)
		if cap(b.data) != tc.capacity {
			t.Fatalf("size %d: capacity %d, want %d", tc.n, cap(b.data), tc.capacity)
		}
		releaseStreamBuffer(b)
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
						r.Close()
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
