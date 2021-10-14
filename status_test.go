package fasthttp

import (
	"bytes"
	"testing"
)

func TestStatusLine(t *testing.T) {
	t.Parallel()

	testStatusLine(t, -1, []byte("HTTP/1.1 -1 Unknown Status Code\r\n"))
	testStatusLine(t, 99, []byte("HTTP/1.1 99 Unknown Status Code\r\n"))
	testStatusLine(t, 200, []byte("HTTP/1.1 200 OK\r\n"))
	testStatusLine(t, 512, []byte("HTTP/1.1 512 Unknown Status Code\r\n"))
	testStatusLine(t, 512, []byte("HTTP/1.1 512 Unknown Status Code\r\n"))
	testStatusLine(t, 520, []byte("HTTP/1.1 520 Unknown Status Code\r\n"))
}

func testStatusLine(t *testing.T, statusCode int, expected []byte) {
	line := statusLine(statusCode)
	if !bytes.Equal(expected, line) {
		t.Fatalf("unexpected status line %s. Expecting %s", string(line), string(expected))
	}
}
