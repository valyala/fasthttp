package fasthttpadaptor

import (
	"bytes"
	"errors"
	"io"
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

func BenchmarkConvertNetHTTPRequestToFastHTTPRequest(b *testing.B) {
	httpReq := http.Request{
		Method:     "GET",
		RequestURI: "/test",
		Host:       "test",
		Header: http.Header{
			"X": []string{"test"},
			"Y": []string{"test"},
		},
	}

	ctx := &fasthttp.RequestCtx{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConvertNetHTTPRequestToFastHTTPRequest(&httpReq, ctx)
	}
}

// errReader is a reader that always returns an error.
type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read error")
}

func TestConvertNetHTTPRequestToFastHTTPRequest(t *testing.T) {
	t.Parallel()

	t.Run("basic conversion", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/test/path?query=1",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Method()) != "POST" {
			t.Errorf("expected method POST, got %s", ctx.Method())
		}
		if string(ctx.RequestURI()) != "/test/path?query=1" {
			t.Errorf("expected URI /test/path?query=1, got %s", ctx.RequestURI())
		}
		if string(ctx.Request.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", ctx.Request.Header.Protocol())
		}
		if string(ctx.Host()) != "example.com" {
			t.Errorf("expected host example.com, got %s", ctx.Host())
		}
	})

	t.Run("URL fallback when RequestURI is empty", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "",
			URL: &url.URL{
				Path:     "/fallback/path",
				RawQuery: "foo=bar",
			},
			Proto:  "HTTP/1.1",
			Host:   "fallback.com",
			Header: http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.RequestURI()) != "/fallback/path?foo=bar" {
			t.Errorf("expected URI /fallback/path?foo=bar, got %s", ctx.RequestURI())
		}
	})

	t.Run("single header", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"X-Custom-Header": []string{"custom-value"},
			},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.Header.Peek("X-Custom-Header")) != "custom-value" {
			t.Errorf("expected header value custom-value, got %s", ctx.Request.Header.Peek("X-Custom-Header"))
		}
	})

	t.Run("multiple header values", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"Accept": []string{"text/html", "application/json", "text/plain"},
			},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		// Check all header values are present
		var values []string
		ctx.Request.Header.All()(func(key, value []byte) bool {
			if string(key) == "Accept" {
				values = append(values, string(value))
			}
			return true
		})

		if len(values) != 3 {
			t.Errorf("expected 3 Accept header values, got %d", len(values))
		}
	})

	t.Run("request body", func(t *testing.T) {
		t.Parallel()
		bodyContent := []byte("test body content")
		httpReq := &http.Request{
			Method:        "POST",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          io.NopCloser(bytes.NewReader(bodyContent)),
			ContentLength: int64(len(bodyContent)),
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if !bytes.Equal(ctx.Request.Body(), bodyContent) {
			t.Errorf("expected body %q, got %q", bodyContent, ctx.Request.Body())
		}
	})

	t.Run("nil body", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Body:       nil,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if len(ctx.Request.Body()) != 0 {
			t.Errorf("expected empty body, got %q", ctx.Request.Body())
		}
	})

	t.Run("remote address with port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "192.168.1.100:8080",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "192.168.1.100:8080" {
			t.Errorf("expected remote addr 192.168.1.100:8080, got %s", remoteAddr)
		}
	})

	t.Run("remote address without port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "192.168.1.100",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "192.168.1.100:0" {
			t.Errorf("expected remote addr 192.168.1.100:0, got %s", remoteAddr)
		}
	})

	t.Run("IPv6 remote address with port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "[2001:db8::1]:8080",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "[2001:db8::1]:8080" {
			t.Errorf("expected remote addr [2001:db8::1]:8080, got %s", remoteAddr)
		}
	})

	t.Run("IPv6 remote address without port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "2001:db8::1",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "[2001:db8::1]:0" {
			t.Errorf("expected remote addr [2001:db8::1]:0, got %s", remoteAddr)
		}
	})

	t.Run("IPv6 remote address with zone and port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "[fe80::1%eth0]:9090",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "[fe80::1%eth0]:9090" {
			t.Errorf("expected remote addr [fe80::1%%eth0]:9090, got %s", remoteAddr)
		}
	})

	t.Run("IPv6 remote address with zone without port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "fe80::1%eth0",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "[fe80::1%eth0]:0" {
			t.Errorf("expected remote addr [fe80::1%%eth0]:0, got %s", remoteAddr)
		}
	})

	t.Run("IPv6 loopback with port", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "[::1]:3000",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "[::1]:3000" {
			t.Errorf("expected remote addr [::1]:3000, got %s", remoteAddr)
		}
	})

	t.Run("body read error", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:        "POST",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          io.NopCloser(errReader{}),
			ContentLength: 10,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		_, err := io.ReadAll(ctx.RequestBodyStream())
		if err == nil {
			t.Fatal("expected error when reading body stream, got nil")
		}
	})
}
