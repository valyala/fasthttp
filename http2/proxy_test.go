package http2

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

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
