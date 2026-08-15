package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"
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

func zstdEncodeWithFCS(t *testing.T, body []byte) []byte {
	t.Helper()

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	compressedBody := encoder.EncodeAll(body, nil)
	encoder.Close()
	return compressedBody
}

func TestEstimateUnzstdSize(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("a"), 11_000)
	withFCS := zstdEncodeWithFCS(t, body)
	withoutFCS := []byte{
		0x28, 0xb5, 0x2f, 0xfd, // zstd magic
		0x00,       // no frame content size and not a single segment
		0x00,       // 1 KiB window
		0x09, 0, 0, // last raw block containing one byte
		'x',
	}
	skippablePrefix := []byte{
		0x50, 0x2a, 0x4d, 0x18, // skippable frame magic
		0x03, 0, 0, 0, // payload size
		'f', 'o', 'o',
	}
	forgedFCS := []byte{
		0x28, 0xb5, 0x2f, 0xfd,
		0xe0,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
	invalid := []byte("not zstd")

	testCases := []struct {
		name string
		src  []byte
		want int
	}{
		{name: "frame content size", src: withFCS, want: len(body)},
		{name: "leading skippable frame", src: append(skippablePrefix, withFCS...), want: len(body)},
		{name: "without frame content size", src: withoutFCS, want: 2 * len(withoutFCS)},
		{name: "forged frame content size is clamped", src: forgedFCS, want: 4_000_000},
		{name: "invalid input", src: invalid, want: 2 * len(invalid)},
		{name: "empty input", src: nil, want: 0},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := estimateUnzstdSize(testCase.src); got != testCase.want {
				t.Fatalf("unexpected estimate %d. Expecting %d", got, testCase.want)
			}
		})
	}
}

func TestWriteUnzstdPresizesDestination(t *testing.T) {
	t.Parallel()

	body := moderatelyCompressibleBody(110000)
	compressedBody := zstdEncodeWithFCS(t, body)

	for _, maxBodySize := range []int{0, len(body), 2 * len(body)} {
		var bb bytebufferpool.ByteBuffer
		n, err := writeUnzstd(&bb, compressedBody, maxBodySize)
		if err != nil {
			t.Fatalf("unexpected error with maxBodySize=%d: %v", maxBodySize, err)
		}
		if n != len(body) {
			t.Fatalf("unexpected decompressed size %d with maxBodySize=%d. Expecting %d", n, maxBodySize, len(body))
		}
		if !bytes.Equal(bb.B, body) {
			t.Fatalf("unexpected decompressed body with maxBodySize=%d", maxBodySize)
		}
		// The destination has been pre-sized from the frame header, so
		// decompression must not have grown it beyond the initial reservation.
		if cap(bb.B) > 2*len(body) {
			t.Fatalf("destination buffer was reallocated with maxBodySize=%d: cap=%d, decompressed size=%d",
				maxBodySize, cap(bb.B), len(body))
		}
	}
}

func TestWriteUnzstdSmallMaxBodySize(t *testing.T) {
	t.Parallel()

	body := moderatelyCompressibleBody(110000)
	compressedBody := zstdEncodeWithFCS(t, body)

	// The pre-allocation must not exceed maxBodySize, and the body must still
	// be rejected as too large.
	var bb bytebufferpool.ByteBuffer
	if _, err := writeUnzstd(&bb, compressedBody, 100); err != ErrBodyTooLarge {
		t.Fatalf("unexpected error %v. Expecting %v", err, ErrBodyTooLarge)
	}
	if cap(bb.B) > 2*len(body) {
		t.Fatalf("unexpected destination buffer capacity %d", cap(bb.B))
	}
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
