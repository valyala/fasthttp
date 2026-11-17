package http2

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A response an HTTP/2 client filled in is relayed to an HTTP/1 client with
// an HTTP/1.1 status line.
func TestProxyHTTP2ResponseToHTTP1Client(t *testing.T) {
	upstream := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.Response.Header.AddTrailer("X-T")
		ctx.Response.Header.Set("X-T", "v")
		ctx.SetBodyString("hello")
	}}, ServerConfig{})
	h2 := &fasthttp.HostClient{Addr: upstream.listener.Addr().String()}
	if err := ConfigureHostClient(h2, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(h2.CloseIdleConnections)
	front := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.SetRequestURI("http://" + upstream.listener.Addr().String() + "/")
		if err := h2.Do(req, &ctx.Response); err != nil {
			ctx.Error(err.Error(), fasthttp.StatusBadGateway)
		}
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- front.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = front.ShutdownWithContext(ctx)
		<-done
	})
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: front\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(bufio.NewReader(client))
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)
	if !strings.HasPrefix(wire, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("status line = %q", wire)
	}
	if !strings.Contains(wire, "Transfer-Encoding: chunked") || !strings.Contains(wire, "\r\nX-T: v\r\n") || !strings.Contains(wire, "hello") {
		t.Fatalf("relayed response = %q", wire)
	}
}

// A request an HTTP/2 server filled in is forwarded to an HTTP/1 upstream with
// an HTTP/1.1 request line.
func TestProxyHTTP2RequestToHTTP1Upstream(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	requestLines := make(chan string, 2)
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				requestLines <- strings.TrimRight(line, "\r\n")
				var req fasthttp.Request
				_ = req.Header.Read(reader) // the rest of the header
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
		}
	}()
	h1 := &fasthttp.HostClient{Addr: upstream.Addr().String()}
	t.Cleanup(h1.CloseIdleConnections)
	s := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if err := h1.Do(&ctx.Request, &ctx.Response); err != nil {
			ctx.Error(err.Error(), fasthttp.StatusBadGateway)
		}
	}}, ServerConfig{})
	resp, err := s.client.Get(s.URL("/x"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	select {
	case line := <-requestLines:
		if line != "GET /x HTTP/1.1" {
			t.Fatalf("upstream saw request line %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream saw no request")
	}
}
