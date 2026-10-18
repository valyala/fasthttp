package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
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
