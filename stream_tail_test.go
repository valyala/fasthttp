package fasthttp

import (
	"bytes"
	"io"
	"testing"
	"testing/synctest"
)

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
