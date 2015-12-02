package fasthttp

import (
	"bufio"
	"bytes"
	"testing"
)

func TestIssue6RequestHeaderSetContentType(t *testing.T) {
	testIssue6RequestHeaderSetContentType(t, "GET")
	testIssue6RequestHeaderSetContentType(t, "POST")
	testIssue6RequestHeaderSetContentType(t, "PUT")
	testIssue6RequestHeaderSetContentType(t, "PATCH")
}

func testIssue6RequestHeaderSetContentType(t *testing.T, method string) {
	contentType := "application/json"
	contentLength := 123

	var h RequestHeader
	h.SetMethod(method)
	h.SetRequestURI("http://localhost/test")
	h.SetContentType(contentType)
	h.SetContentLength(contentLength)

	issue6VerifyRequestHeader(t, &h, contentType, contentLength, method)

	s := h.String()

	var h1 RequestHeader

	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := h1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
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
	if method != "GET" {
		if h.ContentLength() != contentLength {
			t.Fatalf("unexpected content-length: %d. Expecting %d. method=%q", h.ContentLength(), contentLength, method)
		}
	} else if h.ContentLength() != 0 {
		t.Fatalf("unexpected content-length for GET method: %d. Expecting 0", h.ContentLength())
	}
}
