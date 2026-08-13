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

// moderatelyCompressibleBody returns n bytes of text which zstd compresses with
// a ratio well below the one estimateUnzstdSize is willing to trust, so that the
// uncompressed size from the frame header is used as-is.
func moderatelyCompressibleBody(n int) []byte {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	pseudoRandomState := uint64(42)

	body := make([]byte, 0, n+8)
	for len(body) < n {
		pseudoRandomState = pseudoRandomState*6364136223846793005 + 1442695040888963407
		body = append(body, words[pseudoRandomState>>60&7]...)
		body = append(body, byte('0'+pseudoRandomState%10), ' ')
	}
	return body[:n]
}

func TestEstimateUnzstdSize(t *testing.T) {
	t.Parallel()

	body := moderatelyCompressibleBody(11000)
	compressedBody := zstdEncodeWithFCS(t, body)
	if size := estimateUnzstdSize(compressedBody); size != len(body) {
		t.Fatalf("unexpected estimate %d. Expecting %d", size, len(body))
	}

	// An uncompressed size implying a suspiciously high compression ratio is
	// clamped, even though such a ratio is legitimately reachable.
	highlyCompressibleBody := bytes.Repeat([]byte("foobar baz "), 1000000)
	compressedBody = zstdEncodeWithFCS(t, highlyCompressibleBody)
	expectedSize := 4_000_000
	if size := estimateUnzstdSize(compressedBody); size != expectedSize {
		t.Fatalf("unexpected estimate %d for a highly compressible body. Expecting %d", size, expectedSize)
	}

	// Streaming encoders don't know the body size upfront, so a compression
	// factor of 2 is assumed for the frames they produce. Bodies small enough to
	// be buffered whole are an exception, as the encoder does learn their size
	// before writing the frame header.
	streamedCompressedBody := AppendZstdBytes(nil, moderatelyCompressibleBody(4*1024*1024))
	if zstdFrameHasContentSize(streamedCompressedBody) {
		t.Fatalf("expecting no uncompressed size in a frame produced by the streaming encoder")
	}
	expectedSize = 2 * len(streamedCompressedBody)
	if size := estimateUnzstdSize(streamedCompressedBody); size != expectedSize {
		t.Fatalf("unexpected estimate %d for a streamed frame. Expecting %d", size, expectedSize)
	}

	if size := estimateUnzstdSize(nil); size != 0 {
		t.Fatalf("unexpected estimate %d for empty data. Expecting 0", size)
	}

	nonZstdData := []byte("this is not zstd at all")
	expectedSize = 2 * len(nonZstdData)
	if size := estimateUnzstdSize(nonZstdData); size != expectedSize {
		t.Fatalf("unexpected estimate %d for non-zstd data. Expecting %d", size, expectedSize)
	}

	// A forged huge uncompressed size must not be trusted as-is:
	// magic number, then a frame header descriptor with single segment set and
	// an 8 byte frame content size field, then the maximum content size.
	forgedHeader := []byte{
		0x28, 0xb5, 0x2f, 0xfd,
		0xe0,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
	expectedSize = 4_000_000
	if size := estimateUnzstdSize(forgedHeader); size != expectedSize {
		t.Fatalf("unexpected estimate %d for a forged header. Expecting %d", size, expectedSize)
	}
}

func zstdFrameHasContentSize(p []byte) bool {
	var header zstd.Header
	return header.Decode(p) == nil && header.HasFCS
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
