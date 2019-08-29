package fasthttpadaptor

import (
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
)

func BenchmarkAdaptor(b *testing.B) {
	var req fasthttp.Request
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI("/foo/bar?baz=123")
	req.Header.SetHost("foobar.com")
	req.Header.Add(fasthttp.HeaderTransferEncoding, "encoding")
	req.BodyWriter().Write([]byte("body 123 foo bar baz"))
	for k, v := range map[string]string{
		"Foo-Bar":         "baz",
		"Abc":             "defg",
		"XXX-Remote-Addr": "123.43.4543.345",
	} {
		req.Header.Set(k, v)
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", "1.2.3.4:6789")
	if err != nil {
		b.Fatalf("unexpected error: %s", err)
	}

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Header1", "value1")
		w.Header().Set("Header2", "value2")
		w.WriteHeader(http.StatusBadRequest)

		buffer := bytebufferpool.Get()
		buffer.ReadFrom(r.Body)
		fmt.Fprintf(w, "request body is %q", buffer.B)
		bytebufferpool.Put(buffer)
	}
	fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH))

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var ctx fasthttp.RequestCtx
			ctx.Init(&req, remoteAddr, nil)
			fasthttpH(&ctx)
		}
	})
}
