package fasthttp

import (
	"bytes"
	"testing"
)

func TestCompressHandlerMediaTypeCase(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte("compressible response "), 1024)
	for _, contentType := range []string{
		"text/plain", "Text/Plain", "APPLICATION/JSON", "Image/SVG+XML",
		"IMAGE/X-ICON", "FONT/WOFF", "Multipart/Mixed; boundary=abc",
	} {
		t.Run(contentType, func(t *testing.T) {
			var ctx RequestCtx
			ctx.Request.Header.Set("Accept-Encoding", "gzip")
			handler := CompressHandler(func(ctx *RequestCtx) {
				ctx.SetContentType(contentType)
				ctx.SetBody(body)
			})
			handler(&ctx)
			if got := string(ctx.Response.Header.ContentEncoding()); got != "gzip" {
				t.Fatalf("Content-Encoding = %q; %s response was not compressed", got, contentType)
			}
			decoded, err := ctx.Response.BodyGunzip()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, body) {
				t.Fatal("decoded body changed")
			}
			if !bytes.Equal(ctx.Response.Header.ContentType(), []byte(contentType)) {
				t.Fatal("Content-Type changed")
			}
			if len(ctx.Response.Body()) >= len(body)/10 {
				t.Fatalf("compressed %d bytes into %d bytes", len(body), len(ctx.Response.Body()))
			}
		})
	}
}
