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

func TestWriteUnzstdOptimizedDestinations(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("payload"), 1000)
	compressedBody := zstdEncodeWithFCS(t, body)
	prefix := []byte("prefix")

	testCases := []struct {
		name      string
		newWriter func() (io.Writer, func() []byte)
	}{
		{name: "byte slice writer", newWriter: func() (io.Writer, func() []byte) {
			w := &byteSliceWriter{b: append([]byte(nil), prefix...)}
			return w, func() []byte { return w.b }
		}},
		{name: "byte buffer pool", newWriter: func() (io.Writer, func() []byte) {
			w := &bytebufferpool.ByteBuffer{B: append([]byte(nil), prefix...)}
			return w, func() []byte { return w.B }
		}},
		{name: "bytes buffer", newWriter: func() (io.Writer, func() []byte) {
			w := bytes.NewBuffer(append([]byte(nil), prefix...))
			return w, w.Bytes
		}},
	}

	want := append(append([]byte(nil), prefix...), body...)
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			w, result := testCase.newWriter()
			n, err := writeUnzstd(w, compressedBody, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n != len(body) {
				t.Fatalf("unexpected decompressed size %d. Expecting %d", n, len(body))
			}
			if got := result(); !bytes.Equal(got, want) {
				t.Fatalf("unexpected destination %q. Expecting %q", got, want)
			}
		})
	}
}

func BenchmarkWriteUnzstdPresized(b *testing.B) {
	body := bytes.Repeat([]byte("payload"), 16*1024)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		b.Fatal(err)
	}
	compressedBody := encoder.EncodeAll(body, nil)
	encoder.Close()

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var dst bytebufferpool.ByteBuffer
		if _, err := WriteUnzstd(&dst, compressedBody); err != nil {
			b.Fatal(err)
		}
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
