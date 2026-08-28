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

	var req fasthttp.Request

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Reset()
		ConvertNetHTTPRequestToFastHTTPRequest(&httpReq, &req)
	}
}

type closeTrackingReader struct {
	io.Reader

	closed *bool
}

func (r *closeTrackingReader) Close() error {
	*r.closed = true
	return nil
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.Method()) != "POST" {
			t.Errorf("expected method POST, got %s", req.Header.Method())
		}
		if string(req.Header.RequestURI()) != "/test/path?query=1" {
			t.Errorf("expected URI /test/path?query=1, got %s", req.Header.RequestURI())
		}
		if string(req.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", req.Header.Protocol())
		}
		if string(req.Host()) != "example.com" {
			t.Errorf("expected host example.com, got %s", req.Host())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.RequestURI()) != "/fallback/path?foo=bar" {
			t.Errorf("expected URI /fallback/path?foo=bar, got %s", req.Header.RequestURI())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Host()) != "url-host.com" {
			t.Errorf("expected host url-host.com, got %s", req.Host())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Host()) != "canonical.example" {
			t.Errorf("expected host canonical.example, got %s", req.Host())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.URI().Scheme()) != "https" {
			t.Errorf("expected scheme https, got %s", req.URI().Scheme())
		}
		if string(req.Host()) != "example.com" {
			t.Errorf("expected host example.com, got %s", req.Host())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.URI().Scheme()) != "https" {
			t.Errorf("expected scheme https, got %s", req.URI().Scheme())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", req.Header.Protocol())
		}
		if !req.Header.IsHTTP11() {
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.Protocol()) != "HTTP/1.1" {
			t.Errorf("expected protocol HTTP/1.1, got %s", req.Header.Protocol())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.Protocol()) != "HTTP/1.0" {
			t.Errorf("expected protocol HTTP/1.0, got %s", req.Header.Protocol())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Header.Peek("X-Custom-Header")) != "custom-value" {
			t.Errorf("expected header value custom-value, got %s", req.Header.Peek("X-Custom-Header"))
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		values := req.Header.PeekAll("Accept")
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if !req.Header.ConnectionClose() {
			t.Error("expected connection close to be set")
		}

		var connValues []string
		for k, v := range req.Header.All() {
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != len(bodyContent) {
			t.Errorf("expected content length %d, got %d", len(bodyContent), req.Header.ContentLength())
		}
		if !bytes.Equal(req.Body(), bodyContent) {
			t.Errorf("expected body %q, got %q", bodyContent, req.Body())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if len(req.Body()) != 0 {
			t.Errorf("expected empty body, got %q", req.Body())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != 0 {
			t.Errorf("expected content length 0, got %d", req.Header.ContentLength())
		}
		if len(req.Body()) != 0 {
			t.Errorf("expected empty body, got %q", req.Body())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", req.Header.ContentLength())
		}
		if string(req.Body()) != "data" {
			t.Errorf("expected body data, got %q", req.Body())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", req.Header.ContentLength())
		}
		if string(req.Header.Peek(fasthttp.HeaderTransferEncoding)) != "chunked" {
			t.Errorf("expected chunked transfer encoding, got %q", req.Header.Peek(fasthttp.HeaderTransferEncoding))
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", req.Header.ContentLength())
		}
	})

	t.Run("Content-Length header entry is ignored", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"Content-Length": []string{"42"},
			},
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != 0 {
			t.Errorf("expected content length 0, got %d", req.Header.ContentLength())
		}
		if len(req.Header.Peek(fasthttp.HeaderContentLength)) != 0 {
			t.Errorf("expected no Content-Length header, got %q", req.Header.Peek(fasthttp.HeaderContentLength))
		}

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}
		if strings.Contains(buf.String(), "Content-Length") {
			t.Errorf("expected no Content-Length on the wire, got:\n%s", buf.String())
		}
	})

	t.Run("chunked TransferEncoding overrides ContentLength", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:           "POST",
			RequestURI:       "/",
			Proto:            "HTTP/1.1",
			Host:             "example.com",
			Header:           http.Header{},
			Body:             io.NopCloser(strings.NewReader("data")),
			ContentLength:    4,
			TransferEncoding: []string{"chunked"},
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", req.Header.ContentLength())
		}

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}
		if !strings.Contains(buf.String(), "Transfer-Encoding: chunked") {
			t.Errorf("expected chunked framing on the wire, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "Content-Length") {
			t.Errorf("expected no Content-Length on the wire, got:\n%s", buf.String())
		}

		var parsed fasthttp.Request
		if err := parsed.Read(bufio.NewReader(bytes.NewReader(buf.Bytes()))); err != nil {
			t.Fatalf("unexpected error reading request back: %v", err)
		}
		if string(parsed.Body()) != "data" {
			t.Errorf("expected body data, got %q", parsed.Body())
		}
	})

	t.Run("Transfer-Encoding header entry is ignored", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"Transfer-Encoding": []string{"chunked"},
			},
			Body:          io.NopCloser(strings.NewReader("data")),
			ContentLength: 4,
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != 4 {
			t.Errorf("expected content length 4, got %d", req.Header.ContentLength())
		}
		if len(req.Header.Peek(fasthttp.HeaderTransferEncoding)) != 0 {
			t.Errorf("expected no transfer encoding, got %q", req.Header.Peek(fasthttp.HeaderTransferEncoding))
		}
	})

	t.Run("Trailer header entry is ignored", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header: http.Header{
				"Trailer": []string{"X-Foo"},
			},
			Body:          io.NopCloser(strings.NewReader("data")),
			ContentLength: 4,
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if len(req.Header.Peek(fasthttp.HeaderTrailer)) != 0 {
			t.Errorf("expected no trailer announcement, got %q", req.Header.Peek(fasthttp.HeaderTrailer))
		}
		if req.Header.ContentLength() != 4 {
			t.Errorf("expected content length 4, got %d", req.Header.ContentLength())
		}
	})

	t.Run("trailers with known content length force chunked", func(t *testing.T) {
		t.Parallel()
		// HTTP/2 requests can carry both a known content length and
		// trailers; HTTP/1.x can only transport the trailers after a
		// chunked body.
		bodyContent := "data"
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Checksum": []string{"abc123"},
			},
			Body:          io.NopCloser(strings.NewReader(bodyContent)),
			ContentLength: int64(len(bodyContent)),
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if req.Header.ContentLength() != -1 {
			t.Errorf("expected content length -1, got %d", req.Header.ContentLength())
		}

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
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

	t.Run("trailers without body are dropped", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Checksum": []string{"abc123"},
			},
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if len(req.Header.Peek(fasthttp.HeaderTrailer)) != 0 {
			t.Errorf("expected no trailer announcement, got %q", req.Header.Peek(fasthttp.HeaderTrailer))
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if !req.Header.ConnectionClose() {
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		trailer := string(req.Header.TrailerHeader())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		trailer := string(req.Header.TrailerHeader())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
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

	t.Run("trailer values from http.ReadRequest are synced at EOF", func(t *testing.T) {
		t.Parallel()
		raw := "POST /upload HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"Transfer-Encoding: chunked\r\n" +
			"Trailer: X-Final\r\n" +
			"\r\n" +
			"4\r\ndata\r\n0\r\nX-Final: done\r\n\r\n"
		httpReq, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatalf("unexpected error reading raw request: %v", err)
		}
		if got := httpReq.Trailer.Get("X-Final"); got != "" {
			t.Fatalf("expected empty trailer value before the body is read, got %q", got)
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}

		var parsed fasthttp.Request
		if err := parsed.Read(bufio.NewReader(bytes.NewReader(buf.Bytes()))); err != nil {
			t.Fatalf("unexpected error reading request back: %v", err)
		}
		if string(parsed.Body()) != "data" {
			t.Errorf("expected body data, got %q", parsed.Body())
		}
		if string(parsed.Header.Peek("X-Final")) != "done" {
			t.Errorf("expected trailer value done, got %q", parsed.Header.Peek("X-Final"))
		}
	})

	t.Run("trailer values are synced when the body is drained", func(t *testing.T) {
		t.Parallel()
		raw := "POST /upload HTTP/1.1\r\n" +
			"Host: example.com\r\n" +
			"Transfer-Encoding: chunked\r\n" +
			"Trailer: X-Final\r\n" +
			"\r\n" +
			"4\r\ndata\r\n0\r\nX-Final: done\r\n\r\n"
		httpReq, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatalf("unexpected error reading raw request: %v", err)
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		if string(req.Body()) != "data" {
			t.Errorf("expected body data, got %q", req.Body())
		}
		if string(req.Header.Peek("X-Final")) != "done" {
			t.Errorf("expected trailer value done, got %q", req.Header.Peek("X-Final"))
		}
	})

	t.Run("attached body with trailers is closed after write", func(t *testing.T) {
		t.Parallel()
		closed := false
		httpReq := &http.Request{
			Method:     "POST",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			Header:     http.Header{},
			Trailer: http.Header{
				"X-Checksum": []string{"abc123"},
			},
			Body:          &closeTrackingReader{Reader: strings.NewReader("data"), closed: &closed},
			ContentLength: -1,
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}

		if !closed {
			t.Error("expected the attached body to be closed after write")
		}
	})

	t.Run("explicit Authorization header wins over URL credentials", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			URL: &url.URL{
				Path: "/",
				User: url.UserPassword("user", "pass"),
			},
			Header: http.Header{
				"Authorization": []string{"Bearer explicit-token"},
			},
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}

		if !strings.Contains(buf.String(), "Authorization: Bearer explicit-token\r\n") {
			t.Errorf("expected the explicit Authorization header on the wire, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "Basic") {
			t.Errorf("expected no Basic credentials on the wire, got:\n%s", buf.String())
		}
	})

	t.Run("URL credentials become Basic authorization", func(t *testing.T) {
		t.Parallel()
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: "/",
			Proto:      "HTTP/1.1",
			Host:       "example.com",
			URL: &url.URL{
				Path: "/",
				User: url.UserPassword("user", "pass"),
			},
			Header: http.Header{},
		}

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		var buf bytes.Buffer
		bw := bufio.NewWriter(&buf)
		if err := req.Write(bw); err != nil {
			t.Fatalf("unexpected error writing request: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("unexpected error flushing request: %v", err)
		}

		// base64("user:pass"), the same credentials net/http.Client would send.
		if !strings.Contains(buf.String(), "Authorization: Basic dXNlcjpwYXNz\r\n") {
			t.Errorf("expected Basic credentials on the wire, got:\n%s", buf.String())
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

		var req fasthttp.Request
		ConvertNetHTTPRequestToFastHTTPRequest(httpReq, &req)

		_, err := io.ReadAll(req.BodyStream())
		if err == nil {
			t.Fatal("expected error when reading body stream, got nil")
		}
	})
}
