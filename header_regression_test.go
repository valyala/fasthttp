package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestIssue28ResponseWithoutBodyNoContentType(t *testing.T) {
	t.Parallel()

	var r Response

	// Empty response without content-type
	s := r.String()
	if strings.Contains(s, "Content-Type") {
		t.Fatalf("unexpected Content-Type found in response header with empty body: %q", s)
	}

	// Explicitly set content-type
	r.Header.SetContentType("foo/bar")
	s = r.String()
	if !strings.Contains(s, "Content-Type: foo/bar\r\n") {
		t.Fatalf("missing explicitly set content-type for empty response: %q", s)
	}

	// Non-empty response.
	r.Reset()
	r.SetBodyString("foobar")
	s = r.String()
	if !strings.Contains(s, fmt.Sprintf("Content-Type: %s\r\n", defaultContentType)) {
		t.Fatalf("missing default content-type for non-empty response: %q", s)
	}

	// Non-empty response with custom content-type.
	r.Header.SetContentType("aaa/bbb")
	s = r.String()
	if !strings.Contains(s, "Content-Type: aaa/bbb\r\n") {
		t.Fatalf("missing custom content-type: %q", s)
	}
}

// A response whose status forbids a body must not be written back out with a
// Content-Length.
//
// RFC 9110 section 8.6 forbids Content-Length on a response with a 1xx or 204
// status code, and fasthttp already refuses to set one for those statuses
// (ResponseHeader.SetContentLength). Parsing keeps whatever the peer sent so
// the value stays observable, but serializing it re-emits a Content-Length
// that announces a body the status code does not allow. That is what a proxy
// forwarding a 204 would put on the wire.
//
// Only 204 is covered here: a 1xx response carrying Content-Length is already
// rejected while parsing, so the serialization guard is unreachable for it.
func TestResponseBodylessStatusOmitsContentLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		raw    string
		status string
	}{
		{"204", "HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\n", "HTTP/1.1 204 No Content\r\n"},
	} {
		var resp Response
		if err := resp.Read(bufio.NewReader(strings.NewReader(tc.raw))); err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		// The parsed value stays observable; only the wire form drops it.
		if cl := resp.Header.ContentLength(); cl != 5 {
			t.Fatalf("%s: unexpected content-length %d. Expecting 5", tc.name, cl)
		}

		// Re-serializing (what a proxy does) must not reintroduce it.
		s := resp.String()
		if !strings.HasPrefix(s, tc.status) {
			t.Fatalf("%s: unexpected status line in %q", tc.name, s)
		}
		if strings.Contains(s, "Content-Length") {
			t.Fatalf("%s: unexpected Content-Length in %q", tc.name, s)
		}
		if strings.Contains(s, "Content-Type") {
			t.Fatalf("%s: unexpected Content-Type in %q", tc.name, s)
		}
	}
}

// A 304 has no body of its own, but RFC 9110 section 8.6 still allows
// Content-Length there, where it describes the corresponding 200 response. A
// proxy forwarding a parsed 304 must keep it, or the client cannot tell how
// large the cached representation is.
func TestResponseNotModifiedKeepsContentLength(t *testing.T) {
	t.Parallel()

	var resp Response
	if err := resp.Read(bufio.NewReader(strings.NewReader(
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\n\r\n"))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cl := resp.Header.ContentLength(); cl != 5 {
		t.Fatalf("unexpected content-length %d. Expecting 5", cl)
	}

	s := resp.String()
	if !strings.Contains(s, "Content-Length: 5\r\n") {
		t.Fatalf("expecting Content-Length to be preserved in %q", s)
	}
}

// A 204 must not gain a default Content-Type, but an explicitly set one is
// still emitted: a response without a body may still carry representation
// metadata (RFC 9110 section 15.3.5).
func TestResponseNoContentExplicitContentType(t *testing.T) {
	t.Parallel()

	var r Response
	r.Header.SetStatusCode(StatusNoContent)
	r.Header.SetContentType("application/json")

	s := r.String()
	if !strings.Contains(s, "Content-Type: application/json\r\n") {
		t.Fatalf("expecting explicitly set Content-Type in %q", s)
	}
	if strings.Contains(s, "Content-Length") {
		t.Fatalf("unexpected Content-Length in %q", s)
	}
}

// A 200 response keeps its Content-Length; the rule above must not be
// over-applied.
func TestResponseBodyStatusKeepsContentLength(t *testing.T) {
	t.Parallel()

	var resp Response
	if err := resp.Read(bufio.NewReader(strings.NewReader("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cl := resp.Header.ContentLength(); cl != 5 {
		t.Fatalf("unexpected content-length %d. Expecting 5", cl)
	}
	if body := string(resp.Body()); body != "hello" {
		t.Fatalf("unexpected body %q. Expecting %q", body, "hello")
	}
	s := resp.String()
	if !strings.Contains(s, "Content-Length: 5\r\n") {
		t.Fatalf("expecting Content-Length in %q", s)
	}
	if !strings.Contains(s, "Content-Type: text/plain; charset=utf-8\r\n") {
		t.Fatalf("expecting Content-Type in %q", s)
	}
}

func TestIssue6RequestHeaderSetContentType(t *testing.T) {
	t.Parallel()

	testIssue6RequestHeaderSetContentType(t, MethodGet)
	testIssue6RequestHeaderSetContentType(t, MethodPost)
	testIssue6RequestHeaderSetContentType(t, MethodPut)
	testIssue6RequestHeaderSetContentType(t, MethodPatch)
}

func testIssue6RequestHeaderSetContentType(t *testing.T, method string) {
	contentType := "application/json"
	contentLength := 123

	var h RequestHeader
	h.SetMethod(method)
	h.SetRequestURI("http://localhost/test")
	h.SetHost("localhost")
	h.SetContentType(contentType)
	h.SetContentLength(contentLength)

	issue6VerifyRequestHeader(t, &h, contentType, contentLength, method)

	s := h.String()

	var h1 RequestHeader

	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := h1.Read(br); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	issue6VerifyRequestHeader(t, &h1, contentType, contentLength, method)
}

func issue6VerifyRequestHeader(t *testing.T, h *RequestHeader, contentType string, contentLength int, method string) {
	if string(h.ContentType()) != contentType {
		t.Fatalf("unexpected content-type: %q. Expecting %q. method=%q", h.ContentType(), contentType, method)
	}
	if string(h.Method()) != method {
		t.Fatalf("unexpected method: %q. Expecting %q", h.Method(), method)
	}
	if h.ContentLength() != contentLength {
		t.Fatalf("unexpected content-length: %d. Expecting %d. method=%q", h.ContentLength(), contentLength, method)
	}
}
