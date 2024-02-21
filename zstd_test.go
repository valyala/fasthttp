package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

func TestZstdBytesSerial(t *testing.T) {
	t.Parallel()

	if err := testZstdBytes(); err != nil {
		t.Fatal(err)
	}
}

func TestZstdBytesConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testZstdBytes); err != nil {
		t.Fatal(err)
	}
}

func testZstdBytes() error {
	for _, s := range compressTestcases {
		if err := testZstdBytesSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testZstdBytesSingleCase(s string) error {
	prefix := []byte("foobar")
	ZstdpedS := AppendZstdBytes(prefix, []byte(s))
	if !bytes.Equal(ZstdpedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when compressing %q: %q. Expecting %q", s, ZstdpedS[:len(prefix)], prefix)
	}

	unZstdedS, err := AppendUnzstdBytes(prefix, ZstdpedS[len(prefix):])
	if err != nil {
		return fmt.Errorf("unexpected error when uncompressing %q: %w", s, err)
	}
	if !bytes.Equal(unZstdedS[:len(prefix)], prefix) {
		return fmt.Errorf("unexpected prefix when uncompressing %q: %q. Expecting %q", s, unZstdedS[:len(prefix)], prefix)
	}
	unZstdedS = unZstdedS[len(prefix):]
	if string(unZstdedS) != s {
		return fmt.Errorf("unexpected uncompressed string %q. Expecting %q", unZstdedS, s)
	}
	return nil
}

func TestZstdCompressSerial(t *testing.T) {
	t.Parallel()

	if err := testZstdCompress(); err != nil {
		t.Fatal(err)
	}
}

func TestZstdCompressConcurrent(t *testing.T) {
	t.Parallel()

	if err := testConcurrent(10, testZstdCompress); err != nil {
		t.Fatal(err)
	}
}

func testZstdCompress() error {
	for _, s := range compressTestcases {
		if err := testZstdCompressSingleCase(s); err != nil {
			return err
		}
	}
	return nil
}

func testZstdCompressSingleCase(s string) error {
	var buf bytes.Buffer
	zw := acquireStacklessZstdWriter(&buf, CompressZstdDefault)
	if _, err := zw.Write([]byte(s)); err != nil {
		return fmt.Errorf("unexpected error: %w. s=%q", err, s)
	}
	releaseStacklessZstdWriter(zw, CompressZstdDefault)

	zr, err := acquireZstdReader(&buf)
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
	releaseZstdReader(zr)
	return nil
}
