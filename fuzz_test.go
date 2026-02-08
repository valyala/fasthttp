package fasthttp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
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
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3;ext=1\r\nabc\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1;ext=\"ok\" \r\nx\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1;ext=\"a\\\\\\\"b\";foo=bar\r\nx\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: Foo\r\n\r\n3\r\nabc\r\n0\r\nFoo: bar\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nA\r\n0123456789\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3 \t\r\nabc\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.0 200 OK\r\nContent-Length: 5\r\n\r\nhello"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 5\r\n\r\nhello"), 1024)
	f.Add([]byte("HTTP/1.0 200 OK\r\nConnection: close\r\n\r\nBody here\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\n\r\nBody here\n"), 1024)
	f.Add([]byte("HTTP/1.0 303 \r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 256\r\nConnection: keep-alive, close\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.0 200 OK\r\nTransfer-Encoding: bogus\r\n\r\nBody here\n"), 1024)
	f.Add([]byte("HTTP/1.0 200 OK\r\nTransfer-Encoding: bogus\r\nContent-Length: 10\r\n\r\nBody here\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\n Content-type: text/html\r\nFoo: bar\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\nContent-Length: 7\r\n\r\nGopher hey\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 204 No Content\r\n\r\nBody should not be read!\n"), 1024)
	f.Add([]byte("HTTP/1.1 200\r\nContent-Length: 0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: Chunked\r\n\r\n1\r\na\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 000\nTrAnsfer-EnCoding:Chunked\nTrAnsfer-EnCoding:Chunked\n\n0\r\n\r\n"), 998)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked \r\n\r\n1\r\na\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0;ext=done\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: Foo, Bar\r\n\r\n1\r\nx\r\n0\r\nFoo: 1\r\nBar: 2\r\n\r\n"), 1024)
	f.Add([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nf;note=v\r\n0123456789abcde\r\n0\r\n\r\n"), 1024)

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

		t.Logf("%q %d", body, maxBodySize)

		var res Response
		fastErr := res.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), maxBodySize)
		fastBody := res.Body()

		netBody, netErr := readResponseBodyNetHTTPLimit(body, maxBodySize)
		if fastErr != nil {
			return
		}
		if netErr != nil {
			/*if (len(body) > 0 && (body[0] == '\r' || body[0] == '\n')) &&
				strings.Contains(netErr.Error(), "malformed HTTP response") {
				return
			}*/
			t.Fatalf("fasthttp:\n%s; net/http err=%v", res.String(), netErr)
		}
		if !bytes.Equal(fastBody, netBody) {
			t.Fatalf("body mismatch: fasthttp=%q net/http=%q", fastBody, netBody)
		}
	})
}

func FuzzRequestReadLimitBody(f *testing.F) {
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nContent-Type: aa\r\n\r\n6\r\nfoobar\r\n3\r\nbaz\r\n0\r\nfoobar\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n3;ext=1\r\nabc\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n1;ext=\"ok\" \r\nx\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n1;ext=\"a\\\\\\\"b\";foo=bar\r\nx\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nTrailer: Foo\r\n\r\n3\r\nabc\r\n0\r\nFoo: bar\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\nA\r\n0123456789\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n3 \t\r\nabc\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("GET /foo?bar=baz HTTP/1.1\r\nHost: a.com\r\nUser-Agent: fuzz\r\nAccept: */*\r\n\r\n"), 1024)
	f.Add([]byte("GET http://a.com/abs/path?x=1 HTTP/1.1\r\nHost: a.com\r\n\r\n"), 1024)
	f.Add([]byte("OPTIONS * HTTP/1.1\r\nHost: a.com\r\n\r\n"), 1024)
	f.Add([]byte("CONNECT a.com:443 HTTP/1.1\r\nHost: a.com:443\r\n\r\n"), 1024)
	f.Add([]byte("POST /submit HTTP/1.1\r\nHost: a.com\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 7\r\n\r\nname=aa"), 1024)
	f.Add([]byte("GET http://user@a.com/path HTTP/1.1\r\nHost: a.com\r\n\r\n"), 1024)
	f.Add([]byte("GET http://[fe80::1%25en0]/ HTTP/1.1\r\nHost: [fe80::1%25en0]\r\n\r\n"), 1024)
	f.Add([]byte("CONNECT user@a.com:443 HTTP/1.1\r\nHost: a.com:443\r\n\r\n"), 1024)
	f.Add([]byte("GET http://a.com HTTP/1.1\r\nHost: ignored.com\r\n\r\n"), 1024)
	f.Add([]byte("GET http://gooGle.com/foO/%20bar?xxx#aaa HTTP/1.1\r\nHost: aa.cOM\r\n\r\ntrail"), 1024)
	f.Add([]byte("GET A://#0000000 HTTP/0.0\nHost:0\r\n\r\n"), 1024)
	f.Add([]byte("0 /% HTTP/0.0\nHost:0\r\n\r\n"), 1024)
	f.Add([]byte("\n0 * HTTP/0.0\nHost:0\r\n\r\n"), 1024)
	f.Add([]byte("GET / HTTP/1.1\r\nHost: aaa.com\r\nhost: bbb.com\r\n\r\n"), 1024)
	f.Add([]byte("GET /foo/bar HTTP/1.1\r\n foo: bar\r\n\r\n"), 1024)
	f.Add([]byte("CONNECT /rpc HTTP/1.1\r\nHost: a.com\r\n\r\n"), 1024)
	f.Add([]byte("CONNECT [::1]:443 HTTP/1.1\r\nHost: [::1]:443\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: Chunked\r\n\r\n1\r\na\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n0;ext=done\r\n\r\n"), 1024)
	f.Add([]byte("POST / HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\nTrailer: Foo, Bar\r\n\r\n1\r\nx\r\n0\r\nFoo: 1\r\nBar: 2\r\n\r\n"), 1024)

	f.Fuzz(func(t *testing.T, body []byte, maxBodySize int) {
		if len(body) > 1024*1024 || maxBodySize > 1024*1024 {
			return
		}
		// Only test with a max for the body, otherwise a very large Content-Length will just OOM.
		if maxBodySize <= 0 {
			return
		}

		t.Logf("%q %d", body, maxBodySize)

		var req Request
		fastErr := req.ReadLimitBody(bufio.NewReader(bytes.NewReader(body)), maxBodySize)
		fastBody := req.Body()

		netBody, netErr := readRequestBodyNetHTTPLimit(body, maxBodySize)
		if fastErr != nil {
			return
		}
		if netErr != nil {
			t.Fatalf("fasthttp:\n%s; net/http err=%v", req.String(), netErr)
		}
		if !bytes.Equal(fastBody, netBody) {
			t.Fatalf("body mismatch: fasthttp=%q net/http=%q", fastBody, netBody)
		}
	})
}

func readResponseBodyNetHTTPLimit(raw []byte, maxBodySize int) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return readBodyWithLimit(resp.Body, maxBodySize)
}

func readRequestBodyNetHTTPLimit(raw []byte, maxBodySize int) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	req, err := http.ReadRequest(br)
	if err != nil {
		return nil, err
	}
	defer req.Body.Close()

	return readBodyWithLimit(req.Body, maxBodySize)
}

func readBodyWithLimit(r io.Reader, maxBodySize int) ([]byte, error) {
	if maxBodySize <= 0 {
		return io.ReadAll(r)
	}

	limit := int64(maxBodySize) + 1
	body, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodySize {
		return nil, ErrBodyTooLarge
	}
	return body, nil
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
	f.Add([]byte("X-Empty:\r\n\r\n"))
	f.Add([]byte("X-WS: \t \t\r\n\r\n"))
	f.Add([]byte("X-Quoted: \"a,b\"; q=1\r\n\r\n"))
	f.Add([]byte("Set-Cookie: a=b; Path=/; HttpOnly\r\nSet-Cookie: c=d\r\n\r\n"))

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
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n3;ext=1\r\nabc\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\nA\r\n0123456789\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST /submit HTTP/1.1\r\nHost: a.com\r\nContent-Length: 7\r\n\r\nname=aa"), 1024)
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: Chunked\r\n\r\n1\r\na\r\n0\r\n\r\n"), 1024)
	f.Add([]byte("POST /a HTTP/1.1\r\nHost: a.com\r\nTransfer-Encoding: chunked\r\n\r\n0;ext=done\r\n\r\n"), 1024)

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
