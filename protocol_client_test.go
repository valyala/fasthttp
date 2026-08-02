package fasthttp

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtocolClientConnPrepareResponseBody(t *testing.T) {
	var response Response
	response.AppendBodyString("prefix")
	(&ProtocolClientConn{}).PrepareResponseBody(&response, 1024)
	if got := string(response.Body()); got != "prefix" {
		t.Fatalf("body = %q, want prefix", got)
	}
	if available := cap(response.bodyBuffer().B) - len(response.bodyBuffer().B); available < 1024 {
		t.Fatalf("available body capacity = %d, want at least 1024", available)
	}
	response.ResetBody()
}

type testProtocolTransport struct {
	roundTripCalled         atomic.Bool
	protocolRoundTripCalled atomic.Bool
	closeIdleCalled         atomic.Bool
}

func (t *testProtocolTransport) RoundTrip(
	_ *HostClient,
	_ *Request,
	_ *Response,
) (bool, error) {
	t.roundTripCalled.Store(true)
	return false, nil
}

func (t *testProtocolTransport) RoundTripWithContext(
	_ *ProtocolClientContext,
	_ *HostClient,
	_ *Request,
	_ *Response,
) (bool, error) {
	t.protocolRoundTripCalled.Store(true)
	return false, nil
}

func (t *testProtocolTransport) CloseIdleConnections(_ *HostClient) {
	t.closeIdleCalled.Store(true)
}

func TestHostClientProtocolRoundTripper(t *testing.T) {
	transport := &testProtocolTransport{}
	hc := &HostClient{
		Addr: "example.com:80",
	}
	if err := hc.RegisterProtocolTransport(transport); err != nil {
		t.Fatalf("RegisterProtocolTransport() error: %v", err)
	}
	req := AcquireRequest()
	defer ReleaseRequest(req)
	resp := AcquireResponse()
	defer ReleaseResponse(resp)
	req.SetRequestURI("http://example.com/")

	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !transport.protocolRoundTripCalled.Load() {
		t.Fatal("Do() didn't call ProtocolRoundTripper")
	}
	if transport.roundTripCalled.Load() {
		t.Fatal("Do() called the legacy RoundTripper path")
	}

	hc.CloseIdleConnections()
	if !transport.closeIdleCalled.Load() {
		t.Fatal("CloseIdleConnections() didn't notify the protocol transport")
	}
}

func TestHostClientProtocolTransportRejectsCustomHTTP1Fallback(t *testing.T) {
	transport := &testProtocolTransport{}
	hc := &HostClient{
		Addr:      "example.com:80",
		Transport: transport,
	}
	if err := hc.RegisterProtocolTransport(transport); err == nil {
		t.Fatal("RegisterProtocolTransport() accepted a custom HTTP/1 fallback")
	}
}

func TestProtocolClientContextAcquireConnALPN(t *testing.T) {
	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatalf("generating certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	serverError := make(chan error, 1)
	go func() {
		serverTLS := tls.Server(serverConn, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"h2", "http/1.1"},
		})
		if err := serverTLS.Handshake(); err != nil {
			serverError <- err
			return
		}
		var one [1]byte
		_, err := serverTLS.Read(one[:])
		if err != nil && err != io.EOF {
			serverError <- err
			return
		}
		serverError <- nil
		_ = serverTLS.Close()
	}()

	hc := &HostClient{
		Addr:  "localhost:443",
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Test-only self-signed certificate.
		},
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	ctx := ProtocolClientContext{
		hostClient: hc,
		deadline:   time.Now().Add(time.Second),
	}
	conn, err := ctx.AcquireConn([]string{"h2", "http/1.1"})
	if err != nil {
		t.Fatalf("AcquireConn() error: %v", err)
	}
	if got := conn.NegotiatedProtocol(); got != "h2" {
		t.Fatalf("NegotiatedProtocol() = %q, want %q", got, "h2")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := <-serverError; err != nil {
		t.Fatalf("server handshake error: %v", err)
	}
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("ConnsCount() = %d, want 0", got)
	}
}

func TestProtocolClientConnRoundTripHTTP1(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		var req Request
		if err := req.Read(reader); err != nil {
			serverError <- err
			return
		}
		_, err := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		serverError <- err
	}()

	hc := &HostClient{
		Addr: "example.com:80",
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	ctx := ProtocolClientContext{hostClient: hc}
	conn, err := ctx.AcquireConn(nil)
	if err != nil {
		t.Fatalf("AcquireConn() error: %v", err)
	}

	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response
	retry, err := conn.RoundTripHTTP1(&req, &resp)
	if err != nil {
		t.Fatalf("RoundTripHTTP1() error: %v (retry=%v)", err, retry)
	}
	if got := string(resp.Body()); got != "ok" {
		t.Fatalf("response body = %q, want %q", got, "ok")
	}
	if err := <-serverError; err != nil {
		t.Fatalf("server error: %v", err)
	}

	hc.CloseIdleConnections()
	if got := hc.ConnsCount(); got != 0 {
		t.Fatalf("ConnsCount() after CloseIdleConnections = %d, want 0", got)
	}
}

func TestHostClientOpenStreamUnsupported(t *testing.T) {
	hc := &HostClient{Addr: "example.com:80"}
	var req Request
	req.SetRequestURI("http://example.com/")
	var resp Response

	stream, err := hc.OpenStream(&req, &resp)
	if stream != nil {
		t.Fatal("OpenStream() returned a stream for the default transport")
	}
	if err != ErrProtocolNotSupported {
		t.Fatalf("OpenStream() error = %v, want ErrProtocolNotSupported", err)
	}
}
