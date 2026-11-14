//go:build !race

package fasthttp

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestAllocationClient_FreshDial(t *testing.T) {
	rw := &readWriter{}
	rw.r.Grow(1024)
	rw.w.Grow(1024)

	c := &HostClient{
		Addr: "example.com:80",
		DialTimeout: func(addr string, timeout time.Duration) (net.Conn, error) {
			rw.r.Reset()
			rw.w.Reset()
			rw.r.WriteString("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok")
			return rw, nil
		},
	}

	req := AcquireRequest()
	res := AcquireResponse()
	defer ReleaseRequest(req)
	defer ReleaseResponse(res)

	req.SetRequestURI("http://example.com/")
	req.Header.SetMethod(MethodGet)

	n := testing.AllocsPerRun(100, func() {
		req.Reset()
		res.Reset()
		req.SetRequestURI("http://example.com/")
		req.Header.SetMethod(MethodGet)
		if err := c.DoTimeout(req, res, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
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

func TestAllocationFSTrimmedSlash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"docs/index.html": "docs", "file.txt": "file"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file := []byte("file.txt")
	h := (&FS{
		FS:             os.DirFS(dir),
		AllowEmptyRoot: true,
		IndexNames:     []string{"index.html"},
		PathRewrite: func(ctx *RequestCtx) []byte {
			if string(ctx.Path()) == "/asset/" {
				return file // an alias: the client's slash is not in the rewritten path
			}
			return bytes.Trim(ctx.Path(), "/")
		},
	}).NewRequestHandler()

	ctx := &RequestCtx{}
	for _, uri := range []string{"/docs/", "/asset/"} {
		get := func() {
			ctx.Request.Reset()
			ctx.Response.Reset()
			ctx.Request.SetRequestURI(uri)
			ctx.Request.Header.Set("Host", "localhost")
			h(ctx)
		}
		get() // warm the cache
		if ctx.Response.StatusCode() != StatusOK {
			t.Fatalf("%s: unexpected status code %d", uri, ctx.Response.StatusCode())
		}
		if n := testing.AllocsPerRun(100, get); n != 0 {
			t.Fatalf("%s: expected 0 allocations, got %f", uri, n)
		}
	}
}
