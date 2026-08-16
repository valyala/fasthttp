package fasthttpadaptor

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"

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

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx.Request.Reset()
		ConvertNetHTTPRequestToFastHTTPRequest(&httpReq, ctx)
	}
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

	t.Run("URL host fallback when Host is empty", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			URL: &url.URL{
				Host: "url-host.com",
				Path: "/",
			},
			Proto:  "HTTP/1.1",
			Header: http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Host()) != "url-host.com" {
			t.Errorf("expected host url-host.com, got %s", ctx.Host())
		}
	})

	t.Run("Host field wins over Host header", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "canonical.example",
			Header: http.Header{
				"Host": []string{"other.example"},
			},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Host()) != "canonical.example" {
			t.Errorf("expected host canonical.example, got %s", ctx.Host())
		}
	})

	t.Run("scheme from URL", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method: "GET",
			URL: &url.URL{
				Scheme: "https",
				Host:   "example.com",
				Path:   "/secure",
			},
			Proto:  "HTTP/1.1",
			Header: http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.URI().Scheme()) != "https" {
			t.Errorf("expected scheme https, got %s", ctx.Request.URI().Scheme())
		}
		if string(ctx.Host()) != "example.com" {
			t.Errorf("expected host example.com, got %s", ctx.Host())
		}
	})

	t.Run("scheme from TLS connection state", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			TLS:        &tls.ConnectionState{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.URI().Scheme()) != "https" {
			t.Errorf("expected scheme https, got %s", ctx.Request.URI().Scheme())
		}
	})

	t.Run("HTTP/2 protocol is normalized to HTTP/1.1", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			Host:       "example.com",
			Header:     http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", ctx.Request.Header.Protocol())
		}
		if !ctx.Request.Header.IsHTTP11() {
			t.Error("expected IsHTTP11 to be true")
		}
	})

	t.Run("HTTP/2 protocol without minor version is normalized", func(t *testing.T) {
		t.Parallel()
		// ConvertRequest produces exactly this protocol and major version.
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/2",
			ProtoMajor: 2,
			Host:       "example.com",
			Header:     http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", ctx.Request.Header.Protocol())
		}
	})

	t.Run("HTTP/1.0 protocol is kept", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.0",
			ProtoMajor: 1,
			Host:       "example.com",
			Header:     http.Header{},
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if string(ctx.Request.Header.Protocol()) != "HTTP/1.0" {
			t.Errorf("expected protocol HTTP/1.0, got %s", ctx.Request.Header.Protocol())
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

		values := ctx.Request.Header.PeekAll("Accept")
		if len(values) != 3 {
			t.Errorf("expected 3 Accept header values, got %d", len(values))
		}
	})

	t.Run("close overrides copied connection header", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"Connection": []string{"keep-alive"},
			},
			Close: true,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if !ctx.Request.Header.ConnectionClose() {
			t.Error("expected connection close to be set")
		}

		var connValues []string
		for k, v := range ctx.Request.Header.All() {
			if strings.EqualFold(string(k), fasthttp.HeaderConnection) {
				connValues = append(connValues, string(v))
			}
		}
		if len(connValues) != 1 || connValues[0] != "close" {
			t.Errorf("expected a single Connection: close header, got %v", connValues)
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

		if ctx.Request.Header.ContentLength() != len(bodyContent) {
			t.Errorf("expected content length %d, got %d", len(bodyContent), ctx.Request.Header.ContentLength())
		}
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

	t.Run("NoBody keeps zero content length", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:        "GET",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          http.NoBody,
			ContentLength: 0,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if ctx.Request.Header.ContentLength() != 0 {
			t.Errorf("expected content length 0, got %d", ctx.Request.Header.ContentLength())
		}
		if len(ctx.Request.Body()) != 0 {
			t.Errorf("expected empty body, got %q", ctx.Request.Body())
		}
	})

	t.Run("zero content length with body means unknown", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:        "POST",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          io.NopCloser(strings.NewReader("data")),
			ContentLength: 0,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if ctx.Request.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", ctx.Request.Header.ContentLength())
		}
		if string(ctx.Request.Body()) != "data" {
			t.Errorf("expected body data, got %q", ctx.Request.Body())
		}
	})

	t.Run("negative content length means unknown", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:        "POST",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          io.NopCloser(strings.NewReader("streamed")),
			ContentLength: -1,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if ctx.Request.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", ctx.Request.Header.ContentLength())
		}
		if string(ctx.Request.Header.Peek(fasthttp.HeaderTransferEncoding)) != "chunked" {
			t.Errorf("expected chunked transfer encoding, got %q", ctx.Request.Header.Peek(fasthttp.HeaderTransferEncoding))
		}
	})

	t.Run("content length larger than max int means unknown", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:        "POST",
			RequestURI:    "/",
			Proto:         "HTTP/1.1",
			Host:          "example.com",
			Header:        http.Header{},
			Body:          io.NopCloser(strings.NewReader("big")),
			ContentLength: math.MaxInt64,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if ctx.Request.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", ctx.Request.Header.ContentLength())
		}
	})

	t.Run("connection close", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Close:      true,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		if !ctx.Request.Header.ConnectionClose() {
			t.Error("expected connection close to be set")
		}
	})

	t.Run("trailers", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Checksum":     []string{"abc123"},
				"Content-Length": []string{"42"}, // forbidden trailer, must be skipped
			},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: -1,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		trailer := string(ctx.Request.Header.TrailerHeader())
		if !strings.Contains(trailer, "X-Checksum: abc123") {
			t.Errorf("expected trailer X-Checksum: abc123, got %q", trailer)
		}
		if strings.Contains(trailer, "Content-Length") {
			t.Errorf("expected forbidden trailer Content-Length to be skipped, got %q", trailer)
		}
	})

	t.Run("multiple trailer values are joined", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Tag": []string{"a", "b"},
			},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: -1,
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		trailer := string(ctx.Request.Header.TrailerHeader())
		if !strings.Contains(trailer, "X-Tag: a, b") {
			t.Errorf("expected trailer X-Tag: a, b, got %q", trailer)
		}
	})

	t.Run("write round trip with chunked body and trailer", func(t *testing.T) {
		t.Parallel()
		bodyContent := "chunked body content"
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/upload",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Checksum": []string{"abc123"},
			},
			ContentLength: -1,
			Body:          io.NopCloser(strings.NewReader(bodyContent)),
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := ctx.Request.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}

		var parsed fasthttp.Request
		if err := parsed.Read(bufio.NewReader(bytes.NewReader(buf.Bytes()))); err != nil {
			t.Fatalf("unexpected error reading request back: %v", err)
		}

		if string(parsed.Body()) != bodyContent {
			t.Errorf("expected body %q, got %q", bodyContent, parsed.Body())
		}
		if string(parsed.Header.Peek("X-Checksum")) != "abc123" {
			t.Errorf("expected trailer value abc123, got %q", parsed.Header.Peek("X-Checksum"))
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

	t.Run("invalid remote address is ignored", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			RemoteAddr: "not-an-address",
		}

		ctx := &fasthttp.RequestCtx{}
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, ctx)

		remoteAddr := ctx.RemoteAddr().String()
		if remoteAddr != "0.0.0.0:0" {
			t.Errorf("expected default remote addr 0.0.0.0:0, got %s", remoteAddr)
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
			Body:          io.NopCloser(iotest.ErrReader(errors.New("read error"))),
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

func TestParseRemoteAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr     string
		expected string
	}{
		{"192.168.1.100:8080", "192.168.1.100:8080"},
		{"192.168.1.100", "192.168.1.100:0"},
		{"[2001:db8::1]:8080", "[2001:db8::1]:8080"},
		{"2001:db8::1", "[2001:db8::1]:0"},
		{"[fe80::1%eth0]:9090", "[fe80::1%eth0]:9090"},
		{"fe80::1%eth0", "[fe80::1%eth0]:0"},
		{"[::1]:3000", "[::1]:3000"},
		{"not-an-address", ""},
		{"example.com:8080", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			addr := parseRemoteAddr(tt.addr)
			if tt.expected == "" {
				if addr != nil {
					t.Errorf("expected nil for %q, got %s", tt.addr, addr)
				}
				return
			}
			if addr == nil {
				t.Fatalf("expected %s for %q, got nil", tt.expected, tt.addr)
			}
			if addr.String() != tt.expected {
				t.Errorf("expected %s for %q, got %s", tt.expected, tt.addr, addr)
			}
		})
	}
}
