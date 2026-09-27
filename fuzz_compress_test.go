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
		// A malformed stream or a limit error must not poison a pooled decoder.
		// Alternate the limits and exercise both public request/response paths.
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
		if (err == nil) != (againErr == nil) || errors.Is(err, ErrBodyTooLarge) != errors.Is(againErr, ErrBodyTooLarge) ||
			!bytes.Equal(got, again) {
			t.Fatalf("%s pooled request/response decoding changed result: %v vs %v", encoding, err, againErr)
		}
	})
}
