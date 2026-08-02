package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

type testServer struct {
	server    *fasthttp.Server
	listener  net.Listener
	transport *xhttp2.Transport
	client    *stdhttp.Client
	serveDone chan error
}

func newTestServer(t *testing.T, server *fasthttp.Server, config ServerConfig) *testServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	if err := ConfigureServer(server, config); err != nil {
		listener.Close()
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	transport := &xhttp2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	result := &testServer{
		server:    server,
		listener:  listener,
		transport: transport,
		client:    &stdhttp.Client{Transport: transport},
		serveDone: make(chan error, 1),
	}
	go func() {
		result.serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		select {
		case err := <-result.serveDone:
			if err != nil {
				t.Errorf("Serve() error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Serve() didn't return")
		}
	})
	return result
}

func (s *testServer) URL(path string) string {
	return "http://" + s.listener.Addr().String() + path
}

func TestServerRequestResponse(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if got := string(ctx.Request.Header.Protocol()); got != "HTTP/2" {
				t.Errorf("request protocol = %q, want HTTP/2", got)
			}
			ctx.Response.Header.Set("X-Method", string(ctx.Method()))
			ctx.SetBody(ctx.Request.Body())
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	body := bytes.Repeat([]byte("request-body-"), 10_000)
	req, err := stdhttp.NewRequest(stdhttp.MethodPost, testServer.URL("/echo?q=1"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error: %v", err)
	}
	resp, err := testServer.client.Do(req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	defer resp.Body.Close()
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("response body length = %d, want %d", len(gotBody), len(body))
	}
	if got := resp.Header.Get("X-Method"); got != stdhttp.MethodPost {
		t.Fatalf("X-Method = %q, want POST", got)
	}
}

func TestServerMultiplexesHandlers(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/slow" {
				close(slowStarted)
				<-releaseSlow
			}
			ctx.SetBodyString(string(ctx.Path()))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	var slowResponse *stdhttp.Response
	var slowErr error
	var wait sync.WaitGroup
	wait.Go(func() {
		slowResponse, slowErr = testServer.client.Get(testServer.URL("/slow"))
	})
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow handler didn't start")
	}

	fastDone := make(chan struct{})
	var fastResponse *stdhttp.Response
	var fastErr error
	go func() {
		fastResponse, fastErr = testServer.client.Get(testServer.URL("/fast"))
		close(fastDone)
	}()
	select {
	case <-fastDone:
	case <-time.After(time.Second):
		t.Fatal("fast request was blocked by slow handler")
	}
	if fastErr != nil {
		t.Fatalf("fast request error: %v", fastErr)
	}
	fastBody, err := io.ReadAll(fastResponse.Body)
	fastResponse.Body.Close()
	if err != nil || string(fastBody) != "/fast" {
		t.Fatalf("fast response = %q, %v", fastBody, err)
	}

	close(releaseSlow)
	wait.Wait()
	if slowErr != nil {
		t.Fatalf("slow request error: %v", slowErr)
	}
	slowResponse.Body.Close()
}

func TestServerStreamsRequestAndResponse(t *testing.T) {
	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			body, err := io.ReadAll(ctx.RequestBodyStream())
			if err != nil {
				t.Errorf("reading request stream: %v", err)
				ctx.SetStatusCode(fasthttp.StatusInternalServerError)
				return
			}
			ctx.Response.SetBodyStream(bytes.NewReader(body), len(body))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	body := bytes.Repeat([]byte("streaming-body"), 20_000)
	resp, err := testServer.client.Post(testServer.URL("/stream"), "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post() error: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response stream: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("streamed response length = %d, want %d", len(got), len(body))
	}
}

func TestServerEarlyHints(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("Link", "</style.css>; rel=preload")
			if err := ctx.EarlyHints(); err != nil {
				t.Errorf("EarlyHints() error: %v", err)
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	gotStatus := make(chan int, 1)
	req, err := stdhttp.NewRequest(stdhttp.MethodGet, testServer.URL("/"), nil)
	if err != nil {
		t.Fatalf("NewRequest() error: %v", err)
	}
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			gotStatus <- code
			return nil
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := testServer.client.Do(req)
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()
	select {
	case status := <-gotStatus:
		if status != fasthttp.StatusEarlyHints {
			t.Fatalf("informational status = %d, want 103", status)
		}
	case <-time.After(time.Second):
		t.Fatal("didn't receive early hints")
	}
}
