package fasthttp

import (
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
