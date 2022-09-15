//go:build !race
// +build !race

package fasthttp

import (
	"net"
	"testing"
)

func TestAllocationServeConn(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
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
		},
	}
	go s.Serve(ln) //nolint:errcheck

	c := &Client{}
	url := "http://test:test@" + ln.Addr().String() + "/foo?bar=baz"

	n := testing.AllocsPerRun(100, func() {
		req := AcquireRequest()
		res := AcquireResponse()

		req.SetRequestURI(url)
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
