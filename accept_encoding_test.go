package fasthttp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestRequestHeaderHasAcceptEncodingTokens(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, header, encoding string
		want                   bool
	}{
		{"first", "gzip,br", "gzip", true},
		{"no_space", "br,gzip", "gzip", true},
		{"ows", "br,\t gzip \t,deflate", "gzip", true},
		{"mixed_case", "br,GZip", "gzip", true},
		{"mixed_case_query", "gzip,br", "GZIP", true},
		{"later_token", "xgzip, gzip", "gzip", true},
		{"later_token_after_prefix", "gzip-extra, gzip", "gzip", true},
		{"suffix_only", "xgzip,br", "gzip", false},
		{"prefix_only", "gzip-extra,br", "gzip", false},
		{"internal_space", "g zip,br", "gzip", false},
		{"absent", "br,deflate", "gzip", false},
		{"empty_header", "", "gzip", false},
		{"quality_unchanged", "gzip;q=0", "gzip", false},
		{"wildcard_unchanged", "*", "gzip", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var h RequestHeader
			h.Set(HeaderAcceptEncoding, tc.header)
			if got := h.HasAcceptEncoding(tc.encoding); got != tc.want {
				t.Fatalf("HasAcceptEncoding(%q, %q) = %v, want %v", tc.header, tc.encoding, got, tc.want)
			}
			if got := h.HasAcceptEncodingBytes([]byte(tc.encoding)); got != tc.want {
				t.Fatalf("HasAcceptEncodingBytes(%q, %q) = %v, want %v", tc.header, tc.encoding, got, tc.want)
			}
		})
	}
}

func TestCompressHandlerAcceptEncodingTokens(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("content coding token test ", 100)
	handler := CompressHandler(func(ctx *RequestCtx) {
		ctx.SetBodyString(body)
	})
	for _, acceptEncoding := range []string{"br,gzip", "br,\t GZip \t", "xgzip, gzip"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			t.Parallel()

			var ctx RequestCtx
			wire := "GET / HTTP/1.1\r\nHost: example.com\r\nAccept-Encoding: " + acceptEncoding + "\r\n\r\n"
			if err := ctx.Request.Read(bufio.NewReader(strings.NewReader(wire))); err != nil {
				t.Fatal(err)
			}
			handler(&ctx)
			var response Response
			if err := response.Read(bufio.NewReader(bytes.NewBufferString(ctx.Response.String()))); err != nil {
				t.Fatal(err)
			}
			if got := string(response.Header.ContentEncoding()); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			decoded, err := response.BodyGunzip()
			if err != nil {
				t.Fatal(err)
			}
			if string(decoded) != body {
				t.Fatal("decompressed response differs from the original body")
			}
		})
	}
}

func BenchmarkRequestHeaderHasAcceptEncodingTokens(b *testing.B) {
	for _, tc := range []struct {
		name, header, encoding string
	}{
		{"first", "gzip, deflate, br", "gzip"},
		{"last", "gzip, deflate, br", "br"},
		{"absent", "gzip, deflate, br", "zstd"},
		{"no_space", "br,gzip", "gzip"},
		{"later_token", "xgzip, gzip", "gzip"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var h RequestHeader
			h.Set(HeaderAcceptEncoding, tc.header)
			encoding := []byte(tc.encoding)
			b.ReportAllocs()
			for b.Loop() {
				h.HasAcceptEncodingBytes(encoding)
			}
		})
	}
}
