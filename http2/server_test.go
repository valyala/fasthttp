package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
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
	scheme    string
	transport *xhttp2.Transport
	client    *stdhttp.Client
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
		scheme:    "http",
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

func TestServerRapidResetLimit(t *testing.T) {
	conn := &serverConn{
		config:             serverConfig{maxRapidResetsPerSecond: 2},
		lastClientStreamID: 5,
		streams:            make(map[uint32]*serverStream),
	}
	for _, streamID := range []uint32{1, 3} {
		if err := conn.processRST(incomingFrame{streamID: streamID}); err != nil {
			t.Fatalf("processRST(%d) error: %v", streamID, err)
		}
	}
	err := conn.processRST(incomingFrame{streamID: 5})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeEnhanceYourCalm {
		t.Fatalf("processRST() error = %v, want ENHANCE_YOUR_CALM", err)
	}
}

func TestMalformedHeaderStreamIDCannotBeReused(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 10},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]struct{}),
	}
	event := incomingFrame{
		kind:     incomingFrameStreamError,
		streamID: 1,
		errCode:  xhttp2.ErrCodeProtocol,
		err:      errInvalidRequestHeaders,
	}
	if err := conn.processHeaderStreamError(event); err != nil {
		t.Fatalf("first malformed stream error: %v", err)
	}
	if conn.lastClientStreamID != 1 {
		t.Fatalf("last client stream ID = %d, want 1", conn.lastClientStreamID)
	}
	if _, ok := conn.closedClientStreams[1]; !ok {
		t.Fatal("malformed stream wasn't remembered as closed")
	}
	err := conn.processHeaderStreamError(event)
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("reused malformed stream error = %v, want connection PROTOCOL_ERROR", err)
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

func TestProcessStreamingDataTransfersOwnershipWithoutConsumingPayload(t *testing.T) {
	const windowSize = 1 << 20
	conn := &serverConn{
		config: serverConfig{
			connectionWindowSize: windowSize,
			streamWindowSize:     windowSize,
		},
		streams:                 make(map[uint32]*serverStream),
		receiveConnectionWindow: windowSize,
		framer:                  xhttp2.NewFramer(io.Discard, nil),
	}
	stream := &serverStream{
		id:           1,
		conn:         conn,
		recvWindow:   windowSize,
		expectedBody: -1,
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
	if conn.pendingConnectionUpdate != 0 {
		t.Fatalf("pending connection update before body read = %d, want 0", conn.pendingConnectionUpdate)
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
	if conn.pendingConnectionUpdate != 4 {
		t.Fatalf("pending connection update after body read = %d, want 4", conn.pendingConnectionUpdate)
	}
	stream.body.discardWithError(errStreamClosed)
}
