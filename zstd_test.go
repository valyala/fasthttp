package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
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

func TestZstdDecoderReuseInvalidBlock(t *testing.T) {
	t.Parallel()

	header := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0}
	// Fill each decoder block buffer with a previous caller's data.
	previous := slices.Clone(header)
	for range 3 {
		previous = append(previous, 0x1c, 0, 0, 8, 'x', 0) // compressed block with one literal
	}
	previous = append(previous, 0x1d, 0, 0, 8, 'x', 0)
	zr, err := acquireZstdReader(&byteSliceReader{b: previous})
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if n, err := zr.WriteTo(io.Discard); n != 4 || err != nil {
		t.Fatalf("decoding previous frame: wrote %d bytes, err %v", n, err)
	}

	// The first sequence refers to a match before the start of this frame.
	invalid := append(slices.Clone(header), 0x3d, 0, 0, 0, 1, 0x54, 0, 0, 0, 1)
	if err := zr.Reset(&byteSliceReader{b: invalid}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := zr.WriteTo(&buf); err == nil || buf.Len() != 0 {
		t.Fatalf("decoding invalid frame: wrote %q, err %v", buf.Bytes(), err)
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
		{name: "leading skippable frame", src: slices.Concat(skippablePrefix, withFCS), want: len(body)},
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

func TestWriteUnzstdWithLimitDecoderMemory(t *testing.T) {
	smallWindowFrame := zstdRawFrame(0x00, []byte{'x'})
	var dst bytes.Buffer
	n, err := writeUnzstd(&dst, smallWindowFrame, 1)
	if err != nil || n != 1 || dst.String() != "x" {
		t.Fatalf("unexpected small-window result: n=%d output=%q err=%v", n, dst.Bytes(), err)
	}

	eightMiBWindowFrame := zstdRawFrame(0x68, []byte{'x'})
	dst.Reset()
	n, err = writeUnzstd(&dst, eightMiBWindowFrame, 1)
	if err != nil || n != 1 || dst.String() != "x" {
		t.Fatalf("unexpected 8 MiB-window result: n=%d output=%q err=%v", n, dst.Bytes(), err)
	}

	sixteenMiBWindowFrame := zstdRawFrame(0x70, []byte{'x'})
	dst.Reset()
	n, err = writeUnzstd(&dst, sixteenMiBWindowFrame, 1)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("unexpected over-headroom window error: %v", err)
	}
	if n != 0 || dst.Len() != 0 {
		t.Fatalf("unexpected over-headroom window result: n=%d output=%q", n, dst.Bytes())
	}

	// Crossing an 8 MiB boundary must replace the pooled decoder instead of
	// reusing one whose cap is too small or too large for the request.
	dst.Reset()
	if _, err = writeUnzstd(&dst, sixteenMiBWindowFrame, 8<<20+1); err != nil {
		t.Fatalf("valid 16 MiB-window frame rejected above the bucket boundary: %v", err)
	}
	dst.Reset()
	if _, err = writeUnzstd(&dst, eightMiBWindowFrame, 1); err != nil {
		t.Fatalf("8 MiB-window frame rejected after returning to the smaller bucket: %v", err)
	}

	dst.Reset()
	if _, err = writeUnzstd(&dst, smallWindowFrame, 0); err != nil {
		t.Fatalf("unexpected unlimited decode error: %v", err)
	}
	dst.Reset()
	if _, err = writeUnzstd(&dst, smallWindowFrame, 1); err != nil {
		t.Fatalf("small-window frame rejected after changing limits: %v", err)
	}

	maliciousWindowFrame := zstdRawFrame(0x98, []byte("AA"))
	for _, testCase := range []struct {
		name   string
		decode func() error
	}{
		{name: "request unzstd", decode: func() error {
			var req Request
			req.SetBodyRaw(maliciousWindowFrame)
			_, err := req.BodyUnzstdWithLimit(1)
			return err
		}},
		{name: "request uncompressed", decode: func() error {
			var req Request
			req.Header.SetContentEncoding("zstd")
			req.SetBodyRaw(maliciousWindowFrame)
			_, err := req.BodyUncompressedWithLimit(1)
			return err
		}},
		{name: "response unzstd", decode: func() error {
			var resp Response
			resp.SetBodyRaw(maliciousWindowFrame)
			_, err := resp.BodyUnzstdWithLimit(1)
			return err
		}},
		{name: "response uncompressed", decode: func() error {
			var resp Response
			resp.Header.SetContentEncoding("zstd")
			resp.SetBodyRaw(maliciousWindowFrame)
			_, err := resp.BodyUncompressedWithLimit(1)
			return err
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.decode(); !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestWriteUnzstdWithLimitStreamingWindows(t *testing.T) {
	body := bytes.Repeat([]byte{'x'}, 1<<20)
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = encoder.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = encoder.Close(); err != nil {
		t.Fatal(err)
	}

	var dst bytes.Buffer
	n, err := writeUnzstd(&dst, compressed.Bytes(), len(body))
	if err != nil {
		t.Fatalf("default streaming frame rejected: %v", err)
	}
	if n != len(body) || !bytes.Equal(dst.Bytes(), body) {
		t.Fatalf("unexpected streaming result: n=%d output length=%d", n, dst.Len())
	}

	compressed.Reset()
	encoder, err = zstd.NewWriter(&compressed, zstd.WithWindowSize(16<<20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = encoder.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = encoder.Close(); err != nil {
		t.Fatal(err)
	}

	dst.Reset()
	if _, err = writeUnzstd(&dst, compressed.Bytes(), len(body)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("16 MiB streaming window accepted with an 8 MiB decoder cap: %v", err)
	}
	dst.Reset()
	n, err = writeUnzstd(&dst, compressed.Bytes(), 8<<20+1)
	if err != nil {
		t.Fatalf("16 MiB streaming window rejected with a 16 MiB decoder cap: %v", err)
	}
	if n != len(body) || !bytes.Equal(dst.Bytes(), body) {
		t.Fatalf("unexpected configured-window result: n=%d output length=%d", n, dst.Len())
	}
}

func TestWriteUnzstdWithLimitConcurrentCaps(t *testing.T) {
	frames := [][]byte{
		zstdRawFrame(0x00, []byte{'x'}),
		zstdRawFrame(0x68, []byte{'x'}),
		zstdRawFrame(0x70, []byte{'x'}),
	}
	limits := []int{1, 1 << 20, 8<<20 + 1}

	errCh := make(chan error, 32)
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Go(func() {
			for i := range 100 {
				index := (worker + i) % len(frames)
				var dst bytes.Buffer
				n, err := writeUnzstd(&dst, frames[index], limits[index])
				if err != nil || n != 1 || dst.String() != "x" {
					errCh <- fmt.Errorf("limit %d: n=%d output=%q err=%v", limits[index], n, dst.Bytes(), err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func zstdRawFrame(windowDescriptor byte, payload []byte) []byte {
	blockHeader := uint32(len(payload)<<3 | 1)
	return append([]byte{
		0x28, 0xb5, 0x2f, 0xfd,
		0x00,
		windowDescriptor,
		byte(blockHeader), byte(blockHeader >> 8), byte(blockHeader >> 16),
	}, payload...)
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
	_ = releaseStacklessZstdWriter(zw, CompressZstdDefault)

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

func TestZstdOutOfRangeLevels(t *testing.T) {
	t.Parallel()

	src := bytes.Repeat([]byte("foobar"), 100)
	for _, level := range []int{CompressZstdSpeedNotSet, -1, CompressZstdBestCompression + 1, CompressDefaultCompression} {
		compressed := AppendZstdBytesLevel(nil, src, level)
		decompressed, err := AppendUnzstdBytes(nil, compressed)
		if err != nil {
			t.Fatalf("level %d: unexpected error: %v", level, err)
		}
		if !bytes.Equal(decompressed, src) {
			t.Fatalf("level %d: unexpected decompressed body", level)
		}
	}
}

func TestWriteZstdLevelMultiBlock(t *testing.T) {
	// Hide bytes.Buffer so WriteZstdLevel uses the stackless writer.
	var dst bytes.Buffer
	w := struct{ io.Writer }{&dst}
	rng := rand.New(rand.NewPCG(1, 2))
	for level := CompressZstdBestSpeed; level <= CompressZstdBestCompression; level++ {
		t.Run(strconv.Itoa(level), func(t *testing.T) {
			for _, size := range []int{128<<10 - 1, 128 << 10, 128<<10 + 1, 1<<20 + 1} {
				body := make([]byte, size)
				for i := range body {
					body[i] = byte(rng.Uint32())
				}
				for iteration := range 3 {
					dst.Reset()
					body[0] = byte(iteration)
					n, err := WriteZstdLevel(w, body, level)
					if err != nil || n != len(body) {
						t.Fatalf("size %d: WriteZstdLevel = %d, %v", size, n, err)
					}
					decoded, err := AppendUnzstdBytes(nil, dst.Bytes())
					if err != nil {
						t.Fatalf("size %d: decode: %v", size, err)
					}
					if !bytes.Equal(decoded, body) {
						t.Fatalf("size %d: decoded stream differs from input", size)
					}
				}
			}
		})
	}
}
