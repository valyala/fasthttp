package fasthttp

import (
	"fmt"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fasthttp/stackless"
	"io"
	"sync"
)

const (
	CompressZstdSpeedNotSet = iota
	CompressZstdBestSpeed
	CompressZstdDefault
	CompressZstdSpeedBetter
	CompressZstdBestCompression
)

var (
	zstdDecoderPool            sync.Pool
	stacklessZstdWriterPoolMap = newStacklessZstdWriterPoolMap()
)

func newStacklessZstdWriterPoolMap() []*sync.Pool {
	// Initialize pools for all the compression levels defined
	// in https://github.com/klauspost/compress/blob/v1.17.4/zstd/encoder_options.go#L146
	// Compression levels are normalized with normalizeCompressLevel,
	// so the fit [0..7].
	p := make([]*sync.Pool, 6)
	for i := range p {
		p[i] = &sync.Pool{}
	}
	return p
}

func acquireZstdReader(r io.Reader) (*zstd.Decoder, error) {
	v := zstdDecoderPool.Get()
	if v == nil {
		return zstd.NewReader(r)
	}
	zr := v.(*zstd.Decoder)
	if err := zr.Reset(r); err != nil {
		return nil, err
	}
	return zr, nil
}

func releaseZstdReader(zr *zstd.Decoder) {
	zstdDecoderPool.Put(zr)
}

var zstdEncoderPool sync.Pool

func acquireZstdWriter(w io.Writer, level int) (*zstd.Encoder, error) {
	v := zstdEncoderPool.Get()
	if v == nil {
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
	}
	zw := v.(*zstd.Encoder)
	zw.Reset(w)
	return zw, nil
}

func releaseZstdWriter(zw *zstd.Encoder) {
	zw.Close()
	zstdEncoderPool.Put(zw)
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
	sw := v.(stackless.Writer)
	sw.Reset(w)
	return sw

}

func releaseStacklessZstdWriter(zf stackless.Writer, zstdDefault int) {
	zf.Close()
	nLevel := normalizeZstdCompressLevel(zstdDefault)
	p := stacklessZstdWriterPoolMap[nLevel]
	p.Put(zf)
}

func acquireRealZstdWriter(w io.Writer, level int) stackless.Writer {
	nLevel := normalizeZstdCompressLevel(level)
	p := stacklessZstdWriterPoolMap[nLevel]
	v := p.Get()
	if v == nil {
		zw, err := acquireZstdWriter(w, level)
		if err != nil {
			panic(err)
		}
		return zw
	}
	zw := v.(*zstd.Encoder)
	zw.Reset(w)
	return zw
}

func AppendZstdBytesLevel(dst, src []byte, level int) []byte {
	w := &byteSliceWriter{dst}
	WriteZstdLevel(w, src, level) //nolint:errcheck
	return w.b
}

func WriteZstdLevel(w io.Writer, src []byte, level int) (int, error) {
	zw := acquireStacklessZstdWriter(w, level)
	n, err := zw.Write(src)
	releaseStacklessZstdWriter(zw, level)
	return n, err
}

// AppendZstdBytes appends zstd src to dst and returns the resulting dst.
func AppendZstdBytes(dst, src []byte) []byte {
	return AppendZstdBytesLevel(dst, src, CompressBrotliDefaultCompression)
}

// WriteUnzstd writes unzstd p to w and returns the number of uncompressed
// bytes written to w.
func WriteUnzstd(w io.Writer, p []byte) (int, error) {
	r := &byteSliceReader{p}
	zr, err := acquireZstdReader(r)
	if err != nil {
		return 0, err
	}
	n, err := copyZeroAlloc(w, zr)
	releaseZstdReader(zr)
	nn := int(n)
	if int64(nn) != n {
		return 0, fmt.Errorf("too much data unzstd: %d", n)
	}
	return nn, err
}

// AppendUnzstdBytes appends unzstd src to dst and returns the resulting dst.
func AppendUnzstdBytes(dst, src []byte) ([]byte, error) {
	w := &byteSliceWriter{dst}
	_, err := WriteUnzstd(w, src)
	return w.b, err
}

// normalizes compression level into [0..7], so it could be used as an index
// in *PoolMap.
func normalizeZstdCompressLevel(level int) int {
	// -2 is the lowest compression level - CompressHuffmanOnly
	// 9 is the highest compression level - CompressBestCompression
	if level < CompressZstdSpeedNotSet || level > CompressZstdBestCompression {
		level = CompressZstdDefault
	}
	return level
}
