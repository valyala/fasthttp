package fasthttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp/stackless"
)

const (
	CompressZstdSpeedNotSet = iota
	CompressZstdBestSpeed
	CompressZstdDefault
	CompressZstdSpeedBetter
	CompressZstdBestCompression
)

const (
	limitedZstdDecoderMemoryHeadroom uint64 = 8 * 1024 * 1024
	maxZstdDecoderMemory             uint64 = 1 << 63
)

var (
	zstdDecoderPool            sync.Pool
	zstdLimitedDecoderPool     sync.Pool
	realZstdWriterPoolMap      = newCompressWriterPoolMap()
	stacklessZstdWriterPoolMap = newCompressWriterPoolMap()
)

type limitedZstdReader struct {
	zr        *zstd.Decoder
	maxMemory uint64
}

func acquireZstdReader(r io.Reader) (*zstd.Decoder, error) {
	v := zstdDecoderPool.Get()
	if v == nil {
		return zstd.NewReader(r)
	}
	zr := v.(*zstd.Decoder) //nolint:forcetypeassert
	if err := zr.Reset(r); err != nil {
		return nil, err
	}
	return zr, nil
}

func releaseZstdReader(zr *zstd.Decoder) {
	zstdDecoderPool.Put(zr)
}

func acquireLimitedZstdReader(r io.Reader, maxBodySize int) (*limitedZstdReader, error) {
	requiredMemory := uint64(max(maxBodySize, zstd.MinWindowSize))
	if v := zstdLimitedDecoderPool.Get(); v != nil {
		limited := v.(*limitedZstdReader) //nolint:forcetypeassert
		// Decoder caps are rounded up to 8 MiB buckets below. These bounds
		// accept exactly the bucket a fresh decoder would use for this limit.
		if limited.maxMemory >= requiredMemory &&
			limited.maxMemory-requiredMemory < limitedZstdDecoderMemoryHeadroom {
			if err := limited.zr.Reset(r); err != nil {
				limited.zr.Close()
				return nil, err
			}
			return limited, nil
		}
		limited.zr.Close()
	}

	maxMemory := requiredMemory
	if remainder := requiredMemory % limitedZstdDecoderMemoryHeadroom; remainder != 0 {
		maxMemory = min(
			requiredMemory+limitedZstdDecoderMemoryHeadroom-remainder,
			maxZstdDecoderMemory,
		)
	}
	zr, err := zstd.NewReader(
		r,
		zstd.WithDecoderMaxMemory(maxMemory),
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		return nil, err
	}
	return &limitedZstdReader{zr: zr, maxMemory: maxMemory}, nil
}

func releaseLimitedZstdReader(limited *limitedZstdReader) {
	if err := limited.zr.Reset(nil); err != nil {
		limited.zr.Close()
		return
	}
	zstdLimitedDecoderPool.Put(limited)
}

func acquireStacklessZstdWriter(w io.Writer, compressLevel int) stackless.Writer {
	nLevel := normalizeZstdCompressLevel(compressLevel)
	p := stacklessZstdWriterPoolMap[nLevel]
	v := p.Get()
	if v == nil {
		return stackless.NewWriter(w, func(w io.Writer) stackless.Writer {
			return acquireRealZstdWriter(w, compressLevel)
		})
	}
	sw := v.(stackless.Writer) //nolint:forcetypeassert
	sw.Reset(w)
	return sw
}

func releaseStacklessZstdWriter(zf stackless.Writer, level int) {
	zf.Close()
	nLevel := normalizeZstdCompressLevel(level)
	p := stacklessZstdWriterPoolMap[nLevel]
	p.Put(zf)
}

func acquireRealZstdWriter(w io.Writer, level int) *zstd.Encoder {
	nLevel := normalizeZstdCompressLevel(level)
	p := realZstdWriterPoolMap[nLevel]
	v := p.Get()
	if v == nil {
		zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevel(nLevel)))
		if err != nil {
			panic(err)
		}
		return zw
	}
	zw := v.(*zstd.Encoder) //nolint:forcetypeassert
	zw.Reset(w)
	return zw
}

func releaseRealZstdWriter(zw *zstd.Encoder, level int) {
	zw.Close()
	nLevel := normalizeZstdCompressLevel(level)
	p := realZstdWriterPoolMap[nLevel]
	p.Put(zw)
}

func AppendZstdBytesLevel(dst, src []byte, level int) []byte {
	w := &byteSliceWriter{b: dst}
	WriteZstdLevel(w, src, level) //nolint:errcheck
	return w.b
}

func WriteZstdLevel(w io.Writer, p []byte, level int) (int, error) {
	level = normalizeZstdCompressLevel(level)
	switch w.(type) {
	case *byteSliceWriter,
		*bytes.Buffer,
		*bytebufferpool.ByteBuffer:
		ctx := &compressCtx{
			w:     w,
			p:     p,
			level: level,
		}
		stacklessWriteZstd(ctx)
		return len(p), nil
	default:
		zw := acquireStacklessZstdWriter(w, level)
		n, err := zw.Write(p)
		releaseStacklessZstdWriter(zw, level)
		return n, err
	}
}

var (
	stacklessWriteZstdOnce sync.Once
	stacklessWriteZstdFunc func(ctx any) bool
)

func stacklessWriteZstd(ctx any) {
	stacklessWriteZstdOnce.Do(func() {
		stacklessWriteZstdFunc = stackless.NewFunc(nonblockingWriteZstd)
	})
	stacklessWriteZstdFunc(ctx)
}

func nonblockingWriteZstd(ctxv any) {
	ctx := ctxv.(*compressCtx) //nolint:forcetypeassert
	zw := acquireRealZstdWriter(ctx.w, ctx.level)
	zw.Write(ctx.p) //nolint:errcheck
	releaseRealZstdWriter(zw, ctx.level)
}

// AppendZstdBytes appends zstd src to dst and returns the resulting dst.
func AppendZstdBytes(dst, src []byte) []byte {
	return AppendZstdBytesLevel(dst, src, CompressZstdDefault)
}

// WriteUnzstd writes unzstd p to w and returns the number of uncompressed
// bytes written to w.
func WriteUnzstd(w io.Writer, p []byte) (int, error) {
	return writeUnzstd(w, p, 0)
}

func writeUnzstd(w io.Writer, p []byte, maxBodySize int) (int, error) {
	estimatedDecompressedSize := estimateUnzstdSize(p)
	if maxBodySize > 0 {
		estimatedDecompressedSize = min(estimatedDecompressedSize, maxBodySize)
	}

	switch dst := w.(type) {
	case *byteSliceWriter:
		dst.b = slices.Grow(dst.b, estimatedDecompressedSize)
	case *bytebufferpool.ByteBuffer:
		dst.B = slices.Grow(dst.B, estimatedDecompressedSize)
	case *bytes.Buffer:
		dst.Grow(estimatedDecompressedSize)
	}

	r := &byteSliceReader{b: p}
	var zr *zstd.Decoder
	var limited *limitedZstdReader
	var err error
	if maxBodySize > 0 {
		limited, err = acquireLimitedZstdReader(r, maxBodySize)
		if limited != nil {
			zr = limited.zr
		}
	} else {
		zr, err = acquireZstdReader(r)
	}
	if err != nil {
		return 0, err
	}
	n, err := copyZeroAllocWithLimit(w, zr, maxBodySize)
	if maxBodySize > 0 {
		releaseLimitedZstdReader(limited)
		if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
			err = ErrBodyTooLarge
		}
	} else {
		releaseZstdReader(zr)
	}
	nn := int(n)
	if int64(nn) != n {
		return 0, fmt.Errorf("too much data unzstd: %d", n)
	}
	return nn, err
}

func estimateUnzstdSize(p []byte) int {
	// Somewhat reasonable and conservative expectation of compression factor of 2
	sizeHint := 2 * len(p)

	// We look for the first non-skippable header
	var header zstd.Header
	for {
		if err := header.Decode(p); err != nil {
			break
		}
		if !header.Skippable {
			break
		}
		skippedBytes := header.HeaderSize + int(header.SkippableSize)
		if skippedBytes <= 0 || skippedBytes > len(p) {
			break
		}
		p = p[skippedBytes:]
	}

	if header.HasFCS {
		// Let's have some limit just in case the input is malicious
		// and wants us to allocate bazillion bytes.
		// In a non-malicious case it's still better to start growing from 4 MB than from 0.

		// gosec complains about integer overflow but the uint64 argument to int() is not larger than 4_000_000, so we silence it.
		sizeHint = int(min(header.FrameContentSize, 4_000_000)) // #nosec G115
	}
	return sizeHint
}

// AppendUnzstdBytes appends unzstd src to dst and returns the resulting dst.
func AppendUnzstdBytes(dst, src []byte) ([]byte, error) {
	w := &byteSliceWriter{b: dst}
	_, err := WriteUnzstd(w, src)
	return w.b, err
}

// normalizes compression level into [0..7], so it could be used as an index
// in *PoolMap.
func normalizeZstdCompressLevel(level int) int {
	if level < CompressZstdSpeedNotSet || level > CompressZstdBestCompression {
		level = CompressZstdDefault
	}
	return level
}
