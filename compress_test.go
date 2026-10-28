package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

var compressTestcases = func() []string {
	a := make([]string, 0, 4)
	a = append(a,
		"",
		"foobar",
		"выфаодлодл одлфываыв sd2 k34",
	)
	bigS := createFixedBody(1e4)
	a = append(a, string(bigS))
	return a
}()

func TestGzipBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testGzipBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestGzipBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testGzipBytes); err != nil {
		t.Fatal(err)
	}
}

func TestDeflateBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testDeflateBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestDeflateBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testDeflateBytes); err != nil {
		t.Fatal(err)
	}
}

func testGzipBytes() error {
	for _, s := range compressTestcases {
		if err := testGzipBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testDeflateBytes() error {
	for _, s := range compressTestcases {
		if err := testDeflateBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testGzipBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	gzippedS := AppendGzipBytes(prefix, []byte(s))
	if !bytes.Equal(gzippedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, gzippedS[:len(prefix)], prefix)
	}

	gunzippedS, err := AppendGunzipBytes(prefix, gzippedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(gunzippedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, gunzippedS[:len(prefix)], prefix)
	}
	gunzippedS = gunzippedS[len(prefix):]
	if string(gunzippedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", gunzippedS, s)
	}
	return nil
}

func testDeflateBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	deflatedS := AppendDeflateBytes(prefix, []byte(s))
	if !bytes.Equal(deflatedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, deflatedS[:len(prefix)], prefix)
	}

	inflatedS, err := AppendInflateBytes(prefix, deflatedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(inflatedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, inflatedS[:len(prefix)], prefix)
	}
	inflatedS = inflatedS[len(prefix):]
	if string(inflatedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", inflatedS, s)
	}
	return nil
}

func TestGzipCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testGzipCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestGzipCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testGzipCompress); err != nil {
		t.Fatal(err)
	}
}

func TestFlateCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testFlateCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestFlateCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testFlateCompress); err != nil {
		t.Fatal(err)
	}
}

func testGzipCompress() error {
	for _, s := range compressTestcases {
		if err := testGzipCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testFlateCompress() error {
	for _, s := range compressTestcases {
		if err := testFlateCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testGzipCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessGzipWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	releaseStacklessGzipWriter(zw, CompressDefaultCompression)

	zr, err := acquireGzipReader(&buf)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseGzipReader(zr)
	return nil
}

func testFlateCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessDeflateWriter(&buf, CompressDefaultCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	releaseStacklessDeflateWriter(zw, CompressDefaultCompression)

	zr, err := acquireFlateReader(&buf)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	if string(body) != s {
		return fmt.Errorf("unexpected string after decompression: %q. Expecting %q", body, s)
	}
	releaseFlateReader(zr)
	return nil
}

func testConcurrent(concurrency int, f func() error) error {
	ch := make(chan error, concurrency)
	for i := range concurrency {
		go func(idx int) {
			err := f()
			if err != nil {
				ch <- fmt.Errorf("error in goroutine %d: %w", idx, err)
				return
			}
			ch <- nil
		}(i)
	}
	for range concurrency {
		select {
		case err := <-ch:
			if err != nil {
				return err
			}
		case <-time.After(testTimeout(time.Second)):
			return errors.New("timeout")
		}
	}
	return nil
}

var withLimitCodecs = []struct {
	name      string
	compress  func(dst, src []byte) []byte
	withLimit func(dst, src []byte, maxBodySize int) ([]byte, error)
}{
	{"gzip", AppendGzipBytes, AppendGunzipBytesWithLimit},
	{"deflate", AppendDeflateBytes, AppendInflateBytesWithLimit},
	{"brotli", AppendBrotliBytes, AppendUnbrotliBytesWithLimit},
	{"zstd", AppendZstdBytes, AppendUnzstdBytesWithLimit},
}

func TestAppendUncompressBytesWithLimit(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte("hello, world! "), 1000)
	prefix := []byte("prefix:")

	for _, codec := range withLimitCodecs {
		t.Run(codec.name, func(t *testing.T) {
			t.Parallel()

			compressed := codec.compress(nil, data)

			// Without a limit, or with one that isn't reached, the data is
			// appended to dst whole.
			for _, maxBodySize := range []int{0, -1, len(data), len(data) + 1, 10 * len(data)} {
				got, err := codec.withLimit(nil, compressed, maxBodySize)
				if err != nil {
					t.Fatalf("unexpected error with the limit %d: %v", maxBodySize, err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("unexpected data with the limit %d: %d bytes. Expecting %d bytes", maxBodySize, len(got), len(data))
				}

				got, err = codec.withLimit(append([]byte(nil), prefix...), compressed, maxBodySize)
				if err != nil {
					t.Fatalf("unexpected error with the limit %d and a prefix: %v", maxBodySize, err)
				}
				if !bytes.Equal(got, append(append([]byte(nil), prefix...), data...)) {
					t.Fatalf("unexpected data with the limit %d and a prefix: %d bytes", maxBodySize, len(got))
				}
			}

			// One byte over the limit is too much, and so is far over it. The
			// prefix of dst doesn't count towards the limit and stays in the
			// result, and no more than a byte over the limit is decoded.
			for _, maxBodySize := range []int{len(data) - 1, len(data) / 2, 1} {
				got, err := codec.withLimit(append([]byte(nil), prefix...), compressed, maxBodySize)
				if !errors.Is(err, ErrBodyTooLarge) {
					t.Fatalf("unexpected error %v with the limit %d. Expecting %v", err, maxBodySize, ErrBodyTooLarge)
				}
				if !bytes.HasPrefix(got, prefix) {
					t.Fatalf("the result lost dst with the limit %d: %q", maxBodySize, got[:min(len(got), 20)])
				}
				if extra := len(got) - len(prefix); extra > maxBodySize+1 {
					t.Fatalf("%d bytes were appended with the limit %d", extra, maxBodySize)
				}
			}

			// Data that isn't compressed fails the way it always did.
			if _, err := codec.withLimit(nil, []byte("not compressed data"), len(data)); err == nil || errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("unexpected error %v for data that isn't %s. Expecting a decoding error", err, codec.name)
			}
		})
	}
}

// A small input can expand to something huge. The limit has to be what stops
// it, not what is left of the memory.
func TestAppendUncompressBytesWithLimitBomb(t *testing.T) {
	t.Parallel()

	data := make([]byte, 8<<20)
	const maxBodySize = 64 << 10

	for _, codec := range withLimitCodecs {
		t.Run(codec.name, func(t *testing.T) {
			t.Parallel()

			compressed := codec.compress(nil, data)
			if len(compressed) > len(data)/100 {
				t.Fatalf("the test data should compress well: %d bytes", len(compressed))
			}
			got, err := codec.withLimit(nil, compressed, maxBodySize)
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("unexpected error %v. Expecting %v", err, ErrBodyTooLarge)
			}
			if len(got) > maxBodySize+1 {
				t.Fatalf("%d bytes were decoded with the limit %d", len(got), maxBodySize)
			}
		})
	}
}
