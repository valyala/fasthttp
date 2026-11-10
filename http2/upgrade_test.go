package http2

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync/atomic"
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

// A client that never sends its preface after the 101 holds Shutdown for
// the preface timeout only.
func TestUpgradeH2CShutdownIsBoundedByThePrefaceTimeout(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {}}
	if err := ConfigureServer(server, ServerConfig{IdleTimeout: 50 * time.Millisecond}); err != nil {
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
		t.Fatal("Shutdown() outlasted the h2c preface timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown() took %v, want at most 1s", elapsed)
	}
}

// An h2c upgrade request read while the server shuts down is still served
// on stream 1, whether the shutdown starts before the request is complete or
// while the preface is awaited.
func TestUpgradeDuringShutdownServesTheUpgradedRequest(t *testing.T) {
	for _, shutdownBeforeUpgrade := range []bool{true, false} {
		name := "during-preface"
		if shutdownBeforeUpgrade {
			name = "before-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) {
					calls.Add(1)
					ctx.SetBodyString("served")
				},
			}
			if err := ConfigureServer(server, ServerConfig{}); err != nil {
				t.Fatalf("ConfigureServer() error: %v", err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			serveDone := make(chan error, 1)
			go func() { serveDone <- server.Serve(listener) }()
			conn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			shutdownDone := make(chan error, 1)
			shutdown := func() {
				go func() { shutdownDone <- server.Shutdown() }()
				time.Sleep(50 * time.Millisecond)
			}

			if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
				"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\n"+
				"HTTP2-Settings: %s\r\n", h2SettingsHeader); err != nil {
				t.Fatal(err)
			}
			if shutdownBeforeUpgrade {
				// The request is in progress when Shutdown starts.
				time.Sleep(50 * time.Millisecond)
				shutdown()
			}
			if _, err := io.WriteString(conn, "\r\n"); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			if response := readHTTP1Response(t, reader); !strings.HasPrefix(response, "HTTP/1.1 101 ") {
				t.Fatalf("upgrade response = %q, want 101", response)
			}
			if !shutdownBeforeUpgrade {
				shutdown()
			}
			if _, err := io.WriteString(conn, clientPreface); err != nil {
				t.Fatalf("writing preface: %v", err)
			}
			framer := xhttp2.NewFramer(conn, reader)
			framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
			if err := framer.WriteSettings(); err != nil {
				t.Fatal(err)
			}
			// The shutdown PING may follow the response; keep answering until
			// the server closes.
			answered := false
			for {
				frame, err := framer.ReadFrame()
				if err != nil {
					if !answered {
						t.Fatalf("connection ended before the upgraded request's response (handler calls = %d): %v", calls.Load(), err)
					}
					if err := <-shutdownDone; err != nil {
						t.Fatalf("Shutdown() error: %v", err)
					}
					if err := <-serveDone; err != nil {
						t.Fatalf("Serve() error: %v", err)
					}
					return
				}
				switch frame := frame.(type) {
				case *xhttp2.MetaHeadersFrame:
					if frame.StreamID != 1 || frame.PseudoValue("status") != "200" {
						t.Fatalf("unexpected response %v", frame.Fields)
					}
					answered = true
				case *xhttp2.GoAwayFrame:
					if frame.LastStreamID < 1 {
						t.Fatalf("GOAWAY(last=%d) excludes the upgraded stream", frame.LastStreamID)
					}
				case *xhttp2.PingFrame:
					if !frame.IsAck() {
						_ = framer.WritePing(true, frame.Data)
					}
				case *xhttp2.SettingsFrame:
					if !frame.IsAck() {
						_ = framer.WriteSettingsAck()
					}
				}
			}
		})
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

// A well-formed HTTP2-Settings payload with an invalid value declines the
// upgrade before the 101, which would otherwise be followed by a connection
// error for the upgraded request.
func TestUpgradeH2CInvalidSettingValueServesHTTP1(t *testing.T) {
	listener := upgradeTestServer(t)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// SETTINGS_INITIAL_WINDOW_SIZE above 2^31-1.
	payload := []byte{0, 4, 0x80, 0, 0, 0}
	fmt.Fprintf(conn, "GET /plain HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: %s\r\n\r\n",
		base64.RawURLEncoding.EncodeToString(payload))

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

// The upgraded request's HeaderReceived RequestConfig reaches stream 1: its
// WriteTimeout bounds the response as it bounds a prior-knowledge stream's
// (E10).
func TestUpgradedStreamKeepsHeaderReceivedWriteTimeout(t *testing.T) {
	const writeTimeout = 30 * time.Millisecond
	server := &fasthttp.Server{
		HeaderReceived: func(*fasthttp.RequestHeader) fasthttp.RequestConfig {
			return fasthttp.RequestConfig{WriteTimeout: writeTimeout}
		},
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("stalled by the peer's window") },
	}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { listener.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// SETTINGS_INITIAL_WINDOW_SIZE = 0: stream 1 can't send DATA.
	settings := base64.RawURLEncoding.EncodeToString([]byte{0, 4, 0, 0, 0, 0})
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: %s\r\n\r\n", settings)
	reader := bufio.NewReader(conn)
	if response := readHTTP1Response(t, reader); !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade response = %q, want 101", response)
	}
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatal(err)
	}
	framer := xhttp2.NewFramer(conn, reader)
	if err := framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("stream 1 wasn't reset by HeaderReceived's WriteTimeout: %v", err)
		}
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok && rst.StreamID == 1 {
			if rst.ErrCode != xhttp2.ErrCodeInternal || time.Since(start) > 20*writeTimeout {
				t.Fatalf("RST_STREAM(%v) after %v, want INTERNAL_ERROR at about %v", rst.ErrCode, time.Since(start), writeTimeout)
			}
			return
		}
	}
}

// ConnRequestNum continues through an h2c upgrade: the upgraded request is
// stream 1 and keeps its number, later streams count on from it; a request
// TimeoutHandler answers for consumes one number, as over HTTP/1 (E15).
func TestConnRequestNumThroughUpgradeAndTimeoutHandler(t *testing.T) {
	nums := make(chan string, 8)
	handler := func(ctx *fasthttp.RequestCtx) {
		nums <- fmt.Sprintf("%s=%d", ctx.Path(), ctx.ConnRequestNum())
		if string(ctx.Path()) == "/slow" {
			time.Sleep(200 * time.Millisecond)
		}
		ctx.SetBodyString("ok")
	}
	server := &fasthttp.Server{Handler: fasthttp.TimeoutHandler(handler, 50*time.Millisecond, "timeout")}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { listener.Close() })
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	// One HTTP/1 request first: the upgrade is the connection's second.
	fmt.Fprintf(conn, "GET /first HTTP/1.1\r\nHost: example.com\r\n\r\n")
	var first fasthttp.Response
	if err := first.Read(reader); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /upgraded HTTP/1.1\r\nHost: example.com\r\n"+
		"Connection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: %s\r\n\r\n", h2SettingsHeader)
	if response := readHTTP1Response(t, reader); !strings.HasPrefix(response, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade response = %q, want 101", response)
	}
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatal(err)
	}
	framer := xhttp2.NewFramer(conn, reader)
	framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
	if err := framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	peer := &rawPeer{t: t, conn: conn, framer: framer}
	peer.block = &bytes.Buffer{}
	peer.encoder = hpack.NewEncoder(peer.block)
	peer.readHeaders(1, isStreamEnd(1))
	for _, next := range []struct {
		streamID uint32
		path     string
	}{{3, "/next"}, {5, "/slow"}, {7, "/after-timeout"}} {
		peer.request(next.streamID, next.path, true)
		peer.readHeaders(next.streamID, isStreamEnd(next.streamID))
	}
	want := []string{"/after-timeout=5", "/first=1", "/next=3", "/slow=4", "/upgraded=2"}
	var got []string
	for range want {
		select {
		case num := <-nums:
			got = append(got, num)
		case <-time.After(2 * time.Second):
			t.Fatalf("handlers recorded %v, want %d requests", got, len(want))
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("ConnRequestNum per request = %v, want %v", got, want)
	}
}
