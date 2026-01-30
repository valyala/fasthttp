package fasthttp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"
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
	f.Add([]byte(" 0\nTrAnsfer-EnCoding:0\n\n0\r\n1:0\n        00\n 000\n\n"), 24922)
	f.Add([]byte(" 0\n0:\n 0\n :\n"), 1048532)

	// Case found by OSS-Fuzz.
	b, err := base64.StdEncoding.DecodeString("oeYAdyAyClRyYW5zZmVyLUVuY29kaW5nOmlka7AKCjANCiA6MAogOgogOgogPgAAAAAAAAAgICAhICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgCiA6CiA6CiAgOgogOgogYDogCiAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgIAogOgogOgogIDoKIDoKIGA6IAoKIDoKBSAgOgogOgogOgogOgogIDoKIDoKIGA6IAAgIAA6CiA6CiA6CjoKIDoKIDoWCiAyIOgKIDogugogOjAKIDoKIDoKBSAgOgogOgogOgogOgogIDoKIDoKIGA6IAAgIAAAAAAAAABaYQ==")
	if err != nil {
		panic(err)
	}
	f.Add(b[:len(b)-2], int(binary.LittleEndian.Uint16(b[len(b)-2:])))

	f.Fuzz(func(t *testing.T, body []byte, maxBodySize int) {
		if len(body) > 1024*1024 || maxBodySize > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if maxBodySize <= 0 {
			return
		}

		res := AcquireResponse()
		defer ReleaseResponse(res)

		_ = res.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), maxBodySize)
	})
}

func FuzzRequestReadLimitBody(f *testing.F) {
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\nfoobar\r\n\r\n"), 1024)

	f.Fuzz(func(t *testing.T, body []byte, maxBodySize int) {
		if len(body) > 1024*1024 || maxBodySize > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if maxBodySize <= 0 {
			return
		}

		req := AcquireRequest()
		defer ReleaseRequest(req)

		_ = req.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), maxBodySize)
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

func FuzzURIParse(f *testing.F) {
	f.Add(`http://foobar.com/aaa/bb?cc#dd`)
	f.Add(`http://google.com?github.com`)
	f.Add(`http://google.com#@github.com`)

	f.Fuzz(func(t *testing.T, uri string) {
		// Limit the size of the URI to avoid OOMs or timeouts.
		// When using Server or Client the maximum URI is dicated by the maximum header size,
		// which defaults to defaultReadBufferSize (4096 bytes).
		if len(uri) > defaultReadBufferSize {
			return
		}

		var u URI

		uri = strings.ToLower(uri)

		if !strings.HasPrefix(uri, "http://") && !strings.HasPrefix(uri, "https://") {
			return
		}

		if u.Parse(nil, []byte(uri)) != nil {
			return
		}

		nu, err := url.Parse(uri)
		if err != nil {
			return
		}

		if string(u.Host()) != nu.Host {
			t.Fatalf("%q: unexpected host: %q. Expecting %q", uri, u.Host(), nu.Host)
		}
		if string(u.QueryString()) != nu.RawQuery {
			t.Fatalf("%q: unexpected query string: %q. Expecting %q", uri, u.QueryString(), nu.RawQuery)
		}
	})
}

func FuzzTestHeaderScanner(f *testing.F) {
	f.Add([]byte("Host: example.com\r\nUser-Agent: Go-http-client/1.1\r\nAccept-Encoding: gzip, deflate\r\n\r\n"))
	f.Add([]byte("Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 27\r\n\r\nname=John+Doe&age=30"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if !bytes.Contains(data, []byte("\r\n\r\n")) {
			return
		}
		if len(data) > 1024*1024 {
			return
		}

		t.Logf("%q", data)

		tmp, herr := textproto.NewReader(bufio.NewReader(bytes.NewReader(data))).ReadMIMEHeader()
		h := map[string][]string(tmp)

		var s headerScanner
		s.b = data
		f := make(map[string][]string)
		for s.next() {
			// ReadMIMEHeader normalizes header keys, headerScanner doesn't by default.
			normalizeHeaderKey(s.key, false)

			// textproto.ReadMIMEHeader will validate the header value, since we compare
			// errors we should do this as well.
			for _, c := range s.value {
				if !validHeaderValueByte(c) {
					s.err = fmt.Errorf("malformed MIME header: invalid byte %q in value %q for key %q", c, s.value, s.key)
				}
			}
			if s.err != nil {
				break
			}

			key := string(s.key)
			value := string(s.value)

			if _, ok := f[key]; !ok {
				f[key] = []string{}
			}
			f[key] = append(f[key], value)
		}

		if s.err != nil && herr == nil {
			t.Errorf("unexpected error from headerScanner: %v: %v", s.err, h)
		} else if s.err == nil && herr != nil {
			t.Errorf("unexpected error from textproto.NewReader: %v: %v", herr, f)
		}

		if !reflect.DeepEqual(h, f) {
			t.Errorf("headers mismatch:\ntextproto: %v\nfasthttp: %v", h, f)
		}
	})
}

func FuzzRequestReadLimitBodyAllocations(f *testing.F) {
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\nfoobar\r\n\r\n"), 1024)
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nWithTabs: \t v1 \t\r\nWithTabs-Start: \t \t v1 \r\nWithTabs-End: v1 \t \t\t\t\r\nWithTabs-Multi-Line: \t v1 \t;\r\n \t v2 \t;\r\n\t v3\r\n\r\n"), 1024)

	f.Fuzz(func(t *testing.T, body []byte, maxBodySize int) {
		if len(body) > 1024*1024 || maxBodySize > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if maxBodySize <= 0 {
			return
		}

		t.Logf("%d %q", maxBodySize, body)

		req := Request{}
		a := bytes.NewReader(body)
		b := bufio.NewReader(a)

		if err := req.ReadLimitBody(b, maxBodySize); err != nil {
			return
		}

		n := testing.AllocsPerRun(200, func() {
			req.Reset()
			a.Reset(body)
			b.Reset(a)

			_ = req.ReadLimitBody(b, maxBodySize)
		})

		if n != 0 {
			t.Fatalf("expected 0 allocations, got %f", n)
		}
	})
}
