package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"time"
)

var compressTestcases = func() []string {
	a := []string{
		"",
		"foobar",
		"выфаодлодл одлфываыв sd2 k34",
	}
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
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			err := f()
			if err != nil {
				ch <- fmt.Errorf("error in goroutine %d: %w", idx, err)
			}
			ch <- nil
		}(i)
	}
	for i := 0; i < concurrency; i++ {
		select {
		case err := <-ch:
			if err != nil {
				return err
			}
		case <-time.After(time.Second):
			return fmt.Errorf("timeout")
		}
	}
	return nil
}
