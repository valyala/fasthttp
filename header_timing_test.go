package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

var strFoobar = []byte("foobar.com")

type benchReadBuf struct {
	s []byte
	n int
}

func (r *benchReadBuf) Read(p []byte) (int, error) {
	if r.n == len(r.s) {
		return 0, io.EOF
	}

	n := copy(p, r.s[r.n:])
	r.n += n
	return n, nil
}

func BenchmarkRequestHeaderRead(b *testing.B) {
	var h RequestHeader
	buf := &benchReadBuf{
		s: []byte("GET /foo/bar HTTP/1.1\r\nHost: foobar.com\r\nUser-Agent: aaa.bbb\r\nReferer: http://google.com/aaa/bbb\r\n\r\n"),
	}
	br := bufio.NewReader(buf)
	for i := 0; i < b.N; i++ {
		buf.n = 0
		br.Reset(buf)
		if err := h.Read(br); err != nil {
			b.Fatalf("unexpected error when reading header: %s", err)
		}
	}
}

func BenchmarkResponseHeaderRead(b *testing.B) {
	var h ResponseHeader
	buf := &benchReadBuf{
		s: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 1256\r\nServer: aaa 1/2.3\r\nTest: 1.2.3\r\n\r\n"),
	}
	br := bufio.NewReader(buf)
	for i := 0; i < b.N; i++ {
		buf.n = 0
		br.Reset(buf)
		if err := h.Read(br); err != nil {
			b.Fatalf("unexpected error when reading header: %s", err)
		}
	}
}

func BenchmarkRequestHeaderPeekBytesCanonical(b *testing.B) {
	var h RequestHeader
	h.SetBytesV("Host", strFoobar)
	for i := 0; i < b.N; i++ {
		v := h.PeekBytes(strHost)
		if !bytes.Equal(v, strFoobar) {
			b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
		}
	}
}

func BenchmarkRequestHeaderPeekBytesNonCanonical(b *testing.B) {
	var h RequestHeader
	h.SetBytesV("Host", strFoobar)
	hostBytes := []byte("HOST")
	for i := 0; i < b.N; i++ {
		v := h.PeekBytes(hostBytes)
		if !bytes.Equal(v, strFoobar) {
			b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
		}
	}
}
