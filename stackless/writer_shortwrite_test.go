package stackless

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"testing"
)

// shortWriter accepts part of each write without reporting an error, so the
// wrapper must detect the short write itself.
type shortWriter struct{ limit int }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	return len(p), nil
}

// errorWriter returns a custom error along with a short write. The original
// error must be preserved instead of being replaced.
type errorWriter struct {
	limit int
	err   error
}

func (w *errorWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	return len(p), w.err
}

func newTestStacklessWriter(dst io.Writer) Writer {
	return NewWriter(dst, func(w io.Writer) Writer { return gzip.NewWriter(w) })
}

func TestStacklessWriterShortWrite(t *testing.T) {
	t.Run("write reports short write", func(t *testing.T) {
		sw := newTestStacklessWriter(&shortWriter{limit: 8})
		payload := bytes.Repeat([]byte("the quick brown fox "), 16)

		_, err := sw.Write(payload)
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write returned %v, want io.ErrShortWrite", err)
		}
		if err := sw.Close(); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Close returned %v, want io.ErrShortWrite", err)
		}
	})

	t.Run("close reports short write", func(t *testing.T) {
		sw := newTestStacklessWriter(&shortWriter{limit: 8})

		if err := sw.Close(); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Close returned %v, want io.ErrShortWrite", err)
		}
	})

	t.Run("writer error is preserved", func(t *testing.T) {
		want := errors.New("write failed")
		sw := newTestStacklessWriter(&errorWriter{limit: 8, err: want})
		payload := bytes.Repeat([]byte("the quick brown fox "), 16)

		_, err := sw.Write(payload)
		if !errors.Is(err, want) {
			t.Fatalf("Write returned %v, want the original error", err)
		}
		if errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write must not replace the original error with io.ErrShortWrite")
		}
	})

	t.Run("full write succeeds", func(t *testing.T) {
		var dst bytes.Buffer
		sw := newTestStacklessWriter(&dst)
		payload := bytes.Repeat([]byte("the quick brown fox "), 16)

		n, err := sw.Write(payload)
		if err != nil {
			t.Fatalf("Write returned unexpected error: %v", err)
		}
		if n != len(payload) {
			t.Fatalf("Write returned n=%d, want %d", n, len(payload))
		}
		if err := sw.Close(); err != nil {
			t.Fatalf("Close returned unexpected error: %v", err)
		}
		if dst.Len() == 0 {
			t.Fatal("no compressed bytes were written")
		}
	})
}
