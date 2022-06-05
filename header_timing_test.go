package fasthttp

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/valyala/bytebufferpool"
)

var strFoobar = []byte("foobar.com")

// it has the same length as Content-Type
var strNonSpecialHeader = []byte("Dontent-Type")

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
	b.RunParallel(func(pb *testing.PB) {
		var h RequestHeader
		buf := &benchReadBuf{
			s: []byte("GET /foo/bar HTTP/1.1\r\nHost: foobar.com\r\nUser-Agent: aaa.bbb\r\nReferer: http://google.com/aaa/bbb\r\n\r\n"),
		}
		br := bufio.NewReader(buf)
		for pb.Next() {
			buf.n = 0
			br.Reset(buf)
			if err := h.Read(br); err != nil {
				b.Fatalf("unexpected error when reading header: %v", err)
			}
		}
	})
}

func BenchmarkResponseHeaderRead(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h ResponseHeader
		buf := &benchReadBuf{
			s: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: 1256\r\nServer: aaa 1/2.3\r\nTest: 1.2.3\r\n\r\n"),
		}
		br := bufio.NewReader(buf)
		for pb.Next() {
			buf.n = 0
			br.Reset(buf)
			if err := h.Read(br); err != nil {
				b.Fatalf("unexpected error when reading header: %v", err)
			}
		}
	})
}

func BenchmarkRequestHeaderWrite(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h RequestHeader
		h.SetRequestURI("/foo/bar")
		h.SetHost("foobar.com")
		h.SetUserAgent("aaa.bbb")
		h.SetReferer("http://google.com/aaa/bbb")
		var w bytebufferpool.ByteBuffer
		for pb.Next() {
			if _, err := h.WriteTo(&w); err != nil {
				b.Fatalf("unexpected error when writing header: %v", err)
			}
			w.Reset()
		}
	})
}

func BenchmarkResponseHeaderWrite(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h ResponseHeader
		h.SetStatusCode(200)
		h.SetContentType("text/html")
		h.SetContentLength(1256)
		h.SetServer("aaa 1/2.3")
		h.Set("Test", "1.2.3")
		var w bytebufferpool.ByteBuffer
		for pb.Next() {
			if _, err := h.WriteTo(&w); err != nil {
				b.Fatalf("unexpected error when writing header: %v", err)
			}
			w.Reset()
		}
	})
}

// Result: 2.2 ns/op
func BenchmarkRequestHeaderPeekBytesSpecialHeader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h RequestHeader
		h.SetContentTypeBytes(strFoobar)
		for pb.Next() {
			v := h.PeekBytes(strContentType)
			if !bytes.Equal(v, strFoobar) {
				b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
			}
		}
	})
}

// Result: 2.9 ns/op
func BenchmarkRequestHeaderPeekBytesNonSpecialHeader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h RequestHeader
		h.SetBytesKV(strNonSpecialHeader, strFoobar)
		for pb.Next() {
			v := h.PeekBytes(strNonSpecialHeader)
			if !bytes.Equal(v, strFoobar) {
				b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
			}
		}
	})
}

// Result: 2.3 ns/op
func BenchmarkResponseHeaderPeekBytesSpecialHeader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h ResponseHeader
		h.SetContentTypeBytes(strFoobar)
		for pb.Next() {
			v := h.PeekBytes(strContentType)
			if !bytes.Equal(v, strFoobar) {
				b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
			}
		}
	})
}

// Result: 2.9 ns/op
func BenchmarkResponseHeaderPeekBytesNonSpecialHeader(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		var h ResponseHeader
		h.SetBytesKV(strNonSpecialHeader, strFoobar)
		for pb.Next() {
			v := h.PeekBytes(strNonSpecialHeader)
			if !bytes.Equal(v, strFoobar) {
				b.Fatalf("unexpected result: %q. Expected %q", v, strFoobar)
			}
		}
	})
}

func BenchmarkNormalizeHeaderKeyCommonCase(b *testing.B) {
	src := []byte("User-Agent-Host-Content-Type-Content-Length-Server")
	benchmarkNormalizeHeaderKey(b, src)
}

func BenchmarkNormalizeHeaderKeyLowercase(b *testing.B) {
	src := []byte("user-agent-host-content-type-content-length-server")
	benchmarkNormalizeHeaderKey(b, src)
}

func BenchmarkNormalizeHeaderKeyUppercase(b *testing.B) {
	src := []byte("USER-AGENT-HOST-CONTENT-TYPE-CONTENT-LENGTH-SERVER")
	benchmarkNormalizeHeaderKey(b, src)
}

func benchmarkNormalizeHeaderKey(b *testing.B, src []byte) {
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, len(src))
		for pb.Next() {
			copy(buf, src)
			normalizeHeaderKey(buf, false)
		}
	})
}

func BenchmarkRemoveNewLines(b *testing.B) {
	type testcase struct {
		value         string
		expectedValue string
	}

	var testcases = []testcase{
		{value: "MaliciousValue", expectedValue: "MaliciousValue"},
		{value: "MaliciousValue\r\n", expectedValue: "MaliciousValue  "},
		{value: "Malicious\nValue", expectedValue: "Malicious Value"},
		{value: "Malicious\rValue", expectedValue: "Malicious Value"},
	}

	for i, tcase := range testcases {
		caseName := strconv.FormatInt(int64(i), 10)
		b.Run(caseName, func(subB *testing.B) {
			subB.ReportAllocs()
			var h RequestHeader
			for i := 0; i < subB.N; i++ {
				h.Set("Test", tcase.value)
			}
			subB.StopTimer()
			actualValue := string(h.Peek("Test"))

			if actualValue != tcase.expectedValue {
				subB.Errorf("unexpected value, got: %+v", actualValue)
			}
		})
	}
}

func BenchmarkRequestHeaderIsGet(b *testing.B) {
	req := &RequestHeader{method: []byte(MethodGet)}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req.IsGet()
		}
	})
}
