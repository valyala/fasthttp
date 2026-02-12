package fasthttpadaptor

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/valyala/fasthttp"
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
	ctx.Request.Header.Set("y", "test")
	ctx.Request.SetRequestURI("/test")
	ctx.Request.SetHost("test")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = ConvertRequest(ctx, &httpReq, true)
	}
}

func TestParseRequestURICompatibility(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		value string
	}{
		{name: "root", value: "/"},
		{name: "fast-path", value: "/new?a=1"},
		{name: "fast-path-no-query", value: "/new"},
		{name: "force-query", value: "/test?"},
		{name: "escaped-query", value: "/search?q=%2Ftmp"},
		{name: "raw-path", value: "/a%2Fb?x=1"},
		{name: "raw-path-force-query", value: "/a%2Fb?"},
		{name: "double-slash", value: "//foo/bar?x=1"},
		{name: "asterisk", value: "*"},
		{name: "absolute-uri", value: "http://example.com/a%2Fb?x=1"},
		{name: "absolute-uri-userinfo-port", value: "http://u:p@example.com:8080/a/b?x=1"},
		{name: "invalid", value: "://bad"},
		{name: "invalid-escape", value: "/a%2"},
		{name: "empty", value: ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := &http.Request{
				URL: &url.URL{
					Path:       "/old",
					RawPath:    "/old%2Fraw",
					RawQuery:   "stale=1",
					ForceQuery: true,
				},
			}

			gotErr := parseRequestURI(r, tc.value)
			wantURL, wantErr := url.ParseRequestURI(tc.value)

			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("error mismatch for %q: parseRequestURI err=%v, url.ParseRequestURI err=%v", tc.value, gotErr, wantErr)
			}
			if gotErr != nil {
				return
			}

			if r.URL.Path != wantURL.Path {
				t.Fatalf("path mismatch for %q: got=%q want=%q", tc.value, r.URL.Path, wantURL.Path)
			}
			if r.URL.RawPath != wantURL.RawPath {
				t.Fatalf("raw path mismatch for %q: got=%q want=%q", tc.value, r.URL.RawPath, wantURL.RawPath)
			}
			if r.URL.RawQuery != wantURL.RawQuery {
				t.Fatalf("raw query mismatch for %q: got=%q want=%q", tc.value, r.URL.RawQuery, wantURL.RawQuery)
			}
			if r.URL.ForceQuery != wantURL.ForceQuery {
				t.Fatalf("force query mismatch for %q: got=%v want=%v", tc.value, r.URL.ForceQuery, wantURL.ForceQuery)
			}
		})
	}
}
