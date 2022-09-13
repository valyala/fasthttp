package fasthttpadaptor

import (
	"github.com/valyala/fasthttp"
	"net/http"
	"testing"
)

func BenchmarkConvertRequest(b *testing.B) {
	var httpReq http.Request

	ctx := &fasthttp.RequestCtx{
		Request: fasthttp.Request{
			Header:        fasthttp.RequestHeader{},
			UseHostHeader: false,
		},
	}
	ctx.Request.Header.SetMethod("GET")
	ctx.Request.Header.Set("x", "test")
	ctx.Request.SetRequestURI("/test")
	ctx.Request.SetHost("test")

	for i := 0; i < b.N; i++ {
		ConvertRequest(ctx, &httpReq, true)
	}
}
