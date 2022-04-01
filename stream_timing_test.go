package fasthttp

import (
	"bufio"
	"io"
	"testing"
	"time"
)

func BenchmarkStreamReader1(b *testing.B) {
	benchmarkStreamReader(b, 1)
}

func BenchmarkStreamReader10(b *testing.B) {
	benchmarkStreamReader(b, 10)
}

func BenchmarkStreamReader100(b *testing.B) {
	benchmarkStreamReader(b, 100)
}

func BenchmarkStreamReader1K(b *testing.B) {
	benchmarkStreamReader(b, 1000)
}

func BenchmarkStreamReader10K(b *testing.B) {
	benchmarkStreamReader(b, 10000)
}

func benchmarkStreamReader(b *testing.B, size int) {
	src := createFixedBody(size)
	b.SetBytes(int64(size))

	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, size)
		ch := make(chan error, 1)
		sr := NewStreamReader(func(w *bufio.Writer) {
			for pb.Next() {
				if _, err := w.Write(src); err != nil {
					ch <- err
					return
				}
				if err := w.Flush(); err != nil {
					ch <- err
					return
				}
			}
			ch <- nil
		})
		for {
			if _, err := sr.Read(dst); err != nil {
				if err == io.EOF {
					break
				}
				b.Fatalf("unexpected error when reading from stream reader: %v", err)
			}
		}
		if err := sr.Close(); err != nil {
			b.Fatalf("unexpected error when closing stream reader: %v", err)
		}
		select {
		case err := <-ch:
			if err != nil {
				b.Fatalf("unexpected error from stream reader: %v", err)
			}
		case <-time.After(time.Second):
			b.Fatalf("timeout")
		}
	})
}
