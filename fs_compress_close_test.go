package fasthttp

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"
)

var errCloseFailed = errors.New("close failed while writing the trailer")

// failTrailerWriter counts writes made after the compressor is reset onto
// its destination. It is used to assert that a short write reaches the
// destination from both Flush and Close.
type failTrailerWriter struct {
	calls int
}

// writerToResetFile is an fs.File and an io.WriterTo. copyZeroAlloc calls its
// WriteTo with the stackless compressor, so the test can point that compressor
// at a writer that fails the final trailer write without touching the shared
// writer pools. It still copies a real payload first so Flush succeeds.
type writerToResetFile struct {
	data []byte
	fi   iofs.FileInfo
	dst  io.Writer
}

func (f *writerToResetFile) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func (f *writerToResetFile) WriteTo(w io.Writer) (int64, error) {
	// Buffer a real payload in the compressor, then swap its destination so
	// the subsequent Flush/Close writes fail. copyZeroAlloc itself succeeds,
	// which is exactly the case where the old code discarded the close error.
	if _, err := w.Write(f.data); err != nil {
		return 0, err
	}
	if sw, ok := w.(interface{ Reset(io.Writer) }); ok {
		sw.Reset(f.dst)
	}
	return int64(len(f.data)), nil
}

func (f *writerToResetFile) Stat() (iofs.FileInfo, error) { return f.fi, nil }
func (f *writerToResetFile) Close() error                 { return nil }

// TestFSCompressCloseErrorNeverPublishesTruncatedFile reproduces a truncated
// compressed stream being served with HTTP 200: the payload is copied and
// Flush succeeds, but writing the trailer on Close fails. The first error must
// be kept so the existing cleanup removes the temporary file instead of
// renaming it into the cache.
func TestFSCompressCloseErrorNeverPublishesTruncatedFile(t *testing.T) {
	fi, err := os.Stat("fs_test.go")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("compressible payload for the cache, repeated across the whole file\n")

	// gzip and brotli emit on both Flush and Close; zstd buffers everything
	// until Close. Fail only the final write so Flush succeeds for every codec
	// and the error can only come from the compressor close.
	for _, tc := range []struct {
		encoding string
		writes   int
	}{
		{"gzip", 2},
		{"br", 2},
		{"zstd", 1},
	} {
		t.Run(tc.encoding, func(t *testing.T) {
			dir := t.TempDir()
			compressedFilePath := filepath.Join(dir, "out."+tc.encoding)

			fw := &failTrailerWriter{}
			lastErr := &failLastWriteWriter{inner: fw, err: errCloseFailed, failAt: tc.writes}
			src := &writerToResetFile{data: append([]byte(nil), payload...), fi: fi, dst: lastErr}

			h := &fsHandler{}
			_, err := h.compressFileNolock(
				osRoot{}, src, fi, "fs_test.go", compressedFilePath, tc.encoding)
			if !errors.Is(err, errCloseFailed) {
				t.Fatalf("got %v, want the Close error to be reported", err)
			}

			if _, err := os.Stat(compressedFilePath); !errors.Is(err, iofs.ErrNotExist) {
				t.Fatalf("truncated compressed file was published into the cache: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary file %q was left behind", entries[0].Name())
			}
		})
	}
}

// failLastWriteWriter lets all writes up to failAt-1 succeed and fails the
// final one. failAt is the number of writes the codec makes to the destination
// on Flush+Close, so the failing write is always the compressor close. If the
// close error is discarded, each subtest fails instead of passing via Flush.
type failLastWriteWriter struct {
	inner  *failTrailerWriter
	err    error
	failAt int
}

func (w *failLastWriteWriter) Write(p []byte) (int, error) {
	w.inner.calls++
	if w.inner.calls < w.failAt {
		return len(p), nil
	}
	return 0, w.err
}
