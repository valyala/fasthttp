package fasthttp

import (
	"bytes"
	"testing"
)

func BenchmarkStatusLine99(b *testing.B) {
	benchmarkStatusLine(b, 99, []byte("HTTP/1.1 99 Unknown Status Code\r\n"))
}

func BenchmarkStatusLine200(b *testing.B) {
	benchmarkStatusLine(b, 200, []byte("HTTP/1.1 200 OK\r\n"))
}

func BenchmarkStatusLine512(b *testing.B) {
	benchmarkStatusLine(b, 512, []byte("HTTP/1.1 512 Unknown Status Code\r\n"))
}

func benchmarkStatusLine(b *testing.B, statusCode int, expected []byte) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			line := formatStatusLine(nil, strHTTP11, statusCode, s2b(StatusMessage(statusCode)))
			if !bytes.Equal(expected, line) {
				b.Fatalf("unexpected status line %q. Expecting %q", string(line), string(expected))
			}
		}
	})
}
