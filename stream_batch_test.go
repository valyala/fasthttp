package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/synctest"
)

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
