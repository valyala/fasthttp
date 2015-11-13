package fasthttp

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientHTTPSConcurrent(t *testing.T) {
	addrHTTP := "127.0.0.1:56793"
	sHTTP := startEchoServer(t, "tcp", addrHTTP)
	defer sHTTP.Stop()

	addrHTTPS := "127.0.0.1:56794"
	sHTTPS := startEchoServerTLS(t, "tcp", addrHTTPS)
	defer sHTTPS.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		addr := "http://" + addrHTTP
		if i&1 != 0 {
			addr = "https://" + addrHTTPS
		}
		go func() {
			defer wg.Done()
			testClientGet(t, &defaultClient, addr, 3000)
		}()
	}
	wg.Wait()
}

func TestClientManyServers(t *testing.T) {
	var addrs []string
	for i := 0; i < 10; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", 56904+i)
		s := startEchoServer(t, "tcp", addr)
		defer s.Stop()
		addrs = append(addrs, addr)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		addr := "http://" + addrs[i]
		go func() {
			defer wg.Done()
			testClientGet(t, &defaultClient, addr, 3000)
		}()
	}
	wg.Wait()
}

func TestClientGet(t *testing.T) {
	addr := "127.0.0.1:56789"
	s := startEchoServer(t, "tcp", addr)
	defer s.Stop()

	addr = "http://" + addr
	testClientGet(t, &defaultClient, addr, 100)
}

func TestClientGetConcurrent(t *testing.T) {
	addr := "127.0.0.1:56780"
	s := startEchoServer(t, "tcp", addr)
	defer s.Stop()

	addr = "http://" + addr
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientGet(t, &defaultClient, addr, 3000)
		}()
	}
	wg.Wait()
}

func TestHostClientGet(t *testing.T) {
	addr := "./TestHostClientGet.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	testHostClientGet(t, c, 100)
}

func TestHostClientGetConcurrent(t *testing.T) {
	addr := "./TestHostClientGetConcurrent.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

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

func testClientGet(t *testing.T, c clientGetter, addr string, n int) {
	var buf []byte
	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("%s/foo/%d?bar=baz", addr, i)
		statusCode, body, err := c.Get(buf, uri)
		buf = body
		if err != nil {
			t.Fatalf("unexpected error when doing http request: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
		}
		resultURI := string(body)
		if strings.HasPrefix(uri, "https") {
			resultURI = uri[:5] + resultURI[4:]
		}
		if resultURI != uri {
			t.Fatalf("unexpected uri %q. Expecting %q", resultURI, uri)
		}
	}
}

func testHostClientGet(t *testing.T, c *HostClient, n int) {
	testClientGet(t, c, "http://google.com", n)
}

type clientGetter interface {
	Get(dst []byte, uri string) (int, []byte, error)
}

func createEchoClient(t *testing.T, network, addr string) *HostClient {
	return &HostClient{
		Addr: addr,
		Dial: func(addr string) (net.Conn, error) {
			return net.Dial(network, addr)
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

func startEchoServerTLS(t *testing.T, network, addr string) *testEchoServer {
	return startEchoServerExt(t, network, addr, true)
}

func startEchoServer(t *testing.T, network, addr string) *testEchoServer {
	return startEchoServerExt(t, network, addr, false)
}

func startEchoServerExt(t *testing.T, network, addr string, isTLS bool) *testEchoServer {
	if network == "unix" {
		os.Remove(addr)
	}
	var ln net.Listener
	var err error
	if isTLS {
		certFile := "./ssl-cert-snakeoil.pem"
		keyFile := "./ssl-cert-snakeoil.key"
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatalf("Cannot load TLS certificate: %s", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		ln, err = tls.Listen(network, addr, tlsConfig)
	} else {
		ln, err = net.Listen(network, addr)
	}
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
