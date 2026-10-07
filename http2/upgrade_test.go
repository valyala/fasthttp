package http2

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func upgradeTestServer(t *testing.T) net.Listener {
	return upgradeTestServerConfig(t, &ServerConfig{})
}

func upgradeTestServerConfig(t *testing.T, config *ServerConfig) net.Listener {
	t.Helper()
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			fmt.Fprintf(ctx, "%s %s %s body=%s",
				ctx.Method(), ctx.Path(), ctx.Request.Header.Protocol(), ctx.Request.Body())
		},
	}
	if err := ConfigureServer(server, *config); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { listener.Close() })
	return listener
}

func TestUpgradeH2CPrefaceUsesIdleTimeout(t *testing.T) {
	listener := upgradeTestServerConfig(t, &ServerConfig{IdleTimeout: 50 * time.Millisecond})
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: %s\r\n\r\n", h2SettingsHeader)
	reader := bufio.NewReader(conn)
	if response := readHTTP1Response(t, reader); !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade response = %q, want 101", response)
	}
	framer := xhttp2.NewFramer(nil, reader)
	for {
		if _, err := framer.ReadFrame(); err != nil {
			if isTimeout(err) {
				t.Fatal("upgraded connection survived past IdleTimeout without a client preface")
			}
			return
		}
	}
}

func TestUpgradeH2CShutdownDoesNotWaitForPrefaceGracePeriod(t *testing.T) {
	server := &fasthttp.Server{}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer listener.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	defer func() {
		if err := <-serveDone; err != nil {
			t.Errorf("Serve() error: %v", err)
		}
	}()
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: %s\r\n\r\n", h2SettingsHeader); err != nil {
		t.Fatalf("writing upgrade request: %v", err)
	}
	reader := bufio.NewReader(conn)
	if response := readHTTP1Response(t, reader); !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade response = %q, want 101", response)
	}
	start := time.Now()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown() }()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		_ = listener.Close()
		t.Fatal("Shutdown() waited for the fixed h2c preface grace period")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown() took %v, want at most 1s", elapsed)
	}
}

func readHTTP1Response(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var response strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading upgrade response: %v", err)
		}
		response.WriteString(line)
		if line == "\r\n" {
			return response.String()
		}
	}
}

// h2SettingsHeader is SETTINGS_MAX_CONCURRENT_STREAMS=100.
var h2SettingsHeader = base64.RawURLEncoding.EncodeToString([]byte{0, 3, 0, 0, 0, 100})

func TestUpgradeH2C(t *testing.T) {
	listener := upgradeTestServer(t)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "POST /up HTTP/1.1\r\nHost: example.com\r\nContent-Length: 3\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: %s\r\n\r\nabc", h2SettingsHeader)

	reader := bufio.NewReader(conn)
	response := readHTTP1Response(t, reader)
	if !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade response = %q, want 101", response)
	}

	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatalf("writing preface: %v", err)
	}
	framer := xhttp2.NewFramer(conn, reader)
	framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("writing settings: %v", err)
	}

	var status string
	var body bytes.Buffer
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("reading frame (status=%q body=%q): %v", status, body.String(), err)
		}
		switch frame := frame.(type) {
		case *xhttp2.SettingsFrame:
			if !frame.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					t.Fatalf("acking settings: %v", err)
				}
			}
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID != 1 {
				t.Fatalf("response arrived on stream %d, want 1", frame.StreamID)
			}
			status = frame.PseudoValue("status")
		case *xhttp2.DataFrame:
			if frame.StreamID != 1 {
				t.Fatalf("data arrived on stream %d, want 1", frame.StreamID)
			}
			body.Write(frame.Data())
			if frame.StreamEnded() {
				if status != "200" {
					t.Fatalf("status = %q, want 200", status)
				}
				if got, want := body.String(), "POST /up HTTP/2 body=abc"; got != want {
					t.Fatalf("body = %q, want %q", got, want)
				}
				return
			}
		}
	}
}

func TestUpgradeH2CInvalidSettingsServesHTTP1(t *testing.T) {
	listener := upgradeTestServer(t)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET /plain HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: %s\r\n\r\n", "!!!not-base64!!!")

	reader := bufio.NewReader(conn)
	response := readHTTP1Response(t, reader)
	if !strings.HasPrefix(response, "HTTP/1.1 200 ") {
		t.Fatalf("response = %q, want plain HTTP/1 200", response)
	}
}

func TestUpgradeH2CIgnoredForHTTP10(t *testing.T) {
	listener := upgradeTestServer(t)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// keep-alive suppresses the Connection: close fasthttp otherwise implies
	// for HTTP/1.0, which would hide the Upgrade token on its own.
	fmt.Fprintf(conn, "GET /plain HTTP/1.0\r\nHost: example.com\r\n"+
		"Connection: keep-alive, Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
		"HTTP2-Settings: %s\r\n\r\n", h2SettingsHeader)

	reader := bufio.NewReader(conn)
	response := readHTTP1Response(t, reader)
	if !strings.HasPrefix(response, "HTTP/1.1 200 ") {
		t.Fatalf("response = %q, want plain HTTP/1 200", response)
	}
}

func TestUpgradeSettingsValidation(t *testing.T) {
	build := func(configure func(*fasthttp.Request)) *fasthttp.Request {
		var request fasthttp.Request
		request.Header.Set(fasthttp.HeaderConnection, "Upgrade, HTTP2-Settings")
		request.Header.Set("HTTP2-Settings", h2SettingsHeader)
		configure(&request)
		return &request
	}
	valid := build(func(*fasthttp.Request) {})
	if settings, ok := upgradeSettings(valid); !ok || len(settings) != 1 ||
		settings[0].ID != xhttp2.SettingMaxConcurrentStreams || settings[0].Val != 100 {
		t.Fatalf("upgradeSettings() = %v, %v", settings, ok)
	}
	for name, request := range map[string]*fasthttp.Request{
		"connection misses token": build(func(r *fasthttp.Request) {
			r.Header.Set(fasthttp.HeaderConnection, "Upgrade")
		}),
		"duplicated header": build(func(r *fasthttp.Request) {
			r.Header.Add("HTTP2-Settings", h2SettingsHeader)
		}),
		"truncated payload": build(func(r *fasthttp.Request) {
			r.Header.Set("HTTP2-Settings", base64.RawURLEncoding.EncodeToString([]byte{0, 3, 0}))
		}),
	} {
		if _, ok := upgradeSettings(request); ok {
			t.Fatalf("upgradeSettings() accepted %s", name)
		}
	}
}
