//go:build !race

package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"testing"
)

func TestAllocationServeConn(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetStatusCode(StatusOK)
			ctx.SetBodyString("Hello, World!")
			ctx.Response.Header.Set("Content-Type", "text/plain; charset=utf-8")
		},
	}

	rw := &readWriter{}
	// Make space for the request and response here so it
	// doesn't allocate within the test.
	rw.r.Grow(1024)
	rw.w.Grow(1024)

	n := testing.AllocsPerRun(100, func() {
		rw.r.WriteString("GET / HTTP/1.1\r\nHost: google.com\r\nCookie: foo=bar\r\n\r\n")
		if err := s.ServeConn(rw); err != nil {
			t.Fatal(err)
		}

		// Reset the write buffer to make space for the next response.
		rw.w.Reset()
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationClient(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen: %v", err)
	}
	defer ln.Close()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetStatusCode(StatusOK)
			ctx.SetBodyString("Hello, World!")
			ctx.Response.Header.Set("Content-Type", "text/plain; charset=utf-8")
		},
	}
	go s.Serve(ln) //nolint:errcheck

	c := &Client{}
	url := "http://test:test@" + ln.Addr().String() + "/foo?bar=baz"

	n := testing.AllocsPerRun(100, func() {
		req := AcquireRequest()
		res := AcquireResponse()

		req.SetRequestURI(url)
		req.Header.Add("Foo", "bar")
		if err := c.Do(req, res); err != nil {
			t.Fatal(err)
		}

		ReleaseRequest(req)
		ReleaseResponse(res)
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationURI(t *testing.T) {
	uri := []byte("http://username:password@hello.%e4%b8%96%e7%95%8c.com/some/path?foo=bar#test")

	n := testing.AllocsPerRun(100, func() {
		u := AcquireURI()
		u.Parse(nil, uri) //nolint:errcheck
		ReleaseURI(u)
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationFS(t *testing.T) {
	// Create a simple test filesystem handler
	fs := &FS{
		Root:               ".",
		GenerateIndexPages: false,
		Compress:           false,
		AcceptByteRange:    false,
	}
	h := fs.NewRequestHandler()

	ctx := &RequestCtx{}

	n := testing.AllocsPerRun(100, func() {
		ctx.Request.Reset()
		ctx.Response.Reset()
		ctx.Request.SetRequestURI("/allocation_test.go")
		ctx.Request.Header.Set("Host", "localhost")

		h(ctx)
	})

	t.Logf("FS operations allocate %f times per request", n)

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationsHeaderScanner(t *testing.T) {
	body := []byte("Host: a.com\r\nCookie: foo=bar\r\nWithTabs: \t v1 \t\r\nWithTabs-Start: \t \t v1 \r\nWithTabs-End: v1 \t \t\t\t\r\nWithTabs-Multi-Line: \t v1 \t;\r\n \t v2 \t;\r\n\t v3\r\n\r\n")

	n := testing.AllocsPerRun(100, func() {
		var s headerScanner
		s.b = body

		for s.next() {
		}

		if s.err != nil {
			t.Fatal(s.err)
		}
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationReadBodyReusesBuffer(t *testing.T) {
	const size = 3 * unlimitedBodyStep
	body := createFixedBody(size)
	chunked := fmt.Appendf(nil, "%x\r\n%s\r\n0\r\n\r\n", size, body)
	dst := make([]byte, 0, size+len(strCRLF))
	var r bytes.Reader
	br := bufio.NewReader(&r)

	n := testing.AllocsPerRun(10, func() {
		r.Reset(body)
		br.Reset(&r)
		b, err := readBody(br, size, 0, dst)
		if err != nil || &b[0] != &dst[:1][0] {
			t.Fatalf("body not read into dst: %v", err)
		}

		r.Reset(chunked)
		br.Reset(&r)
		b, err = readBodyChunked(br, 0, dst)
		if err != nil || &b[0] != &dst[:1][0] {
			t.Fatalf("chunked body not read into dst: %v", err)
		}
	})

	if n != 0 {
		t.Fatalf("expected 0 allocations, got %f", n)
	}
}

func TestAllocationReadBodyAtOnce(t *testing.T) {
	for _, tc := range []struct{ size, limit int }{
		{size: 3 * unlimitedBodyStep, limit: 3 * unlimitedBodyStep},
		{size: unlimitedBodyStep, limit: 0},
	} {
		body := createFixedBody(tc.size)
		chunked := fmt.Appendf(nil, "%x\r\n%s\r\n0\r\n\r\n", tc.size, body)
		var r bytes.Reader
		br := bufio.NewReader(&r)

		n := testing.AllocsPerRun(10, func() {
			r.Reset(body)
			br.Reset(&r)
			if _, err := readBody(br, tc.size, tc.limit, nil); err != nil {
				t.Fatal(err)
			}

			r.Reset(chunked)
			br.Reset(&r)
			if _, err := readBodyChunked(br, tc.limit, nil); err != nil {
				t.Fatal(err)
			}
		})

		if n != 2 {
			t.Fatalf("expected 2 allocations for size %d and limit %d, got %f", tc.size, tc.limit, n)
		}
	}
}

func TestAllocationReadBodyHugeContentLength(t *testing.T) {
	var r strings.Reader
	br := bufio.NewReader(&r)

	n := testing.AllocsPerRun(10, func() {
		r.Reset("ok")
		br.Reset(&r)
		b, err := readBody(br, math.MaxInt, 0, nil)
		if err != io.ErrUnexpectedEOF || cap(b) != unlimitedBodyStep {
			t.Fatalf("unexpected cap %d, err %v", cap(b), err)
		}
	})

	if n != 1 {
		t.Fatalf("expected 1 allocation, got %f", n)
	}
}
