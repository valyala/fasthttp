package fasthttp

import (
	"bytes"
	"errors"
	"testing"
)

func fuzzCompress(codec uint8, data []byte) (string, []byte) {
	switch codec % 4 {
	case 0:
		return "gzip", AppendGzipBytesLevel(nil, data, CompressBestSpeed)
	case 1:
		return "deflate", AppendDeflateBytesLevel(nil, data, CompressBestSpeed)
	case 2:
		return "br", AppendBrotliBytesLevel(nil, data, CompressBrotliBestSpeed)
	default:
		return "zstd", AppendZstdBytesLevel(nil, data, CompressZstdBestSpeed)
	}
}

func FuzzBodyDecompression(f *testing.F) {
	for codec := range uint8(4) {
		_, data := fuzzCompress(codec, bytes.Repeat([]byte("compressible body"), 100))
		f.Add(data, codec, uint16(64))
		f.Add(data[:len(data)/2], codec, uint16(4096))
		f.Add([]byte{}, codec, uint16(1))
	}
	// A malformed deflate stream can hit the size limit before reporting corruption.
	malformedDeflate := append([]byte{0x78, 0x01, 0x62, 0x41, 0, 0, 0x37}, make([]byte, 3170)...)
	f.Add(append(malformedDeflate, 'A'), uint8(1), uint16(252))
	f.Fuzz(func(t *testing.T, data []byte, codec uint8, limit uint16) {
		if len(data) > 16*1024 {
			return
		}
		encoding, valid := fuzzCompress(codec, data)
		maxSize := int(limit) + 1 // Never invoke an unlimited decoder on fuzz input.
		var req Request
		var resp Response
		defer req.Reset()
		defer resp.Reset()
		req.Header.SetContentEncoding(encoding)
		resp.Header.SetContentEncoding(encoding)
		req.SetBody(data)
		got, err := req.BodyUncompressedWithLimit(maxSize)
		got = bytes.Clone(got)
		if err == nil && len(got) > maxSize {
			t.Fatalf("%s decoder exceeded limit: %d > %d", encoding, len(got), maxSize)
		}
		// Alternate the limits and exercise both public request/response paths.
		// Valid streams must still decode after a malformed or limited stream.
		for _, n := range []int{1, len(data) + 1, maxSize, len(data) + 1} {
			resp.SetBody(valid)
			body, decodeErr := resp.BodyUncompressedWithLimit(n)
			if len(data) > n {
				if !errors.Is(decodeErr, ErrBodyTooLarge) {
					t.Fatalf("%s: expected body limit error, got %v", encoding, decodeErr)
				}
			} else if decodeErr != nil || !bytes.Equal(body, data) {
				t.Fatalf("%s round trip after decoder reuse: err=%v", encoding, decodeErr)
			}
		}
		resp.SetBody(data)
		again, againErr := resp.BodyUncompressedWithLimit(maxSize)
		// For malformed input, the decoder may reach the size limit or a corrupt
		// section first. Both paths must reject it; successful results must match.
		if (err == nil) != (againErr == nil) || (err == nil && !bytes.Equal(got, again)) {
			t.Fatalf("%s pooled request/response decoding changed result: %v vs %v", encoding, err, againErr)
		}
	})
}
