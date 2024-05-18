package fasthttp

import (
	"bufio"
	"bytes"
	"testing"
)

func FuzzCookieParse(f *testing.F) {
	f.Add([]byte(`xxx=yyy`))
	f.Add([]byte(`xxx=yyy; expires=Tue, 10 Nov 2009 23:00:00 GMT; domain=foobar.com; path=/a/b`))
	f.Add([]byte(" \n\t\""))

	f.Fuzz(func(t *testing.T, cookie []byte) {
		var c Cookie

		_ = c.ParseBytes(cookie)

		w := bytes.Buffer{}
		if _, err := c.WriteTo(&w); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func FuzzVisitHeaderParams(f *testing.F) {
	f.Add([]byte(`application/json; v=1; foo=bar; q=0.938; param=param; param="big fox"; q=0.43`))
	f.Add([]byte(`*/*`))
	f.Add([]byte(`\\`))
	f.Add([]byte(`text/plain; foo="\\\"\'\\''\'"`))

	f.Fuzz(func(t *testing.T, header []byte) {
		VisitHeaderParams(header, func(key, value []byte) bool {
			if len(key) == 0 {
				t.Errorf("Unexpected length zero parameter, failed input was: %s", header)
			}
			return true
		})
	})
}

func FuzzResponseReadLimitBody(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 10\r\n\r\n9876543210"), 1024)

	f.Fuzz(func(t *testing.T, body []byte, max int) {
		if len(body) > 1024*1024 || max > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if max <= 0 {
			return
		}

		res := AcquireResponse()
		defer ReleaseResponse(res)

		_ = res.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), max)
	})
}

func FuzzRequestReadLimitBody(f *testing.F) {
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\nfoobar\r\n\r\n"), 1024)

	f.Fuzz(func(t *testing.T, body []byte, max int) {
		if len(body) > 1024*1024 || max > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if max <= 0 {
			return
		}

		req := AcquireRequest()
		defer ReleaseRequest(req)

		_ = req.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), max)
	})
}

func FuzzURIUpdateBytes(f *testing.F) {
	f.Add([]byte(`http://foobar.com/aaa/bb?cc`))
	f.Add([]byte(`//foobar.com/aaa/bb?cc`))
	f.Add([]byte(`/aaa/bb?cc`))
	f.Add([]byte(`xx?yy=abc`))

	f.Fuzz(func(t *testing.T, uri []byte) {
		var u URI

		u.UpdateBytes(uri)

		w := bytes.Buffer{}
		if _, err := u.WriteTo(&w); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
