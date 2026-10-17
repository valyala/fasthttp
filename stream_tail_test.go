package fasthttp

import (
	"bytes"
	"io"
	"testing"
	"testing/synctest"
)

func TestStreamPipeQueuedTailCoalescesCopiedInput(t *testing.T) {
	p := newStreamPipe()
	var want []byte
	for i := range 4 {
		b := bytes.Repeat([]byte{byte('a' + i)}, 64)
		want = append(want, b...)
		if n, err := p.Write(b); n != len(b) || err != nil {
			t.Fatal(n, err)
		}
		clear(b)
	}
	if p.count != 1 || p.queue[0].n != 256 || cap(p.queue[0].data) != 1024 {
		t.Fatalf("queued=%d, first length=%d capacity=%d", p.count, p.queue[0].n, cap(p.queue[0].data))
	}
	p.Close()
	got, err := io.ReadAll(p)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("queued input changed", err)
	}
}

func TestStreamPipeFullQueueAcceptsTailSpace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		for range 3 {
			if _, err := p.Write(make([]byte, streamBufferSize)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := p.Write(make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := p.Write(make([]byte, 64)); done <- err }()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			p.Close()
			<-done
			t.Fatal("free tail space blocked behind queue slot limit")
		}
		if p.count != 4 || p.queue[3].n != 128 {
			t.Fatal("tail was not extended")
		}
		p.Close()
		got, err := io.ReadAll(p)
		if err != nil || len(got) != 3*streamBufferSize+128 {
			t.Fatal(len(got), err)
		}
	})
}

func TestStreamPipeTailPartialWriteCloseAndCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		if _, err := p.Write(bytes.Repeat([]byte("a"), 64)); err != nil {
			t.Fatal(err)
		}
		src := bytes.Repeat([]byte("b"), 10_000_000)
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() { n, err := p.Write(src); done <- result{n, err} }()
		synctest.Wait()
		first := make([]byte, 1)
		if _, err := p.Read(first); err != nil || first[0] != 'a' {
			t.Fatal(first, err)
		}
		synctest.Wait()
		p.mu.Lock()
		capacity := cap(p.current.data)
		for _, b := range p.queue {
			if b != nil {
				capacity += cap(b.data)
			}
		}
		p.mu.Unlock()
		if capacity != 1024+4*streamBufferSize || capacity > 5*streamBufferSize {
			t.Fatalf("active payload capacity=%d", capacity)
		}
		p.Close()
		got := <-done
		if want := 1024 - 64 + 4*streamBufferSize; got.n != want || got.err == nil {
			t.Fatalf("partial Write=(%d,%v), want %d,error", got.n, got.err, want)
		}
		clear(src)
		tail, err := io.ReadAll(p)
		want := append(bytes.Repeat([]byte("a"), 63), bytes.Repeat([]byte("b"), got.n)...)
		if err != nil || !bytes.Equal(tail, want) {
			t.Fatal("accepted partial tail changed", err)
		}
		p.Close()
	})
}

type streamTailHoldingWriter struct {
	entered, release chan struct{}
	want             []byte
	t                *testing.T
}

func (w *streamTailHoldingWriter) Write(b []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)
		w.entered = nil
		<-w.release
		if !bytes.Equal(b, w.want) {
			w.t.Errorf("reader-owned data changed while destination held it: %q", b)
		}
	}
	return len(b), nil
}

func TestStreamPipeQueuedTailNeverExtendsReaderOwnedBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStreamPipe()
		if _, err := p.Write(bytes.Repeat([]byte("a"), 64)); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := p.WriteTo(&streamTailHoldingWriter{entered, release, bytes.Repeat([]byte("a"), 64), t})
			done <- err
		}()
		<-entered
		for range 4 {
			b := bytes.Repeat([]byte("b"), 64)
			if _, err := p.Write(b); err != nil {
				t.Fatal(err)
			}
			clear(b)
		}
		if p.current.n != 64 || p.count != 1 || p.queue[p.head].n != 256 {
			t.Fatalf("current=%d, queued=%d", p.current.n, p.count)
		}
		p.Close()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
