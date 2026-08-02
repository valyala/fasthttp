package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

func newPriorKnowledgeHostClient(t *testing.T, addr string) *fasthttp.HostClient {
	t.Helper()
	hc := &fasthttp.HostClient{Addr: addr}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	return hc
}

func TestClientRequestResponse(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("X-Protocol", string(ctx.Request.Header.Protocol()))
			ctx.SetBody(ctx.Request.Body())
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())

	body := bytes.Repeat([]byte("client-request-body"), 10_000)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI(testServer.URL("/echo"))
	req.SetBody(body)
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := string(resp.Header.Protocol()); got != "HTTP/2" {
		t.Fatalf("response protocol = %q, want HTTP/2", got)
	}
	if got := string(resp.Header.Peek("X-Protocol")); got != "HTTP/2" {
		t.Fatalf("X-Protocol = %q, want HTTP/2", got)
	}
	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("response body length = %d, want %d", len(resp.Body()), len(body))
	}
}

func TestClientMultiplexesRequests(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			time.Sleep(time.Duration(ctx.QueryArgs().GetUintOrZero("delay")) * time.Millisecond)
			ctx.SetBody(ctx.Path())
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = time.Second

	const requests = 50
	errorsByRequest := make(chan error, requests)
	var wait sync.WaitGroup
	for i := range requests {
		wait.Go(func() {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI(fmt.Sprintf("http://%s/request-%d?delay=%d", testServer.listener.Addr(), i, i%5))
			if err := hc.Do(req, resp); err != nil {
				errorsByRequest <- err
				return
			}
			want := fmt.Sprintf("/request-%d", i)
			if string(resp.Body()) != want {
				errorsByRequest <- fmt.Errorf("body = %q, want %q", resp.Body(), want)
			}
		})
	}
	wait.Wait()
	close(errorsByRequest)
	for err := range errorsByRequest {
		t.Error(err)
	}
	if got := hc.ConnsCount(); got != 1 {
		t.Fatalf("physical connection count = %d, want 1", got)
	}
}

func TestClientStreamingResponse(t *testing.T) {
	body := bytes.Repeat([]byte("streaming-response"), 20_000)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.SetBodyStream(bytes.NewReader(body), len(body))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.StreamResponseBody = true

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/stream"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	got, err := io.ReadAll(resp.BodyStream())
	if err != nil {
		t.Fatalf("reading response stream: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("response body length = %d, want %d", len(got), len(body))
	}
}

func TestClientInteroperatesWithGoHTTP2Server(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	server := &xhttp2.Server{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.ServeConn(conn, &xhttp2.ServeConnOpts{Handler: stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
				w.Header().Set("Trailer", "X-Checksum")
				_, _ = io.Copy(w, r.Body)
				w.Header().Set("X-Checksum", "ok")
			})})
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	hc := newPriorKnowledgeHostClient(t, listener.Addr().String())

	body := bytes.Repeat([]byte("go-server"), 20_000)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI("http://" + listener.Addr().String() + "/")
	req.SetBody(body)
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("response body length = %d, want %d", len(resp.Body()), len(body))
	}
	if got := string(resp.Header.Peek("X-Checksum")); got != "ok" {
		t.Fatalf("response trailer = %q, want ok", got)
	}
}

func TestExtendedConnectStream(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if got := string(ctx.Request.Header.ConnectProtocol()); got != "websocket" {
				t.Errorf("connect protocol = %q, want websocket", got)
				ctx.SetStatusCode(fasthttp.StatusBadRequest)
				return
			}
			ctx.SetStatusCode(fasthttp.StatusOK)
			if err := ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				_, _ = io.Copy(conn, conn)
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{
		Mode:                  PriorKnowledge,
		EnableExtendedConnect: true,
	}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodConnect)
	req.Header.SetConnectProtocol("websocket")
	req.SetRequestURI(testServer.URL("/chat"))
	conn, err := hc.OpenStream(req, resp)
	if err != nil {
		t.Fatalf("OpenStream() error: %v", err)
	}
	defer conn.Close()
	message := bytes.Repeat([]byte("websocket-payload"), 10_000)
	if _, err := conn.Write(message); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if !bytes.Equal(got, message) {
		t.Fatalf("echo length = %d, want %d", len(got), len(message))
	}
}

func TestClientTLSALPN(t *testing.T) {
	certData, keyData, err := fasthttp.GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("GenerateTestCertificate() error: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("X509KeyPair() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	server := &fasthttp.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBody(ctx.Request.Header.Protocol())
		},
	}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(tls.NewListener(listener, server.TLSConfig.Clone()))
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		<-done
	})

	hc := &fasthttp.HostClient{
		Addr:  listener.Addr().String(),
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("https://" + listener.Addr().String() + "/")
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := string(resp.Body()); got != "HTTP/2" {
		t.Fatalf("body = %q, want HTTP/2", got)
	}
}

func TestClientTLSHTTP1FallbackReusesNegotiatedConnection(t *testing.T) {
	certData, keyData, err := fasthttp.GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("GenerateTestCertificate() error: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("X509KeyPair() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	server := &fasthttp.Server{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"http/1.1"},
		},
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBody(ctx.Request.Header.Protocol())
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(tls.NewListener(listener, server.TLSConfig.Clone()))
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		<-done
	})

	var dials atomic.Int32
	hc := &fasthttp.HostClient{
		Addr:  listener.Addr().String(),
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
		},
		DialTimeout: func(addr string, timeout time.Duration) (net.Conn, error) {
			dials.Add(1)
			return net.DialTimeout("tcp", addr, timeout)
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	for range 2 {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.SetRequestURI("https://" + listener.Addr().String() + "/")
		err := hc.Do(req, resp)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		if got := string(resp.Body()); got != "HTTP/1.1" {
			t.Fatalf("body = %q, want HTTP/1.1", got)
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
}

func TestClientRequireHTTP2RejectsHTTP1ALPN(t *testing.T) {
	certData, keyData, err := fasthttp.GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("GenerateTestCertificate() error: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("X509KeyPair() error: %v", err)
	}
	serverConn, clientConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		conn := tls.Server(serverConn, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"http/1.1"},
		})
		serverDone <- conn.Handshake()
		_ = conn.Close()
	}()
	hc := &fasthttp.HostClient{
		Addr:  "localhost:443",
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
		},
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: RequireHTTP2}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("https://localhost/")
	if err := hc.Do(req, resp); !errors.Is(err, errHTTP2Required) {
		t.Fatalf("Do() error = %v, want %v", err, errHTTP2Required)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server handshake error: %v", err)
	}
}

type testPushHandler struct {
	t            *testing.T
	acceptedPath chan string
	responseBody chan string
}

func (h *testPushHandler) Accept(_, promised *fasthttp.Request) bool {
	h.acceptedPath <- string(promised.URI().Path())
	return true
}

func (h *testPushHandler) Handle(_ *fasthttp.Request, response *fasthttp.Response) {
	h.responseBody <- string(response.Body())
}

func TestServerPush(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/pushed" {
				ctx.SetBodyString("pushed response")
				return
			}
			if err := ctx.Push("/pushed", nil); err != nil {
				t.Errorf("Push() error: %v", err)
			}
			ctx.SetBodyString("parent response")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnablePush: true})
	handler := &testPushHandler{
		t:            t,
		acceptedPath: make(chan string, 1),
		responseBody: make(chan string, 1),
	}
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{
		Mode:        PriorKnowledge,
		PushHandler: handler,
	}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := string(resp.Body()); got != "parent response" {
		t.Fatalf("parent body = %q", got)
	}
	select {
	case path := <-handler.acceptedPath:
		if path != "/pushed" {
			t.Fatalf("promised path = %q, want /pushed", path)
		}
	case <-time.After(time.Second):
		t.Fatal("push wasn't offered")
	}
	select {
	case body := <-handler.responseBody:
		if body != "pushed response" {
			t.Fatalf("pushed body = %q, want pushed response", body)
		}
	case <-time.After(time.Second):
		t.Fatal("push response wasn't handled")
	}
}
