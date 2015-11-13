package fasthttp

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClientConn struct {
	net.Conn
	s []byte
	n int
	v interface{}
}

func (c *fakeClientConn) Write(b []byte) (int, error) {
	return len(b), nil
}

func (c *fakeClientConn) Read(b []byte) (int, error) {
	n := 0
	for len(b) > 0 {
		if c.n == len(c.s) {
			c.n = 0
			return n, nil
		}
		n = copy(b, c.s[c.n:])
		c.n += n
		b = b[n:]
	}
	return n, nil
}

func (c *fakeClientConn) Close() error {
	releaseFakeServerConn(c)
	return nil
}

func releaseFakeServerConn(c *fakeClientConn) {
	c.n = 0
	fakeClientConnPool.Put(c.v)
}

func acquireFakeServerConn(s []byte) *fakeClientConn {
	v := fakeClientConnPool.Get()
	if v == nil {
		c := &fakeClientConn{
			s: s,
		}
		c.v = c
		return c
	}
	return v.(*fakeClientConn)
}

var fakeClientConnPool sync.Pool

func BenchmarkClientGetFastServer(b *testing.B) {
	body := []byte("012345678912")
	s := []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	requestURI := []byte("http://foobar.com/aaa/bbb")
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return acquireFakeServerConn(s), nil
		},
	}

	b.RunParallel(func(pb *testing.PB) {
		var req Request
		var resp Response
		req.Header.RequestURI = requestURI
		for pb.Next() {
			if err := c.Do(&req, &resp); err != nil {
				b.Fatalf("unexpected error: %s", err)
			}
			if resp.Header.StatusCode != StatusOK {
				b.Fatalf("unexpected status code: %d", resp.Header.StatusCode)
			}
			if !bytes.Equal(resp.Body, body) {
				b.Fatalf("unexpected response body: %q. Expected %q", resp.Body, body)
			}
		}
	})
}

func fasthttpEchoHandler(ctx *RequestCtx) {
	ctx.Success("text/plain", ctx.Request.Header.RequestURI)
}

func nethttpEchoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(r.RequestURI))
}

func BenchmarkClientGetEndToEnd(b *testing.B) {
	addr := "127.0.0.1:8543"

	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		b.Fatalf("cannot listen %q: %s", addr, err)
	}

	ch := make(chan struct{})
	go func() {
		if err := Serve(ln, fasthttpEchoHandler); err != nil {
			b.Fatalf("error when serving requests: %s", err)
		}
		close(ch)
	}()

	requestURI := "/foo/bar?baz=123"
	url := "http://" + addr + requestURI
	b.RunParallel(func(pb *testing.PB) {
		var buf []byte
		for pb.Next() {
			statusCode, body, err := Get(buf, url)
			if err != nil {
				b.Fatalf("unexpected error: %s", err)
			}
			if statusCode != StatusOK {
				b.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
			}
			if !EqualBytesStr(body, requestURI) {
				b.Fatalf("unexpected response %q. Expecting %q", body, requestURI)
			}
			buf = body
		}
	})

	ln.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		b.Fatalf("server wasn't stopped")
	}
}

func BenchmarkNetHTTPClientGetEndToEnd(b *testing.B) {
	addr := "127.0.0.1:8542"

	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		b.Fatalf("cannot listen %q: %s", addr, err)
	}

	ch := make(chan struct{})
	go func() {
		if err := http.Serve(ln, http.HandlerFunc(nethttpEchoHandler)); err != nil && !strings.Contains(
			err.Error(), "use of closed network connection") {
			b.Fatalf("error when serving requests: %s", err)
		}
		close(ch)
	}()

	requestURI := "/foo/bar?baz=123"
	url := "http://" + addr + requestURI
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := http.Get(url)
			if err != nil {
				b.Fatalf("unexpected error: %s", err)
			}
			if resp.StatusCode != http.StatusOK {
				b.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode, http.StatusOK)
			}
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				b.Fatalf("unexpected error when reading response body: %s", err)
			}
			resp.Body.Close()
			if !EqualBytesStr(body, requestURI) {
				b.Fatalf("unexpected response %q. Expecting %q", body, requestURI)
			}
		}
	})

	ln.Close()
	select {
	case <-ch:
	case <-time.After(time.Second):
		b.Fatalf("server wasn't stopped")
	}
}
