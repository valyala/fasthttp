package fasthttputil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
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
				t.Errorf("unexpected error: %s", err)
			}
			defer conn.Close()
			req := fmt.Sprintf("request_%d", n)
			nn, err := conn.Write([]byte(req))
			if err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			if nn != len(req) {
				t.Errorf("unexpected number of bytes written: %d. Expecting %d", nn, len(req))
			}
			buf := make([]byte, 30)
			nn, err = conn.Read(buf)
			if err != nil {
				t.Errorf("unexpected error: %s", err)
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
				t.Errorf("unexpected error: %s", err)
			}
			buf = buf[:n]
			if !bytes.HasPrefix(buf, []byte("request_")) {
				t.Errorf("unexpected request prefix %q. Expecting %q", buf, "request_")
			}
			resp := fmt.Sprintf("response_%s", buf[len("request_"):])
			n, err = conn.Write([]byte(resp))
			if err != nil {
				t.Errorf("unexpected error: %s", err)
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
		t.Fatalf("unexpected error: %s", err)
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
		s.t.Fatalf("unexpected error: %s", err)
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
			t.Errorf("unexpected error: %s", err)
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
		t.Fatalf("unexpected error: %s", err)
	}
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	s := string(b)
	if string(b) != content {
		t.Fatalf("unexpected response %s, expecting %s", s, content)
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
