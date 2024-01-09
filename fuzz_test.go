package fasthttp

import (
	"bufio"
	"bytes"
	"testing"
)

func FuzzCookieParse(f *testing.F) {
	inputs := []string{
		`xxx=yyy`,
		`xxx=yyy; expires=Tue, 10 Nov 2009 23:00:00 GMT; domain=foobar.com; path=/a/b`,
		" \n\t\"",
	}
	for _, input := range inputs {
		f.Add([]byte(input))
	}
	c := AcquireCookie()
	defer ReleaseCookie(c)
	f.Fuzz(func(t *testing.T, cookie []byte) {
		_ = c.ParseBytes(cookie)

		w := bytes.Buffer{}
		if _, err := c.WriteTo(&w); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func FuzzVisitHeaderParams(f *testing.F) {
	inputs := []string{
		`application/json; v=1; foo=bar; q=0.938; param=param; param="big fox"; q=0.43`,
		`*/*`,
		`\\`,
		`text/plain; foo="\\\"\'\\''\'"`,
	}
	for _, input := range inputs {
		f.Add([]byte(input))
	}
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
	res := AcquireResponse()
	defer ReleaseResponse(res)

	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Type: aa\r\nContent-Length: 10\r\n\r\n9876543210"), 1024*1024)

	f.Fuzz(func(t *testing.T, body []byte, max int) {
		_ = res.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), max)
		w := bytes.Buffer{}
		_, _ = res.WriteTo(&w)
	})
}

func FuzzRequestReadLimitBody(f *testing.F) {
	req := AcquireRequest()
	defer ReleaseRequest(req)

	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\nfoobar\r\n\r\n"), 1024*1024)

	f.Fuzz(func(t *testing.T, body []byte, max int) {
		_ = req.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), max)
		w := bytes.Buffer{}
		_, _ = req.WriteTo(&w)
	})
}

func FuzzURIUpdateBytes(f *testing.F) {
	u := AcquireURI()
	defer ReleaseURI(u)

	f.Add([]byte(`http://foobar.com/aaa/bb?cc`))
	f.Add([]byte(`//foobar.com/aaa/bb?cc`))
	f.Add([]byte(`/aaa/bb?cc`))
	f.Add([]byte(`xx?yy=abc`))

	f.Fuzz(func(t *testing.T, uri []byte) {
		u.UpdateBytes(uri)

		w := bytes.Buffer{}
		if _, err := u.WriteTo(&w); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
