package http2

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	stdhttp "net/http"
	"net/http/httptrace"
	"net/textproto"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type testServer struct {
	server    *fasthttp.Server
	listener  net.Listener
	scheme    string
	transport *xhttp2.Transport
	client    *stdhttp.Client
	dials     *atomic.Int64
	serveDone chan error
}

func newTestServer(t testing.TB, server *fasthttp.Server, config ServerConfig) *testServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	if err := ConfigureServer(server, config); err != nil {
		listener.Close()
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	dials := new(atomic.Int64)
	transport := &xhttp2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			dials.Add(1)
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	result := &testServer{
		server:    server,
		listener:  listener,
		scheme:    "http",
		transport: transport,
		client:    &stdhttp.Client{Transport: transport},
		dials:     dials,
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
	return s.scheme + "://" + s.listener.Addr().String() + path
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
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("Content-Length = %d, want %d", resp.ContentLength, len(body))
	}
}

func TestConcurrentBufferedUploadsShareConnection(t *testing.T) {
	testCases := []struct {
		name      string
		uploads   int
		bodyBytes int
	}{
		{name: "eight 1 MiB bodies", uploads: 8, bodyBytes: 1 << 20},
		{name: "four 4 MiB bodies", uploads: 4, bodyBytes: 4 << 20},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			started := make(chan struct{}, testCase.uploads)
			release := make(chan struct{})
			server := &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) {
					started <- struct{}{}
					<-release
					ctx.SetBodyString("ok")
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			testServer.transport.StrictMaxConcurrentStreams = true

			var wg sync.WaitGroup
			errs := make(chan error, testCase.uploads)
			body := bytes.Repeat([]byte{'x'}, testCase.bodyBytes)
			for range testCase.uploads {
				wg.Go(func() {
					req, err := stdhttp.NewRequest(stdhttp.MethodPost, testServer.URL("/upload"), bytes.NewReader(body))
					if err != nil {
						errs <- err
						return
					}
					resp, err := testServer.client.Do(req)
					if err == nil {
						_, err = io.Copy(io.Discard, resp.Body)
						closeErr := resp.Body.Close()
						if err == nil {
							err = closeErr
						}
					}
					errs <- err
				})
			}

			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for i := range testCase.uploads {
				select {
				case <-started:
				case <-deadline.C:
					close(release)
					wg.Wait()
					t.Fatalf("only %d/%d upload handlers started", i, testCase.uploads)
				}
			}
			close(release)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent buffered upload failed: %v", err)
				}
			}
			if got := testServer.dials.Load(); got != 1 {
				t.Fatalf("concurrent uploads used %d TCP connections, want one", got)
			}
		})
	}
}

func TestServerSendsDefaultDate(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		noDefaultDate bool
	}{
		{name: "default"},
		{name: "NoDefaultDate", noDefaultDate: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := &fasthttp.Server{
				NoDefaultDate: testCase.noDefaultDate,
				Handler:       func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
			}
			testServer := newTestServer(t, server, ServerConfig{})
			resp, err := testServer.client.Get(testServer.URL("/"))
			if err != nil {
				t.Fatalf("Get() error: %v", err)
			}
			defer resp.Body.Close()

			dates := resp.Header.Values("Date")
			if testCase.noDefaultDate {
				if len(dates) != 0 {
					t.Fatalf("Date = %q, want none", dates)
				}
				return
			}
			if len(dates) != 1 {
				t.Fatalf("Date fields = %q, want exactly one", dates)
			}
			if _, err := stdhttp.ParseTime(dates[0]); err != nil {
				t.Fatalf("parsing Date %q: %v", dates[0], err)
			}
		})
	}
}

func TestServerLegacyHijackReturnsNotImplemented(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Hijack(func(net.Conn) {
				t.Error("HTTP/2 request invoked a connection hijack handler")
			})
			ctx.SetBodyString("handler body")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	response, err := testServer.client.Get(testServer.URL("/"))
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != fasthttp.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", response.StatusCode)
	}
}

func TestNormalizeServerConfigInheritsReadTimeoutForIdleConnections(t *testing.T) {
	server := &fasthttp.Server{ReadTimeout: 3 * time.Second}
	config, err := normalizeServerConfig(server, &ServerConfig{})
	if err != nil {
		t.Fatalf("normalizeServerConfig() error: %v", err)
	}
	if config.idleTimeout != server.ReadTimeout {
		t.Fatalf("idle timeout = %v, want inherited ReadTimeout %v", config.idleTimeout, server.ReadTimeout)
	}
}

func TestNormalizeServerConfigSeparatesFlowAndBufferedBodyLimits(t *testing.T) {
	config, err := normalizeServerConfig(&fasthttp.Server{}, &ServerConfig{})
	if err != nil {
		t.Fatalf("normalizeServerConfig() error: %v", err)
	}
	if config.connectionWindowSize != defaultConnectionWindowSize {
		t.Fatalf("connection window = %d, want %d", config.connectionWindowSize, defaultConnectionWindowSize)
	}
	if config.maxBufferedRequestBody != defaultBufferedRequestBodySize {
		t.Fatalf("buffered body limit = %d, want %d", config.maxBufferedRequestBody, defaultBufferedRequestBodySize)
	}

	const (
		flowWindow = 2 << 20
		bodyLimit  = 32 << 20
	)
	config, err = normalizeServerConfig(&fasthttp.Server{}, &ServerConfig{
		MaxUploadBufferPerConnection:        flowWindow,
		MaxBufferedRequestBodyPerConnection: bodyLimit,
	})
	if err != nil {
		t.Fatalf("normalizeServerConfig(custom) error: %v", err)
	}
	if config.connectionWindowSize != flowWindow || config.maxBufferedRequestBody != bodyLimit {
		t.Fatalf("custom limits = {flow:%d buffered:%d}, want {%d %d}",
			config.connectionWindowSize, config.maxBufferedRequestBody, flowWindow, bodyLimit)
	}
}

func TestServerWriteTimeoutBoundsBlockedPeer(t *testing.T) {
	serverConn, peerConn := net.Pipe()
	defer peerConn.Close()
	server := &fasthttp.Server{WriteTimeout: 50 * time.Millisecond}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- ServeConn(server, serverConn, ServerConfig{})
	}()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = io.WriteString(peerConn, clientPreface)
		_ = xhttp2.NewFramer(peerConn, nil).WriteSettings()
	}()

	select {
	case err := <-serveDone:
		if !isTimeout(err) {
			t.Fatalf("ServeConn() error = %v, want write timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeConn() remained blocked on a peer that didn't read")
	}
	_ = peerConn.Close()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("peer writer didn't unblock after server write timeout")
	}
}

func TestServerReadTimeoutCancelsOnlyRequestStream(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	conn := &serverConn{
		server:   &fasthttp.Server{ReadTimeout: 20 * time.Millisecond},
		ctx:      ctx,
		commands: make(chan serverCommand, 1),
	}
	stream := &serverStream{id: 3}
	conn.armRequestReadTimeout(stream)
	if deadline, ok := stream.Deadline(); ok {
		t.Fatalf("transport read timeout leaked as application deadline %v", deadline)
	}
	select {
	case command := <-conn.commands:
		if command.kind != serverCommandRequestReadTimeout || command.streamID != stream.id ||
			!errors.Is(command.err, fasthttp.ErrTimeout) {
			t.Fatalf("timeout command = %+v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("request stream wasn't canceled after Server.ReadTimeout")
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
		slowResponse, slowErr = testServer.client.Get(testServer.URL("/slow")) //nolint:bodyclose // closed after wait.Wait
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
		fastResponse, fastErr = testServer.client.Get(testServer.URL("/fast")) //nolint:bodyclose // closed after fastDone
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
			ctx.Response.Header.Set("Set-Cookie", "session=secret")
			if err := ctx.EarlyHints(); err != nil {
				t.Errorf("EarlyHints() error: %v", err)
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})

	gotStatus := make(chan int, 1)
	req, err := stdhttp.NewRequest(stdhttp.MethodGet, testServer.URL("/"), stdhttp.NoBody)
	if err != nil {
		t.Fatalf("NewRequest() error: %v", err)
	}
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			gotStatus <- code
			if got := header.Get("Set-Cookie"); got != "" {
				t.Errorf("103 response leaked Set-Cookie %q", got)
			}
			if got := header.Get("Link"); got != "</style.css>; rel=preload" {
				t.Errorf("103 Link = %q", got)
			}
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

func TestServerRapidResetLimit(t *testing.T) {
	conn := &serverConn{
		config:             serverConfig{maxRapidResetsPerSecond: 2},
		lastClientStreamID: 5,
		streams:            make(map[uint32]*serverStream),
	}
	for _, streamID := range []uint32{1, 3} {
		if err := conn.processRST(&incomingFrame{streamID: streamID}); err != nil {
			t.Fatalf("processRST(%d) error: %v", streamID, err)
		}
	}
	err := conn.processRST(&incomingFrame{streamID: 5})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeEnhanceYourCalm {
		t.Fatalf("processRST() error = %v, want ENHANCE_YOUR_CALM", err)
	}
}

func TestServerInitiatedResetUsesRapidResetLimit(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    3,
			maxRapidResetsPerSecond: 2,
		},
		framer:  xhttp2.NewFramer(&wire, nil),
		streams: make(map[uint32]*serverStream),
	}
	for _, streamID := range []uint32{1, 3, 5} {
		stream := newServerStream(conn, streamID)
		stream.handlerStarted = true // keep reset streams out of the pool
		conn.streams[streamID] = stream
	}
	for _, streamID := range []uint32{1, 3} {
		if err := conn.resetStream(streamID, xhttp2.ErrCodeFlowControl, errors.New("peer-triggered")); err != nil {
			t.Fatalf("resetStream(%d) error: %v", streamID, err)
		}
	}
	err := conn.resetStream(5, xhttp2.ErrCodeFlowControl, errors.New("peer-triggered"))
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeEnhanceYourCalm {
		t.Fatalf("third server reset error = %v, want ENHANCE_YOUR_CALM", err)
	}
}

func TestClosedPushFramesAreNotIdleStreamErrors(t *testing.T) {
	conn := &serverConn{
		config:             serverConfig{maxRapidResetsPerSecond: 100},
		streams:            make(map[uint32]*serverStream),
		lastClientStreamID: 1,
		nextPushStreamID:   4, // stream 2 was promised and has since closed
	}
	if err := conn.processRST(&incomingFrame{streamID: 2, errCode: xhttp2.ErrCodeCancel}); err != nil {
		t.Fatalf("late reset of pushed stream: %v", err)
	}
	if err := conn.processWindowUpdate(&incomingFrame{streamID: 2, increment: 1000}); err != nil {
		t.Fatalf("late window update of pushed stream: %v", err)
	}
	err := conn.processRST(&incomingFrame{streamID: 4, errCode: xhttp2.ErrCodeCancel})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("reset of unpromised stream error = %v, want PROTOCOL_ERROR", err)
	}
}

func TestReadLoopPreservesStreamErrorScope(t *testing.T) {
	wire := bufio.NewReader(bytes.NewReader([]byte{
		0, 0, 4, byte(xhttp2.FrameWindowUpdate), 0, 0, 0, 0, 1,
		0, 0, 0, 0,
	}))
	ctx, cancel := context.WithCancelCause(context.Background())
	conn := &serverConn{
		ctx:    ctx,
		cancel: cancel,
		framer: xhttp2.NewFramer(nil, wire),
		events: make(chan incomingFrame, 1),
	}
	conn.frames = newFrameReader(conn.framer, wire)
	go conn.readLoop()
	event := <-conn.events
	cancel(nil)
	if event.kind != incomingFrameStreamError || event.frameType != xhttp2.FrameWindowUpdate ||
		event.streamID != 1 || event.errCode != xhttp2.ErrCodeProtocol {
		t.Fatalf("readLoop() event = {kind:%d stream:%d code:%v err:%v}", event.kind, event.streamID, event.errCode, event.err)
	}
	state := &serverConn{streams: make(map[uint32]*serverStream)}
	err := state.processHeaderStreamError(&event)
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("idle WINDOW_UPDATE stream error = %v, want connection PROTOCOL_ERROR", err)
	}
}

func TestMalformedHeadersPaddingIsCompressionError(t *testing.T) {
	wire := bufio.NewReader(bytes.NewReader([]byte{
		0, 0, 2, byte(xhttp2.FrameHeaders),
		byte(xhttp2.FlagHeadersPadded | xhttp2.FlagHeadersEndHeaders),
		0, 0, 0, 1,
		0xff, 0,
	}))
	framer := xhttp2.NewFramer(nil, wire)
	reader := newFrameReader(framer, wire)
	if _, err := reader.readFrame(); errorCode(err) != xhttp2.ErrCodeCompression {
		t.Fatalf("malformed padding error = %v (%v), want COMPRESSION_ERROR", err, errorCode(err))
	}
}

func TestMalformedHeaderStreamIDCannotBeReused(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 10},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	event := incomingFrame{
		kind:     incomingFrameStreamError,
		streamID: 1,
		errCode:  xhttp2.ErrCodeProtocol,
		err:      errInvalidRequestHeaders,
	}
	if err := conn.processHeaderStreamError(&event); err != nil {
		t.Fatalf("first malformed stream error: %v", err)
	}
	if conn.lastClientStreamID != 1 {
		t.Fatalf("last client stream ID = %d, want 1", conn.lastClientStreamID)
	}
	if _, ok := conn.closedClientStreams[1]; !ok {
		t.Fatal("malformed stream wasn't remembered as closed")
	}
	err := conn.processHeaderStreamError(&event)
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("reused malformed stream error = %v, want connection PROTOCOL_ERROR", err)
	}
}

func TestRejectedHeaderStreamIDCannotBeReused(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	event := incomingFrame{streamID: 1, hasPriority: true, dependency: 1}
	if err := conn.processHeaders(&event); err != nil {
		t.Fatalf("first rejected HEADERS error: %v", err)
	}
	if conn.lastClientStreamID != 1 {
		t.Fatalf("last client stream ID = %d, want 1", conn.lastClientStreamID)
	}
	err := conn.processHeaders(&event)
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeStreamClosed {
		t.Fatalf("reused rejected stream error = %v, want STREAM_CLOSED", err)
	}
}

func TestDrainingStillRejectsEvenStreamIDAsConnectionError(t *testing.T) {
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		framer:              xhttp2.NewFramer(io.Discard, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		isGoingAway:         true,
	}
	err := conn.processHeaders(&incomingFrame{streamID: 2})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("even stream error = %v, want connection PROTOCOL_ERROR", err)
	}
}

func flushOrderConn(t *testing.T, incremental bool) []uint32 {
	t.Helper()
	var wire bytes.Buffer
	conn := &serverConn{
		framer:        xhttp2.NewFramer(&wire, nil),
		streams:       map[uint32]*serverStream{},
		connFlowState: connFlowState{send: sendWindow{window: 1 << 30}, peerMaxFrameSize: defaultMaxFrameSize},
	}
	for _, streamID := range []uint32{1, 3} {
		stream := &serverStream{
			id:              streamID,
			priority:        priority{urgency: 3, incremental: incremental},
			streamFlowState: streamFlowState{send: sendWindow{window: 1 << 30}},
			pendingData:     bytes.Repeat([]byte{byte(streamID)}, 3*defaultMaxFrameSize),
		}
		conn.streams[streamID] = stream
		conn.queueFlush(stream)
	}
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	reader := xhttp2.NewFramer(nil, &wire)
	var order []uint32
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			return order
		}
		order = append(order, frame.Header().StreamID)
	}
}

func TestFlushServesSequentialResponsesToCompletion(t *testing.T) {
	order := flushOrderConn(t, false)
	if len(order) != 6 {
		t.Fatalf("frames = %d, want 6", len(order))
	}
	want := []uint32{1, 1, 1, 3, 3, 3}
	for i, streamID := range want {
		if order[i] != streamID {
			t.Fatalf("frame order = %v, want %v", order, want)
		}
	}
}

func TestFlushInterleavesIncrementalResponses(t *testing.T) {
	order := flushOrderConn(t, true)
	if len(order) != 6 {
		t.Fatalf("frames = %d, want 6", len(order))
	}
	want := []uint32{1, 3, 1, 3, 1, 3}
	for i, streamID := range want {
		if order[i] != streamID {
			t.Fatalf("frame order = %v, want %v", order, want)
		}
	}
}

func TestParsePriority(t *testing.T) {
	tests := []struct {
		value   string
		want    priority
		wantErr bool
	}{
		{"u=0", priority{urgency: 0}, false},
		{"u=7, i", priority{urgency: 7, incremental: true}, false},
		{"i=?0, u=2", priority{urgency: 2}, false},
		{"u=1; extension=token, x=ignored", priority{urgency: 1}, false},
		{"u=8", priority{}, true},
		{"u=1, u=2", priority{}, true},
		{"i=?2", priority{}, true},
		{"u=3,,i", priority{}, true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := parsePriority(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("parsePriority() error = %v, wantErr=%v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("parsePriority() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRequestBodyOwnsAndBoundsChunks(t *testing.T) {
	const chunks = maxRequestBodyChunks + 2
	released := 0
	consumed := 0
	body := newRequestBody(func(n int) { consumed += n })
	for i := range chunks {
		data := []byte{byte(i)}
		if err := body.writeOwned(data, func([]byte) { released++ }); err != nil {
			t.Fatalf("writeOwned() error: %v", err)
		}
	}
	if len(body.chunks) > maxRequestBodyChunks {
		t.Fatalf("queued chunks = %d, want at most %d", len(body.chunks), maxRequestBodyChunks)
	}
	body.closeWithError(nil)
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	if len(got) != chunks {
		t.Fatalf("body length = %d, want %d", len(got), chunks)
	}
	for i, value := range got {
		if value != byte(i) {
			t.Fatalf("body[%d] = %d, want %d", i, value, byte(i))
		}
	}
	if consumed != chunks {
		t.Fatalf("consumed bytes = %d, want %d", consumed, chunks)
	}
	if released != chunks {
		t.Fatalf("released chunks = %d, want %d", released, chunks)
	}
}

func TestRequestBodyCloseReleasesOwnedChunks(t *testing.T) {
	released := 0
	body := newRequestBody(nil)
	if err := body.writeOwned([]byte("body"), func([]byte) { released++ }); err != nil {
		t.Fatalf("writeOwned() error: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("second Close() error: %v", err)
	}
	if released != 1 {
		t.Fatalf("released chunks = %d, want 1", released)
	}
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, errStreamClosed) {
		t.Fatalf("Read() error = %v, want %v", err, errStreamClosed)
	}
}

func TestProcessTrailersPublishesAtRequestBodyEOF(t *testing.T) {
	stream := &serverStream{
		id:             1,
		request:        &fasthttp.RequestCtx{},
		body:           newRequestBody(nil),
		expectedBody:   -1,
		handlerStarted: true,
	}
	conn := &serverConn{
		config:  serverConfig{maxConcurrentStreams: 1},
		framer:  xhttp2.NewFramer(io.Discard, nil),
		streams: map[uint32]*serverStream{stream.id: stream},
	}
	stream.conn = conn
	event := incomingFrame{
		streamID:  stream.id,
		endStream: true,
		fields:    []hpack.HeaderField{{Name: "x-checksum", Value: "ok"}},
	}

	if err := conn.processTrailers(stream, &event); err != nil {
		t.Fatalf("processTrailers() error: %v", err)
	}
	if value := stream.request.Request.Header.Peek("X-Checksum"); len(value) != 0 {
		t.Fatalf("trailer before body EOF = %q, want empty", value)
	}
	if _, err := stream.body.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("request body Read() error = %v, want EOF", err)
	}
	if value := string(stream.request.Request.Header.Peek("X-Checksum")); value != "ok" {
		t.Fatalf("trailer after body EOF = %q, want ok", value)
	}
}

func TestProcessStreamingDataTransfersOwnershipWithoutConsumingPayload(t *testing.T) {
	const windowSize = 1 << 20
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize: windowSize,
			streamWindowSize:     windowSize,
		},
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{recv: recvWindow{window: windowSize}},
		framer:        xhttp2.NewFramer(io.Discard, nil),
	}
	stream := &serverStream{
		id:              1,
		conn:            conn,
		streamFlowState: streamFlowState{recv: recvWindow{window: windowSize}},
		expectedBody:    -1,
	}
	stream.body = newRequestBody(func(n int) {
		if err := conn.consumeRequestBytes(stream, int64(n)); err != nil {
			t.Errorf("consumeRequestBytes() error: %v", err)
		}
	})
	conn.streams[stream.id] = stream
	event := incomingFrame{
		kind:       incomingFrameData,
		streamID:   stream.id,
		flowLength: 4,
		data:       []byte("body"),
	}
	if err := conn.processData(&event); err != nil {
		t.Fatalf("processData() error: %v", err)
	}
	if conn.recv.pending != 0 {
		t.Fatalf("pending connection update before body read = %d, want 0", conn.recv.pending)
	}
	if stream.unconsumedFlow != 4 {
		t.Fatalf("unconsumed flow = %d, want 4", stream.unconsumedFlow)
	}
	if event.data != nil {
		t.Fatal("DATA event retained payload after ownership transfer")
	}

	got := make([]byte, 4)
	if _, err := io.ReadFull(stream.body, got); err != nil {
		t.Fatalf("ReadFull() error: %v", err)
	}
	if string(got) != "body" {
		t.Fatalf("body = %q, want body", got)
	}
	if conn.recv.pending != 4 {
		t.Fatalf("pending connection update after body read = %d, want 4", conn.recv.pending)
	}
	stream.body.discardWithError(errStreamClosed)
}

func TestProcessDataBatchesConnectionCreditForClosedStream(t *testing.T) {
	const (
		windowSize = int64(1024)
		flowLength = 120
	)
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize: int32(windowSize),
			streamWindowSize:     int32(windowSize),
			maxConcurrentStreams: 1,
		},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		lastClientStreamID:  1,
		connFlowState:       connFlowState{recv: recvWindow{window: windowSize}},
	}
	event := incomingFrame{
		kind:       incomingFrameData,
		streamID:   1,
		flowLength: flowLength,
		data:       bytes.Repeat([]byte{'x'}, 100),
	}

	if err := conn.processData(&event); err != nil {
		t.Fatalf("processData() error: %v", err)
	}
	if want := windowSize - flowLength; conn.recv.window != want {
		t.Fatalf("connection receive window = %d, want %d", conn.recv.window, want)
	}
	if conn.recv.pending != flowLength {
		t.Fatalf("pending connection update = %d, want %d", conn.recv.pending, flowLength)
	}
	event.flowLength = int(windowSize/2) - flowLength
	event.data = bytes.Repeat([]byte{'x'}, event.flowLength)
	if err := conn.processData(&event); err != nil {
		t.Fatalf("second processData() error: %v", err)
	}
	if conn.recv.window != windowSize {
		t.Fatalf("connection receive window after batch = %d, want %d", conn.recv.window, windowSize)
	}
	requireConnectionWindowUpdate(t, wire.Bytes(), uint32(windowSize/2))
}

func TestBufferedRequestBodiesRespectConfiguredBudget(t *testing.T) {
	const limit = int64(8)
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize:    64,
			streamWindowSize:        64,
			maxBufferedRequestBody:  int32(limit),
			maxConcurrentStreams:    2,
			maxRapidResetsPerSecond: 100,
		},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		connFlowState:       connFlowState{recv: recvWindow{window: 64}},
	}
	newStream := func(id uint32) *serverStream {
		stream := newServerStream(conn, id)
		stream.request = &fasthttp.RequestCtx{}
		stream.maxBody = 64
		stream.expectedBody = -1
		stream.recv.window = 64
		stream.handlerStarted = true // keep a reset stream inspectable
		conn.streams[id] = stream
		return stream
	}
	first := newStream(1)
	second := newStream(3)
	if err := conn.processData(&incomingFrame{
		streamID: 1, flowLength: int(limit), data: bytes.Repeat([]byte{'a'}, int(limit)),
	}); err != nil {
		t.Fatalf("filling connection body budget: %v", err)
	}
	if err := conn.processData(&incomingFrame{streamID: 3, flowLength: 1, data: []byte{'b'}}); err != nil {
		t.Fatalf("over-budget body frame: %v", err)
	}
	if conn.bufferedRequestBytes != limit || first.bufferedBytes != limit {
		t.Fatalf("buffered bytes = connection:%d stream:%d, want %d", conn.bufferedRequestBytes, first.bufferedBytes, limit)
	}
	if !second.isReset || second.bufferedBytes != 0 {
		t.Fatalf("over-budget stream = {reset:%v buffered:%d}, want reset with no retained body", second.isReset, second.bufferedBytes)
	}
}

func TestProcessDataDoesNotDoubleCreditClosedRequestBody(t *testing.T) {
	const windowSize = int64(1024)
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize: int32(windowSize),
			streamWindowSize:     int32(windowSize),
			maxConcurrentStreams: 1,
		},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		connFlowState:       connFlowState{recv: recvWindow{window: windowSize}},
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.body = newRequestBody(nil)
	stream.body.discardWithError(errStreamClosed)
	conn.streams[stream.id] = stream
	event := incomingFrame{
		streamID:   stream.id,
		flowLength: 4,
		data:       []byte("body"),
	}

	if err := conn.processData(&event); err != nil {
		t.Fatalf("processData() error: %v", err)
	}
	framer := xhttp2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	credited := uint32(0)
	for {
		frame, err := framer.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if update, ok := frame.(*xhttp2.WindowUpdateFrame); ok && update.StreamID == 0 {
			credited += update.Increment
		}
	}
	if credited != 4 {
		t.Fatalf("connection credit = %d, want 4", credited)
	}
	if !stream.isReset || len(stream.pendingData) != 0 {
		t.Fatalf("DATA after END_STREAM left stream reset=%v pending=%d; want reset with no response", stream.isReset, len(stream.pendingData))
	}
}

func TestPendingPriorityUpdatesStayBounded(t *testing.T) {
	conn := &serverConn{
		config:          serverConfig{maxConcurrentStreams: 1},
		streams:         make(map[uint32]*serverStream),
		priorityUpdates: make(map[uint32]priority),
	}
	for streamID := uint32(1); streamID < 200; streamID += 2 {
		if err := conn.processPriorityUpdate(&incomingFrame{
			streamID: streamID,
			priority: "u=1",
		}); err != nil {
			t.Fatalf("processPriorityUpdate(%d) error: %v", streamID, err)
		}
	}
	if got := len(conn.priorityUpdates); got != 4 {
		t.Fatalf("pending priority updates = %d, want bounded limit 4", got)
	}
	if _, exists := conn.priorityUpdates[199]; exists {
		t.Fatal("update past the full hint cache wasn't dropped")
	}

	// Opening streams moves lastClientStreamID forward; the next insert into a
	// full cache must prune the expired hints instead of dropping the new one.
	conn.lastClientStreamID = 9
	if err := conn.processPriorityUpdate(&incomingFrame{streamID: 11, priority: "u=1"}); err != nil {
		t.Fatalf("processPriorityUpdate(11) error: %v", err)
	}
	if _, exists := conn.priorityUpdates[11]; !exists {
		t.Fatal("pruning expired hints didn't make room for a new update")
	}
	if _, exists := conn.priorityUpdates[1]; exists {
		t.Fatal("expired pending priority update survived pruning")
	}
}

func TestPriorityUpdateRejectsInvalidFutureStreamID(t *testing.T) {
	conn := &serverConn{
		config:          serverConfig{maxConcurrentStreams: 1},
		streams:         make(map[uint32]*serverStream),
		priorityUpdates: make(map[uint32]priority),
	}
	err := conn.processPriorityUpdate(&incomingFrame{streamID: 2, priority: "u=1"})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("processPriorityUpdate() error = %v, want connection PROTOCOL_ERROR", err)
	}
}

func TestPeerGoAwayKeepsControlFramesAlive(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		framer:        xhttp2.NewFramer(&wire, nil),
		connFlowState: connFlowState{receivedSettings: true},
	}
	if err := conn.processFrame(&incomingFrame{
		kind:         incomingFrameGoAway,
		lastStreamID: math.MaxInt32,
	}); err != nil {
		t.Fatalf("processing advisory GOAWAY: %v", err)
	}
	if !conn.peerGoingAway || conn.isGoingAway {
		t.Fatalf("GOAWAY state = peer:%v local:%v", conn.peerGoingAway, conn.isGoingAway)
	}
	pingData := [8]byte{'g', 'o', 'a', 'w', 'a', 'y'}
	if err := conn.processFrame(&incomingFrame{kind: incomingFramePing, pingData: pingData}); err != nil {
		t.Fatalf("processing PING after GOAWAY: %v", err)
	}
	frame, err := xhttp2.NewFramer(nil, bytes.NewReader(wire.Bytes())).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	ping, ok := frame.(*xhttp2.PingFrame)
	if !ok || !ping.IsAck() || ping.Data != pingData {
		t.Fatalf("response frame = %#v, want PING ACK", frame)
	}

	err = conn.processFrame(&incomingFrame{kind: incomingFrameGoAway, lastStreamID: math.MaxInt32 - 1})
	if err != nil {
		t.Fatalf("processing decreasing GOAWAY: %v", err)
	}
	err = conn.processFrame(&incomingFrame{kind: incomingFrameGoAway, lastStreamID: math.MaxInt32})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("increasing GOAWAY error = %v, want PROTOCOL_ERROR", err)
	}
}

func TestResetStreamWakesBlockedRequestBody(t *testing.T) {
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize: 1024,
			streamWindowSize:     1024,
			maxConcurrentStreams: 1,
		},
		framer:              xhttp2.NewFramer(io.Discard, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.body = newRequestBody(nil)
	conn.streams[stream.id] = stream

	readDone := make(chan error, 1)
	go func() {
		_, err := stream.body.Read(make([]byte, 1))
		readDone <- err
	}()
	if err := conn.resetStream(stream.id, xhttp2.ErrCodeCancel, errStreamClosed); err != nil {
		t.Fatalf("resetStream() error: %v", err)
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, errStreamClosed) {
			t.Fatalf("request body Read() error = %v, want %v", err, errStreamClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("request body Read() remained blocked after reset")
	}
	stream.handlerDone = true
	conn.maybeFinalizeStream(stream)
}

func TestServerStreamFinalizerWaitsForAllOwners(t *testing.T) {
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	stream := newServerStream(conn, 1)
	stream.localClosed = true
	stream.remoteClosed = true
	stream.handlerStarted = true
	stream.responsePumpStarted = true
	conn.streams[stream.id] = stream

	conn.maybeFinalizeStream(stream)
	if conn.streams[stream.id] == nil {
		t.Fatal("stream finalized while handler and response pump were active")
	}
	stream.handlerDone = true
	conn.maybeFinalizeStream(stream)
	if conn.streams[stream.id] == nil {
		t.Fatal("stream finalized while response pump was active")
	}
	stream.responsePumpDone = true
	conn.maybeFinalizeStream(stream)
	if conn.streams[1] != nil {
		t.Fatal("terminal stream was not finalized after all owners completed")
	}
}

func TestPeerResetDiscardsAlreadyCreditedRequestBody(t *testing.T) {
	conn := &serverConn{
		config:  serverConfig{connectionWindowSize: 1024, streamWindowSize: 1024},
		framer:  xhttp2.NewFramer(io.Discard, nil),
		streams: make(map[uint32]*serverStream),
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.unconsumedFlow = 4
	stream.body = newRequestBody(func(consumed int) {
		if err := conn.processCommand(&serverCommand{
			kind:     serverCommandBodyConsumed,
			streamID: stream.id,
			consumed: consumed,
		}); err != nil {
			t.Errorf("processing body credit: %v", err)
		}
	})
	if err := stream.body.writeOwned([]byte("body"), nil); err != nil {
		t.Fatal(err)
	}
	conn.streams[stream.id] = stream

	if err := conn.processRST(&incomingFrame{streamID: stream.id, errCode: xhttp2.ErrCodeCancel}); err != nil {
		t.Fatalf("processRST() error: %v", err)
	}
	if n, err := stream.body.Read(make([]byte, 4)); n != 0 || !errors.Is(err, errStreamClosed) {
		t.Fatalf("request body Read() = (%d, %v), want (0, %v)", n, err, errStreamClosed)
	}
}

func TestPeerResetDropsPendingResponseBufferBeforeAcknowledging(t *testing.T) {
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		framer:              xhttp2.NewFramer(io.Discard, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	conn.streams[stream.id] = stream
	buffer := []byte("canary")
	ack := make(chan error, 1)
	if err := conn.processCommand(&serverCommand{
		kind:     serverCommandResponseData,
		streamID: stream.id,
		data:     buffer,
		result:   ack,
	}); err != nil {
		t.Fatalf("queueing response DATA: %v", err)
	}
	if err := conn.processRST(&incomingFrame{streamID: stream.id, errCode: xhttp2.ErrCodeCancel}); err != nil {
		t.Fatalf("processing peer reset: %v", err)
	}
	if err := <-ack; !errors.Is(err, errStreamClosed) {
		t.Fatalf("pending DATA acknowledgement = %v, want stream closed", err)
	}
	for i := range buffer {
		buffer[i] = 'x'
	}
	if len(stream.pendingData) != 0 || stream.pendingAck != nil {
		t.Fatal("reset stream retained a response buffer after acknowledging its producer")
	}
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
}

func TestCancelAcceptedStreamWriteReportsPartialAndDropsRemainder(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
			writeByteTimeout:        time.Second,
		},
		framer:        xhttp2.NewFramer(&wire, nil),
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{send: sendWindow{window: 2}, peerMaxFrameSize: defaultMaxFrameSize},
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.send.window = 2
	conn.streams[stream.id] = stream
	write := &streamWrite{result: make(chan streamWriteResult, 1)}
	if err := conn.processCommand(&serverCommand{
		kind: serverCommandResponseData, streamID: 1, data: []byte("four"), write: write,
	}); err != nil {
		t.Fatalf("queueing stream write: %v", err)
	}
	if progressed, err := conn.flushStream(stream, false); err != nil || !progressed {
		t.Fatalf("partial flush = %v, %v; want progress", progressed, err)
	}
	deadlineErr := errStreamTimeout
	if err := conn.processCommand(&serverCommand{
		kind: serverCommandCancelWrite, streamID: 1, write: write, err: deadlineErr,
	}); err != nil {
		t.Fatalf("cancelling accepted write: %v", err)
	}
	result := <-write.result
	if result.n != 2 || !isTimeout(result.err) {
		t.Fatalf("write result = %d, %v; want 2, timeout", result.n, result.err)
	}
	if len(stream.pendingData) != 0 || !stream.isReset {
		t.Fatalf("cancelled write left pending=%d reset=%v", len(stream.pendingData), stream.isReset)
	}
}

func TestResponseFlowStallTimeoutResetsStream(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
			writeByteTimeout:        20 * time.Millisecond,
		},
		ctx:      ctx,
		commands: make(chan serverCommand, 1),
		framer:   xhttp2.NewFramer(&wire, nil),
		streams:  make(map[uint32]*serverStream),
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.pendingData = []byte("blocked")
	conn.streams[stream.id] = stream
	conn.armResponseWriteTimeout(stream)
	select {
	case command := <-conn.commands:
		if err := conn.processCommand(&command); err != nil {
			t.Fatalf("processing response stall timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response flow stall wasn't timed out")
	}
	if !stream.isReset || len(stream.pendingData) != 0 {
		t.Fatalf("stalled stream = {reset:%v pending:%d}, want reset and released", stream.isReset, len(stream.pendingData))
	}
}

func TestStreamHandlerDoneReleasesClosedStream(t *testing.T) {
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
	}
	stream := &serverStream{
		id:            1,
		conn:          conn,
		localClosed:   true,
		remoteClosed:  true,
		streamHandler: func(fasthttp.StreamConn) {},
	}
	conn.streams[stream.id] = stream

	if err := conn.processCommand(&serverCommand{
		kind:     serverCommandStreamHandlerDone,
		streamID: stream.id,
	}); err != nil {
		t.Fatalf("processCommand() error: %v", err)
	}
	if _, ok := conn.streams[1]; ok {
		t.Fatal("closed extended CONNECT stream was not released")
	}
}

func TestServerTimeoutErrorAbandonsOriginalRequestCtx(t *testing.T) {
	releaseHeldCtx := make(chan struct{})
	var releaseHeldCtxOnce sync.Once
	heldPath := make(chan string, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) != "/timeout" {
				ctx.SetBodyString("ok")
				return
			}
			go func() {
				<-releaseHeldCtx
				heldPath <- string(ctx.Path())
			}()
			ctx.TimeoutError("timeout")
		},
	}
	t.Cleanup(func() { releaseHeldCtxOnce.Do(func() { close(releaseHeldCtx) }) })
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())

	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{path: "/timeout", status: fasthttp.StatusRequestTimeout, body: "timeout"},
		{path: "/ok", status: fasthttp.StatusOK, body: "ok"},
	} {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.SetRequestURI(testServer.URL(test.path))
		err := hc.Do(req, resp)
		if err != nil {
			t.Fatalf("Do(%q) error: %v", test.path, err)
		}
		if resp.StatusCode() != test.status || string(resp.Body()) != test.body {
			t.Fatalf("Do(%q) response = (%d, %q), want (%d, %q)", test.path, resp.StatusCode(), resp.Body(), test.status, test.body)
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	releaseHeldCtxOnce.Do(func() { close(releaseHeldCtx) })
	if path := <-heldPath; path != "/timeout" {
		t.Fatalf("abandoned RequestCtx path = %q, want /timeout", path)
	}
}

func requireConnectionWindowUpdate(t testing.TB, wire []byte, expected uint32) {
	t.Helper()
	framer := xhttp2.NewFramer(nil, bytes.NewReader(wire))
	for {
		frame, err := framer.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading emitted frame: %v", err)
		}
		update, ok := frame.(*xhttp2.WindowUpdateFrame)
		if ok && update.StreamID == 0 && update.Increment == expected {
			return
		}
	}
	t.Fatalf("missing connection WINDOW_UPDATE(%d)", expected)
}

// rawPeer is a minimal HTTP/2 peer used by regression tests that need to drive
// exact frame sequences instead of going through a full client.
type rawPeer struct {
	t       testing.TB
	conn    net.Conn
	framer  *xhttp2.Framer
	encoder *hpack.Encoder
	block   *bytes.Buffer
}

func dialRawPeer(t testing.TB, addr string) *rawPeer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatalf("writing preface: %v", err)
	}
	framer := xhttp2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
	block := &bytes.Buffer{}
	peer := &rawPeer{t: t, conn: conn, framer: framer, encoder: hpack.NewEncoder(block), block: block}
	if err := framer.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	return peer
}

func (p *rawPeer) writeHeaders(streamID uint32, endStream bool, fields ...[2]string) {
	p.t.Helper()
	p.block.Reset()
	for _, field := range fields {
		if err := p.encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			p.t.Fatalf("WriteField() error: %v", err)
		}
	}
	if err := p.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: append([]byte(nil), p.block.Bytes()...),
		EndStream:     endStream,
		EndHeaders:    true,
	}); err != nil {
		p.t.Fatalf("WriteHeaders() error: %v", err)
	}
}

func (p *rawPeer) request(streamID uint32, path string, endStream bool) {
	p.t.Helper()
	p.writeHeaders(streamID, endStream,
		[2]string{":method", "GET"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", path},
	)
}

// waitForAny reads frames until one of kinds arrives and reports which one.
// Unlike collect it returns as soon as an expected frame shows up, so tests
// that only need ordering guarantees don't burn a fixed real-time window. A
// connection that dies resolves as the "closed" kind rather than failing the
// test outright: for tests that assert an error stayed stream-scoped, dying is
// a meaningful answer, and a peer may close without sending GOAWAY.
func (p *rawPeer) waitForAny(timeout time.Duration, kinds ...string) (map[string]int, string) {
	p.t.Helper()
	counts := map[string]int{}
	deadline := time.Now().Add(timeout)
	for {
		for _, kind := range kinds {
			if counts[kind] != 0 {
				return counts, kind
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			p.t.Fatalf("none of %v within %v: %v", kinds, timeout, counts)
		}
		_ = p.conn.SetReadDeadline(time.Now().Add(remaining))
		frame, err := p.framer.ReadFrame()
		if err != nil {
			if isTimeout(err) {
				p.t.Fatalf("none of %v within %v: %v", kinds, timeout, counts)
			}
			counts["closed"]++
			return counts, "closed"
		}
		switch frame := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			counts["headers"]++
		case *xhttp2.RSTStreamFrame:
			counts["rst_"+frame.ErrCode.String()]++
		case *xhttp2.GoAwayFrame:
			counts["goaway_"+frame.ErrCode.String()]++
		}
	}
}

// waitForStreamHeaders reports whether HEADERS arrive on wantID, proving the
// connection still serves streams. A GOAWAY or a closed connection reports
// failure instead of a test error, since a server that dies may or may not
// announce it first. It returns as soon as either resolves instead of burning
// a fixed real-time window.
func (p *rawPeer) waitForStreamHeaders(wantID uint32, timeout time.Duration) (survived bool) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			p.t.Fatalf("no HEADERS on stream %d and no GOAWAY within %v", wantID, timeout)
		}
		_ = p.conn.SetReadDeadline(time.Now().Add(remaining))
		frame, err := p.framer.ReadFrame()
		if err != nil {
			if isTimeout(err) {
				p.t.Fatalf("no HEADERS on stream %d and no GOAWAY within %v", wantID, timeout)
			}
			return false
		}
		switch frame := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID == wantID {
				return true
			}
		case *xhttp2.GoAwayFrame:
			return false
		}
	}
}

// collect reads frames until timeout, tallying them by kind.
func (p *rawPeer) collect(timeout time.Duration) map[string]int {
	p.t.Helper()
	counts := map[string]int{}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return counts
		}
		_ = p.conn.SetReadDeadline(time.Now().Add(remaining))
		frame, err := p.framer.ReadFrame()
		if err != nil {
			if isTimeout(err) {
				return counts
			}
			counts["closed"]++
			return counts
		}
		switch frame := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			counts["headers"]++
		case *xhttp2.RSTStreamFrame:
			counts["rst_"+frame.ErrCode.String()]++
		case *xhttp2.GoAwayFrame:
			counts["goaway_"+frame.ErrCode.String()]++
			return counts
		}
	}
}

// A reset extended CONNECT stream must free its MAX_CONCURRENT_STREAMS slot.
// The completion command used to race stream.Done, so it was dropped about half
// the time; repeat enough to make a regression fail reliably.
func TestResetExtendedConnectFreesStreamSlot(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if len(ctx.Request.Header.ConnectProtocol()) != 0 {
				if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
					buffer := make([]byte, 64)
					for {
						if _, err := stream.Read(buffer); err != nil {
							return
						}
					}
				}); err != nil {
					t.Errorf("AcceptStream() error: %v", err)
				}
				return
			}
			ctx.SetBodyString("plain")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{
		MaxConcurrentStreams:  1,
		EnableExtendedConnect: true,
	})
	peer := dialRawPeer(t, testServer.listener.Addr().String())

	streamID := uint32(1)
	for round := range 8 {
		peer.writeHeaders(streamID, false,
			[2]string{":method", fasthttp.MethodConnect},
			[2]string{":protocol", "websocket"},
			[2]string{":scheme", "http"},
			[2]string{":authority", "example.com"},
			[2]string{":path", "/ws"},
		)
		// The tunnel's 200 confirms the stream slot is occupied.
		peer.waitForAny(2*time.Second, "headers")
		if err := peer.framer.WriteRSTStream(streamID, xhttp2.ErrCodeCancel); err != nil {
			t.Fatalf("WriteRSTStream() error: %v", err)
		}
		streamID += 2

		// Freeing the slot happens when the tunnel handler goroutine returns
		// and its completion command is processed -- asynchronous to frame
		// processing, so the first follow-up may still see REFUSED_STREAM. A
		// real leak never frees the slot, so every attempt would be refused;
		// bounded retries distinguish a transient race from a leak without a
		// fixed sleep.
		accepted := false
		for attempt := 0; attempt < 20 && !accepted; attempt++ {
			peer.request(streamID, "/plain", true)
			streamID += 2
			_, kind := peer.waitForAny(2*time.Second, "headers", "rst_REFUSED_STREAM")
			accepted = kind == "headers"
		}
		if !accepted {
			t.Fatalf("round %d: stream slot stayed occupied after the tunnel reset", round)
		}
	}
}

func TestExtendedConnectWriteDeadlineDoesNotDeliverFailedWrite(t *testing.T) {
	type writeResult struct {
		n   int
		err error
	}
	result := make(chan writeResult, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
				_ = stream.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
				n, err := stream.Write([]byte("must-not-arrive"))
				result <- writeResult{n: n, err: err}
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":protocol", "websocket"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/ws"},
	)
	peer.waitForAny(2*time.Second, "headers")
	select {
	case got := <-result:
		if got.n != 0 || !isTimeout(got.err) {
			t.Fatalf("Write() = %d, %v; want 0, timeout", got.n, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream write didn't honor its deadline")
	}
	if err := peer.framer.WriteWindowUpdate(1, 1<<20); err != nil {
		t.Fatalf("stream window update: %v", err)
	}
	if err := peer.framer.WriteWindowUpdate(0, 1<<20); err != nil {
		t.Fatalf("connection window update: %v", err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	sawReset := false
	for {
		_ = peer.conn.SetReadDeadline(deadline)
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			if isTimeout(err) {
				break
			}
			break
		}
		switch frame := frame.(type) {
		case *xhttp2.DataFrame:
			if len(frame.Data()) != 0 {
				t.Fatalf("peer received %q after Write reported failure", frame.Data())
			}
		case *xhttp2.RSTStreamFrame:
			sawReset = true
		}
	}
	if !sawReset {
		t.Fatal("timed-out stream write did not cancel its stream")
	}
}

// A tunnel never sends END_STREAM, so the request read timeout must not apply.
func TestRequestReadTimeoutSkipsExtendedConnect(t *testing.T) {
	server := &fasthttp.Server{
		ReadTimeout: 100 * time.Millisecond,
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
				buffer := make([]byte, 64)
				for {
					n, err := stream.Read(buffer)
					if err != nil {
						return
					}
					if _, err := stream.Write(buffer[:n]); err != nil {
						return
					}
				}
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":protocol", "websocket"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/ws"},
	)

	// Keep the tunnel busy for several multiples of ReadTimeout.
	for range 6 {
		if err := peer.framer.WriteData(1, false, []byte("ping")); err != nil {
			t.Fatalf("WriteData() error: %v", err)
		}
		if counts := peer.collect(60 * time.Millisecond); counts["rst_CANCEL"] != 0 {
			t.Fatalf("active tunnel was reset by the request read timeout: %v", counts)
		}
	}
}

// A transport read timeout is mutable request-body state, whereas
// context.Context requires successive Deadline calls to be stable.
func TestRequestDeadlineDoesNotExposeTransportTimeout(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	conn := &serverConn{
		server:   &fasthttp.Server{ReadTimeout: time.Minute},
		ctx:      ctx,
		commands: make(chan serverCommand, 1),
	}
	stream := &serverStream{id: 1}
	conn.armRequestReadTimeout(stream)
	if deadline, ok := stream.Deadline(); ok {
		t.Fatalf("Deadline() = %v, true; want no application deadline", deadline)
	}
	conn.stopRequestReadTimeout(stream)
	if deadline, ok := stream.Deadline(); ok {
		t.Fatalf("Deadline() changed after body completion: %v", deadline)
	}
}

// An informational response the peer's SETTINGS_MAX_HEADER_LIST_SIZE rejects is
// scoped to one stream; it must not tear down the connection.
func TestOversizeInformationalResetsOnlyTheStream(t *testing.T) {
	var hintErr atomic.Value
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/hints" {
				ctx.Response.Header.Add("Link", "</"+string(bytes.Repeat([]byte("a"), 4096))+">; rel=preload")
				if err := ctx.EarlyHints(); err != nil {
					hintErr.Store(err.Error())
				}
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingMaxHeaderListSize, Val: 512}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}

	// Stream 1 keeps the oversize Link header, so its final response is
	// rejected too and it never answers; survival has to be read off a second
	// stream. Wait for the handler to observe the rejection first: that proves
	// the informational command was processed and any fatal outcome already
	// decided, which a concurrent trivial handler could otherwise answer ahead
	// of.
	peer.request(1, "/hints", true)
	deadline := time.Now().Add(2 * time.Second)
	for hintErr.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("EarlyHints() didn't report the oversize header list to the handler")
		}
		time.Sleep(time.Millisecond)
	}

	peer.request(3, "/plain", true)
	if !peer.waitForStreamHeaders(3, 2*time.Second) {
		t.Fatal("oversize informational response killed the connection")
	}
}

// trailerAtCloseBody sets an oversize trailer value when the response body
// stream is closed. The response pump closes the body before it reports EOF, so
// the trailer only reaches the encoder after the response headers are already
// on the wire -- the ordering that isolates the trailer encode path.
type trailerAtCloseBody struct {
	body   *bytes.Reader
	header *fasthttp.ResponseHeader
	name   string
	value  string
}

func (b *trailerAtCloseBody) Read(p []byte) (int, error) { return b.body.Read(p) }

func (b *trailerAtCloseBody) Close() error {
	b.header.Set(b.name, b.value)
	return nil
}

// Same contract for response trailers, which used to escape as a connection
// error without even emitting a GOAWAY.
func TestOversizeResponseTrailersResetOnlyTheStream(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/trailers" {
				ctx.Response.Header.Set("Trailer", "X-Pad")
				ctx.SetBodyStream(&trailerAtCloseBody{
					body:   bytes.NewReader([]byte("body")),
					header: &ctx.Response.Header,
					name:   "X-Pad",
					value:  string(bytes.Repeat([]byte("p"), 4096)),
				}, -1)
				return
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingMaxHeaderListSize, Val: 512}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}

	// Stream 1's own 200 arrives before its trailer encode fails, so it proves
	// nothing. The failure itself is the discriminator: scoped handling emits
	// RST_STREAM on this stream, fatal handling emits GOAWAY. Both are ordered
	// after the failure, so exactly one of them arrives.
	peer.request(1, "/trailers", true)
	counts, kind := peer.waitForAny(2*time.Second, "rst_INTERNAL_ERROR", "goaway_INTERNAL_ERROR", "closed")
	if kind != "rst_INTERNAL_ERROR" {
		t.Fatalf("oversize response trailers killed the connection: %v", counts)
	}
}

// RFC 9113 §5.4.2: don't answer every DATA frame on a closed stream with its
// own RST_STREAM.
func TestClosedStreamIsResetOnlyOnce(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
	}, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())

	peer.request(1, "/", true)
	peer.waitForAny(2*time.Second, "headers")

	payload := make([]byte, 256)
	for range 24 {
		if err := peer.framer.WriteData(1, false, payload); err != nil {
			t.Fatalf("WriteData() error: %v", err)
		}
	}
	// A later request flushes the DATA flood: frames are processed in order, so
	// stream 3's response can only arrive once every DATA frame on the closed
	// stream has been handled and any RST it warranted emitted.
	peer.request(3, "/ping", true)
	rstCount := 0
	deadline := time.Now().Add(2 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("no response on stream 3 while draining the DATA flood")
		}
		_ = peer.conn.SetReadDeadline(time.Now().Add(remaining))
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatalf("draining the DATA flood: %v", err)
		}
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok && rst.ErrCode == xhttp2.ErrCodeStreamClosed {
			rstCount++
		}
		if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok && headers.StreamID == 3 {
			break
		}
	}
	if rstCount > 1 {
		t.Fatalf("server sent %d RST_STREAM frames for one closed stream, want at most 1", rstCount)
	}
}

// A SETTINGS_INITIAL_WINDOW_SIZE decrease may drive an active stream's send
// window negative (RFC 9113 §6.9.2). The server must stop sending immediately,
// account for the deficit, and resume exactly where it stopped once
// WINDOW_UPDATE brings the window positive again. h2spec skips its variant of
// this case whenever the server answers before the shrink lands, so the
// behavior is pinned here deterministically.
func TestSettingsDecreaseDrivesSendWindowNegative(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("0123456789")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())

	var streamEnded bool
	waitData := func(want int, phase string) {
		t.Helper()
		received := 0
		deadline := time.Now().Add(2 * time.Second)
		for received < want {
			_ = peer.conn.SetReadDeadline(deadline)
			frame, err := peer.framer.ReadFrame()
			if err != nil {
				t.Fatalf("%s: ReadFrame() error after %d/%d bytes: %v", phase, received, want, err)
			}
			if data, ok := frame.(*xhttp2.DataFrame); ok {
				received += len(data.Data())
				if data.StreamEnded() {
					streamEnded = true
				}
			}
		}
		if received != want {
			t.Fatalf("%s: received %d bytes, want exactly %d", phase, received, want)
		}
	}
	expectNoData := func(wait time.Duration, phase string) {
		t.Helper()
		deadline := time.Now().Add(wait)
		for {
			_ = peer.conn.SetReadDeadline(deadline)
			frame, err := peer.framer.ReadFrame()
			if err != nil {
				if isTimeout(err) {
					return
				}
				t.Fatalf("%s: ReadFrame() error: %v", phase, err)
			}
			if data, ok := frame.(*xhttp2.DataFrame); ok && len(data.Data()) != 0 {
				t.Fatalf("%s: server sent %d body bytes into a non-positive window", phase, len(data.Data()))
			}
		}
	}

	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 5}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	peer.request(1, "/", true)
	waitData(5, "initial window of 5")

	// Window is now 0; shrinking the initial size to 2 drives it to -3.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 2}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	expectNoData(300*time.Millisecond, "window at -3")

	// +4 brings the window to exactly 1: one byte may flow, no more.
	if err := peer.framer.WriteWindowUpdate(1, 4); err != nil {
		t.Fatalf("WriteWindowUpdate() error: %v", err)
	}
	waitData(1, "window raised to 1")
	expectNoData(300*time.Millisecond, "window back at 0")

	// Open the window fully; the remaining 4 bytes must arrive and end the stream.
	if err := peer.framer.WriteWindowUpdate(1, 100); err != nil {
		t.Fatalf("WriteWindowUpdate() error: %v", err)
	}
	waitData(4, "window fully open")
	if !streamEnded {
		deadline := time.Now().Add(2 * time.Second)
		for !streamEnded {
			_ = peer.conn.SetReadDeadline(deadline)
			frame, err := peer.framer.ReadFrame()
			if err != nil {
				t.Fatalf("waiting for END_STREAM: %v", err)
			}
			if data, ok := frame.(*xhttp2.DataFrame); ok {
				if len(data.Data()) != 0 {
					t.Fatalf("unexpected extra body bytes: %d", len(data.Data()))
				}
				streamEnded = data.StreamEnded()
			}
		}
	}
}

func TestRepeatedInitialWindowSettingsApplyOneFinalDelta(t *testing.T) {
	conn := &serverConn{
		config:        serverConfig{maxEncoderTableSize: defaultHeaderTableSize},
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{peerInitialStreamWindow: 65535},
	}
	for id := uint32(1); id <= 499; id += 2 {
		conn.streams[id] = &serverStream{id: id, streamFlowState: streamFlowState{send: sendWindow{window: 65535}}}
	}
	conn.initHeaderEncoder(defaultHeaderTableSize)
	settings := make([]xhttp2.Setting, 2730)
	for i := range settings {
		settings[i] = xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 65535}
	}
	settings[len(settings)-1].Val = 32768
	if err := conn.applySettings(settings); err != nil {
		t.Fatalf("applySettings() error: %v", err)
	}
	for id, stream := range conn.streams {
		if stream.send.window != 32768 {
			t.Fatalf("stream %d send window = %d, want 32768", id, stream.send.window)
		}
	}
}

// A connection must not leave handler workers parked after it closes.
func TestStreamWorkersDoNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	// Subtests so each round's cleanup runs before the next one starts.
	for round := range 5 {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			server := &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
			}
			testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 100})
			hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
			var wait sync.WaitGroup
			for range 50 {
				wait.Go(func() {
					req := fasthttp.AcquireRequest()
					resp := fasthttp.AcquireResponse()
					defer fasthttp.ReleaseRequest(req)
					defer fasthttp.ReleaseResponse(resp)
					req.SetRequestURI(testServer.URL("/"))
					if err := hc.Do(req, resp); err != nil {
						t.Errorf("Do() error: %v", err)
					}
				})
			}
			wait.Wait()
			hc.CloseIdleConnections()
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+10 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines %d -> %d: stream workers leaked", before, after)
}

func TestServerTLSRequestReportsHTTPSScheme(t *testing.T) {
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
			fmt.Fprintf(ctx, "%s %v", ctx.URI().Scheme(), ctx.IsTLS())
		},
	}
	if err := ConfigureServer(server, ServerConfig{}); err != nil {
		t.Fatalf("ConfigureServer() error: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(tls.NewListener(listener, server.TLSConfig.Clone()))
	}()
	transport := &xhttp2.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
		},
	}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		<-done
	})
	client := &stdhttp.Client{Transport: transport}
	resp, err := client.Get("https://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if got := string(body); got != "https true" {
		t.Fatalf("scheme and IsTLS = %q, want %q", got, "https true")
	}
}

func TestServerHonorsNoDefaultContentType(t *testing.T) {
	server := &fasthttp.Server{
		NoDefaultContentType: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	resp, err := testServer.client.Get(testServer.URL("/"))
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	_ = resp.Body.Close()
	if values := resp.Header.Values("Content-Type"); len(values) != 0 {
		t.Fatalf("Content-Type = %q, want none", values)
	}
}

// stalledResponseConn returns a connection whose only stream owes data it
// cannot send: both send windows are zero.
func stalledResponseConn(t *testing.T, writeByteTimeout time.Duration) (*serverConn, *serverStream) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(context.Canceled) })
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
			writeByteTimeout:        writeByteTimeout,
		},
		ctx:           ctx,
		commands:      make(chan serverCommand, 1),
		framer:        xhttp2.NewFramer(io.Discard, nil),
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{peerMaxFrameSize: defaultMaxFrameSize},
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.pendingData = []byte("blocked")
	conn.streams[stream.id] = stream
	conn.queueFlush(stream)
	return conn, stream
}

func TestResponseFlowStallTimeoutSurvivesRepeatedFlushes(t *testing.T) {
	conn, stream := stalledResponseConn(t, 20*time.Millisecond)
	deadline := time.Now().Add(300 * time.Millisecond)
	for len(conn.commands) == 0 && time.Now().Before(deadline) {
		if err := conn.flushResponses(); err != nil {
			t.Fatalf("flushResponses() error: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case command := <-conn.commands:
		if err := conn.processCommand(&command); err != nil {
			t.Fatalf("processing response stall timeout: %v", err)
		}
	default:
		t.Fatal("repeated flushes postponed the response stall timeout")
	}
	if !stream.isReset {
		t.Fatal("stalled stream wasn't reset")
	}
}

func TestResponseWriteTimeoutIgnoresStaleCommand(t *testing.T) {
	conn, stream := stalledResponseConn(t, time.Hour)
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if stream.writeTimer == nil {
		t.Fatal("stall didn't arm the write timeout")
	}
	conn.send.window = 1 << 20
	stream.send.window = 1 << 20
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if len(stream.pendingData) != 0 || stream.writeTimer != nil {
		t.Fatalf("after progress: pending=%d timer=%v, want drained and disarmed", len(stream.pendingData), stream.writeTimer != nil)
	}
	stale := serverCommand{kind: serverCommandResponseWriteTimeout, streamID: 1, generation: 1, err: errStreamTimeout}
	if err := conn.processCommand(&stale); err != nil {
		t.Fatalf("processing stale timeout: %v", err)
	}
	if stream.isReset {
		t.Fatal("stale stall timeout reset a stream that made progress")
	}

	stream.pendingData = []byte("blocked again")
	conn.send.window = 0
	conn.queueFlush(stream)
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if err := conn.processCommand(&stale); err != nil {
		t.Fatalf("processing stale timeout: %v", err)
	}
	if stream.isReset {
		t.Fatal("stale stall timeout reset a re-stalled stream")
	}
	current := serverCommand{kind: serverCommandResponseWriteTimeout, streamID: 1, generation: 2, err: errStreamTimeout}
	if err := conn.processCommand(&current); err != nil {
		t.Fatalf("processing current timeout: %v", err)
	}
	if !stream.isReset {
		t.Fatal("current stall timeout didn't reset the stream")
	}
}
