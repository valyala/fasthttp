package fasthttputil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestInmemoryListener(t *testing.T) {
	ln := NewInmemoryListener()

	ch := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			conn, err := ln.Dial()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			defer conn.Close()
			req := fmt.Sprintf("request_%d", n)
			nn, err := conn.Write([]byte(req))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if nn != len(req) {
				t.Errorf("unexpected number of bytes written: %d. Expecting %d", nn, len(req))
			}
			buf := make([]byte, 30)
			nn, err = conn.Read(buf)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			buf = buf[:nn]
			resp := fmt.Sprintf("response_%d", n)
			if nn != len(resp) {
				t.Errorf("unexpected number of bytes read: %d. Expecting %d", nn, len(resp))
			}
			if string(buf) != resp {
				t.Errorf("unexpected response %q. Expecting %q", buf, resp)
			}
			ch <- struct{}{}
		}(i)
	}

	serverCh := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				close(serverCh)
				return
			}
			defer conn.Close()
			buf := make([]byte, 30)
			n, err := conn.Read(buf)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			buf = buf[:n]
			if !bytes.HasPrefix(buf, []byte("request_")) {
				t.Errorf("unexpected request prefix %q. Expecting %q", buf, "request_")
			}
			resp := fmt.Sprintf("response_%s", buf[len("request_"):])
			n, err = conn.Write([]byte(resp))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if n != len(resp) {
				t.Errorf("unexpected number of bytes written: %d. Expecting %d", n, len(resp))
			}
		}
	}()

	for i := 0; i < 10; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

// echoServerHandler implements http.Handler.
type echoServerHandler struct {
	t *testing.T
}

func (s *echoServerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	time.Sleep(time.Millisecond * 100)
	if _, err := io.Copy(w, r.Body); err != nil {
		s.t.Fatalf("unexpected error: %v", err)
	}
}

func testInmemoryListenerHTTP(t *testing.T, f func(t *testing.T, client *http.Client)) {
	ln := NewInmemoryListener()
	defer ln.Close()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return ln.Dial()
			},
		},
		Timeout: time.Second,
	}

	server := &http.Server{
		Handler: &echoServerHandler{t},
	}

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	f(t, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
	defer cancel()
	server.Shutdown(ctx) //nolint:errcheck
}

func testInmemoryListenerHTTPSingle(t *testing.T, client *http.Client, content string) {
	res, err := client.Post("http://...", "text/plain", bytes.NewBufferString(content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(b)
	if string(b) != content {
		t.Fatalf("unexpected response %q, expecting %q", s, content)
	}
}

func TestInmemoryListenerHTTPSingle(t *testing.T) {
	testInmemoryListenerHTTP(t, func(t *testing.T, client *http.Client) {
		testInmemoryListenerHTTPSingle(t, client, "request")
	})
}

func TestInmemoryListenerHTTPSerial(t *testing.T) {
	testInmemoryListenerHTTP(t, func(t *testing.T, client *http.Client) {
		for i := 0; i < 10; i++ {
			testInmemoryListenerHTTPSingle(t, client, fmt.Sprintf("request_%d", i))
		}
	})
}

func TestInmemoryListenerHTTPConcurrent(t *testing.T) {
	testInmemoryListenerHTTP(t, func(t *testing.T, client *http.Client) {
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				testInmemoryListenerHTTPSingle(t, client, fmt.Sprintf("request_%d", i))
			}(i)
		}
		wg.Wait()
	})
}

func acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			panic(err)
		}

		conn.Close()
	}
}

func TestInmemoryListenerAddrDefault(t *testing.T) {
	ln := NewInmemoryListener()

	verifyAddr(t, ln.Addr(), inmemoryAddr(0))

	go func() {
		c, err := ln.Dial()
		if err != nil {
			panic(err)
		}

		c.Close()
	}()

	lc, err := ln.Accept()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verifyAddr(t, lc.LocalAddr(), inmemoryAddr(0))
	verifyAddr(t, lc.RemoteAddr(), pipeAddr(0))

	go acceptLoop(ln)

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verifyAddr(t, c.LocalAddr(), pipeAddr(0))
	verifyAddr(t, c.RemoteAddr(), inmemoryAddr(0))
}

func verifyAddr(t *testing.T, got, expected net.Addr) {
	if got != expected {
		t.Fatalf("unexpected addr: %v. Expecting %v", got, expected)
	}
}

func TestInmemoryListenerAddrCustom(t *testing.T) {
	ln := NewInmemoryListener()

	listenerAddr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}

	ln.SetLocalAddr(listenerAddr)

	verifyAddr(t, ln.Addr(), listenerAddr)

	go func() {
		c, err := ln.Dial()
		if err != nil {
			panic(err)
		}

		c.Close()
	}()

	lc, err := ln.Accept()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verifyAddr(t, lc.LocalAddr(), listenerAddr)
	verifyAddr(t, lc.RemoteAddr(), pipeAddr(0))

	go acceptLoop(ln)

	clientAddr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 65432}

	c, err := ln.DialWithLocalAddr(clientAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verifyAddr(t, c.LocalAddr(), clientAddr)
	verifyAddr(t, c.RemoteAddr(), listenerAddr)
}
