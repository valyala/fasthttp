package proxy

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestProxyClientMultipleAddrs(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Write(ctx.Host())
			ctx.SetConnectionClose()
		},
	}
	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	dialsCount := make(map[string]int)
	c := &ProxyClient{
		Addr: "foo,bar,baz",
		Dial: func(addr string) (net.Conn, error) {
			dialsCount[addr]++
			return ln.Dial()
		},
	}

	for i := 0; i < 9; i++ {
		req := AcquireRequest()
		req.SetRequestURI("http://foobar/baz/aaa?bbb=ddd")
		resp := AcquireResponse()

		// The following command does the same thing as HostClient.Do().
		retry, s, err := c.SendRequest(req)
		if err == nil {
			retry, err = c.ReadResponseHeader(s, req, resp)
			if err == nil {
				retry, err = c.ReadResponseBody(s, req, resp)
			}
		}
		if err != nil && retry && isIdempotent(req) {
			_, s, err = c.SendRequest(req)
			if err == nil {
				_, err = c.ReadResponseHeader(s, req, resp)
				if err == nil {
					_, err = c.ReadResponseBody(s, req, resp)
				}
			}
		}
		if err == io.EOF {
			err = ErrConnectionClosed
		}

		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		statusCode := resp.StatusCode()
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code %d. Expecting %d", statusCode, StatusOK)
		}
		body := resp.Body()
		if string(body) != "foobar" {
			t.Fatalf("unexpected body %q. Expecting %q", body, "foobar")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	if len(dialsCount) != 3 {
		t.Fatalf("unexpected dialsCount size %d. Expecting 3", len(dialsCount))
	}
	for _, k := range []string{"foo", "bar", "baz"} {
		if dialsCount[k] != 3 {
			t.Fatalf("unexpected dialsCount for %q. Expecting 3", k)
		}
	}
}
