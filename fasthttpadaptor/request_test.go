package fasthttpadaptor

import (
	"net/http"
	"testing"

	"github.com/valyala/fasthttp"
)

func TestConvertRequestPreservesDuplicateHeaders(t *testing.T) {
	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod("GET")
	req.SetRequestURI("/")
	req.Header.SetHost("example.com")
	req.Header.Add("X-Forwarded-For", "10.0.0.1")
	req.Header.Add("X-Forwarded-For", "203.0.113.7")
	ctx.Init(&req, nil, nil)

	var r http.Request
	if err := ConvertRequest(&ctx, &r, true); err != nil {
		t.Fatalf("ConvertRequest returned error: %v", err)
	}

	got := r.Header.Values("X-Forwarded-For")
	want := []string{"10.0.0.1", "203.0.113.7"}
	if len(got) != len(want) {
		t.Fatalf("X-Forwarded-For = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("X-Forwarded-For = %q, want %q", got, want)
		}
	}
}

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
	ctx.Request.Header.Set("y", "test")
	ctx.Request.SetRequestURI("/test")
	ctx.Request.SetHost("test")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = ConvertRequest(ctx, &httpReq, true)
	}
}

func TestConvertRequestProtocolVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		protocol, wantProto  string
		wantMajor, wantMinor int
	}{
		{"HTTP/2", "HTTP/2.0", 2, 0},
		{"HTTP/1.0", "HTTP/1.0", 1, 0},
		{"HTTP/1.1", "HTTP/1.1", 1, 1},
	} {
		ctx := &fasthttp.RequestCtx{}
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		ctx.Request.SetRequestURI("/")
		ctx.Request.Header.SetHost("example.com")
		ctx.Request.Header.SetProtocol(tc.protocol)

		var r http.Request
		if err := ConvertRequest(ctx, &r, true); err != nil {
			t.Fatalf("ConvertRequest(%s) error: %v", tc.protocol, err)
		}
		if r.Proto != tc.wantProto || r.ProtoMajor != tc.wantMajor || r.ProtoMinor != tc.wantMinor {
			t.Errorf("%s -> %q %d.%d, want %q %d.%d",
				tc.protocol, r.Proto, r.ProtoMajor, r.ProtoMinor, tc.wantProto, tc.wantMajor, tc.wantMinor)
		}
	}
}
