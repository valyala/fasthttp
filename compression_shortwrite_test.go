package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// shortWriteAfterWriter accepts the first full writes and then short-writes
// every subsequent write without reporting an error. Compressed streams emit
// their header while consuming p and the remaining bytes (including the
// trailer) on Close, so a short write on Close is easy to miss.
type shortWriteAfterWriter struct {
	calls int
	full  int
}

func (w *shortWriteAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls <= w.full {
		return len(p), nil
	}
	return len(p) - 1, nil
}

// failThenShortWriter fails the first write with a sentinel error and
// short-writes the rest. The public helper must keep the original error
// instead of replacing it with the error from Close.
type failThenShortWriter struct {
	calls int
	err   error
}

func (w *failThenShortWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return 0, w.err
	}
	return len(p) - 1, nil
}

// compressionHelpers lists the public one-shot helpers together with the
// number of downstream writes they emit before Close. Gzip and Deflate emit
// their header while consuming p; Brotli and Zstd buffer everything and emit
// the whole stream on Close. Setting full to that number makes the short write
// land on the final output, not on the Write(p) call.
func compressionHelpers() []struct {
	name string
	full int
	fn   func(io.Writer, []byte) (int, error)
} {
	return []struct {
		name string
		full int
		fn   func(io.Writer, []byte) (int, error)
	}{
		{"Gzip", 1, WriteGzip},
		{"Deflate", 1, WriteDeflate},
		{"Brotli", 0, WriteBrotli},
		{"Zstd", 0, func(w io.Writer, p []byte) (int, error) {
			return WriteZstdLevel(w, p, CompressZstdDefault)
		}},
	}
}

func TestCompressionHelpersReportCloseShortWrite(t *testing.T) {
	payload := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 8)

	for _, h := range compressionHelpers() {
		t.Run(h.name, func(t *testing.T) {
			w := &shortWriteAfterWriter{full: h.full}
			n, err := h.fn(w, payload)
			if w.calls != h.full+1 {
				t.Fatalf("short write did not land on the final output: writes=%d, full=%d", w.calls, h.full)
			}
			if !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("helper returned (%d, %v), want io.ErrShortWrite from Close; writes=%d", n, err, w.calls)
			}
			if n != len(payload) {
				t.Fatalf("helper returned n=%d, want %d", n, len(payload))
			}
		})
	}
}

func TestCompressionHelpersPreserveWriteError(t *testing.T) {
	payload := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 8)
	sentinel := errors.New("write failed")

	for _, h := range compressionHelpers() {
		t.Run(h.name, func(t *testing.T) {
			w := &failThenShortWriter{err: sentinel}
			_, err := h.fn(w, payload)
			if !errors.Is(err, sentinel) {
				t.Fatalf("helper returned %v, want the original write error; writes=%d", err, w.calls)
			}
			if errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("helper replaced the original error with io.ErrShortWrite: %v", err)
			}
		})
	}
}
