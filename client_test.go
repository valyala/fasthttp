package fasthttp

import (
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func TestHostClientGet(t *testing.T) {
	addr := "./TestHostClientGet.unix"
	s := startEchoServer(t, addr)
	defer s.Stop()
	c := createUnixClient(t, addr)

	testHostClientGet(t, c, 100)
}

func TestHostClientGetConcurrent(t *testing.T) {
	addr := "./TestHostClientGetConcurrent.unix"
	s := startEchoServer(t, addr)
	defer s.Stop()
	c := createUnixClient(t, addr)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHostClientGet(t, c, 3000)
		}()
	}
	wg.Wait()
}

func testHostClientGet(t *testing.T, c *HostClient, n int) {
	var buf []byte
	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("http://google.com/foo/%d?bar=baz", i)
		statusCode, body, err := c.Get(buf, uri)
		buf = body
		if err != nil {
			t.Fatalf("unexpected error when doing http request: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
		}
		if string(body) != uri {
			t.Fatalf("unexpected uri %q. Expecting %q", body, uri)
		}
	}
}

func createUnixClient(t *testing.T, addr string) *HostClient {
	return &HostClient{
		Addr: addr,
		Dial: func(addr string) (net.Conn, error) {
			return net.Dial("unix", addr)
		},
	}
}

type testEchoServer struct {
	s  *Server
	ln net.Listener
	ch chan struct{}
	t  *testing.T
}

func (s *testEchoServer) Stop() {
	s.ln.Close()
	select {
	case <-s.ch:
	case <-time.After(time.Second):
		s.t.Fatalf("timeout when waiting for server close")
	}
}

func startEchoServer(t *testing.T, addr string) *testEchoServer {
	os.Remove(addr)
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("cannot listen %q: %s", addr, err)
	}
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Request.ParseURI()
			ctx.Success("text/plain", ctx.Request.URI.URI)
		},
	}
	ch := make(chan struct{})
	go func() {
		err := s.Serve(ln)
		if err != nil {
			t.Fatalf("unexpected error returned from Serve(): %s", err)
		}
		close(ch)
	}()
	return &testEchoServer{
		s:  s,
		ln: ln,
		ch: ch,
		t:  t,
	}
}
