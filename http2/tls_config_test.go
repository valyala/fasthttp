package http2

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A config GetConfigForClient returns negotiates h2 like Server.TLSConfig.
func TestServerTLSGetConfigForClientNegotiatesHTTP2(t *testing.T) {
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
	perClient := &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"http/1.1"}}
	server := &fasthttp.Server{
		TLSConfig: &tls.Config{
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return perClient, nil },
		},
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBody(ctx.Request.Header.Protocol()) },
	}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(tls.NewListener(listener, server.TLSConfig.Clone())) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		<-done
	})
	hc := &fasthttp.HostClient{
		Addr:      listener.Addr().String(),
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: RequireHTTP2}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
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
	if !slicesEqual(perClient.NextProtos, []string{"http/1.1"}) {
		t.Fatalf("the returned config was mutated: %v", perClient.NextProtos)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
