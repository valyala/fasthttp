package fasthttp

import (
	"bufio"
	"bytes"
	"testing"
)

func TestRequestHeaderSetContentType_Issue6(t *testing.T) {
	contentType := "application/json"

	var h RequestHeader
	h.SetRequestURI("http://localhost/test")
	h.SetContentType(contentType)

	if string(h.ContentType()) != contentType {
		t.Fatalf("unexpected content-type: %q. Expecting %q", h.ContentType(), contentType)
	}
	s := h.String()

	var h1 RequestHeader

	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := h1.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(h1.ContentType()) != contentType {
		t.Fatalf("unexpected content-type: %q. Expecting %q", h1.ContentType(), contentType)
	}
}
