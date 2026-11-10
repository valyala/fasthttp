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
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func newPriorKnowledgeHostClient(t testing.TB, addr string) *fasthttp.HostClient {
	t.Helper()
	hc := &fasthttp.HostClient{Addr: addr}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	return hc
}

// installTestWriter adds the write machinery newClientConn always installs.
func installTestWriter(t testing.TB, c *clientConn) *clientConn {
	t.Helper()
	if c.writeSem == nil {
		c.writeSem = make(chan struct{}, 1)
	}
	conn := c.conn
	if conn == nil {
		local, remote := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, remote) }()
		t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
		conn = local
		c.conn = local
	}
	c.writer = newAsyncFrameWriter(conn, defaultWriteBufferSize, defaultWriteQueueBatches, c.config.writeByteTimeout)
	if c.bufferedWriter == nil {
		c.bufferedWriter = c.writer
	}
	if c.framer == nil {
		c.framer = xhttp2.NewFramer(c.bufferedWriter, conn)
	}
	if c.senderSignal == nil {
		c.senderSignal = make(chan struct{}, 1)
	}
	if c.done == nil {
		c.done = make(chan struct{})
	}
	if c.senderDone == nil {
		c.senderDone = make(chan struct{})
	}
	go c.sendControl()
	t.Cleanup(func() {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			close(c.done)
		}
		c.mu.Unlock()
		// The sender frames under the write slot and takes it no more once
		// done is closed, so holding the slot puts the abort after its last
		// flush; a slot a test left taken doesn't hold the abort up.
		slotTaken := false
		select {
		case c.writeSem <- struct{}{}:
			slotTaken = true
		case <-time.After(time.Second):
		}
		c.writer.abort(net.ErrClosed)
		if slotTaken {
			<-c.writeSem
		}
	})
	return c
}

// drainSender returns once sendControl framed everything queued so far;
// taking the write slot orders the caller after its flush.
func drainSender(t testing.TB, c *clientConn) {
	t.Helper()
	waitUntil(t, "the sender is drained", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.creditConn == 0 && len(c.creditStreams) == 0 && len(c.controls) == 0
	})
	c.writeSem <- struct{}{}
	<-c.writeSem
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

func TestTransportCloseIdleConnectionsRemovesEmptyPool(t *testing.T) {
	transport := NewTransport(ClientConfig{})
	hostClient := &fasthttp.HostClient{}
	oldPool, err := transport.poolFor(hostClient)
	if err != nil {
		t.Fatalf("poolFor() error: %v", err)
	}

	transport.CloseIdleConnections(hostClient)
	_, retained := transport.pools.Load(hostClient)
	if retained {
		t.Fatal("CloseIdleConnections retained an empty HostClient pool")
	}

	newPool, err := transport.poolFor(hostClient)
	if err != nil {
		t.Fatalf("poolFor() after close error: %v", err)
	}
	if newPool == oldPool {
		t.Fatal("poolFor() reused a retired pool")
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

func TestClientReadTimeoutCancelsOnlyOneStream(t *testing.T) {
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var releaseSlowOnce sync.Once
	releaseSlowHandler := func() { releaseSlowOnce.Do(func() { close(releaseSlow) }) }
	t.Cleanup(releaseSlowHandler)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/slow" {
				close(slowStarted)
				<-releaseSlow
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = time.Second
	hc.ReadTimeout = 250 * time.Millisecond
	// A timed-out GET is retryable; one attempt keeps the slow handler single.
	hc.MaxIdemponentCallAttempts = 1

	slowDone := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI(testServer.URL("/slow"))
		slowDone <- hc.Do(req, resp)
	}()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow handler didn't start")
	}

	fastReq := fasthttp.AcquireRequest()
	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(fastReq)
	defer fasthttp.ReleaseResponse(fastResp)
	fastReq.SetRequestURI(testServer.URL("/fast"))
	if err := hc.Do(fastReq, fastResp); err != nil {
		t.Fatalf("fast request error: %v", err)
	}
	if got := string(fastResp.Body()); got != "ok" {
		t.Fatalf("fast response body = %q, want ok", got)
	}
	if err := <-slowDone; !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("slow request error = %v, want timeout", err)
	}
	releaseSlowHandler()
}

func TestClientReadTimeoutCoversStreamingResponseBody(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.SetBodyStream(reader, -1)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.ReadTimeout = 50 * time.Millisecond
	hc.StreamResponseBody = true

	request := fasthttp.AcquireRequest()
	response := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(request)
	defer fasthttp.ReleaseResponse(response)
	request.SetRequestURI(testServer.URL("/stream"))
	if err := hc.Do(request, response); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := response.BodyStream().Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if !errors.Is(err, fasthttp.ErrTimeout) {
			t.Fatalf("streaming body Read() error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming body Read() ignored HostClient.ReadTimeout")
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

func TestClientResponseBodyLimitUsesFasthttpSentinel(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBody(bytes.Repeat([]byte("x"), 1024))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.MaxResponseBodySize = 16

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/large"))
	if err := hc.Do(req, resp); !errors.Is(err, fasthttp.ErrBodyTooLarge) {
		t.Fatalf("Do() error = %v, want ErrBodyTooLarge", err)
	}
}

func TestClientSkipBodyDiscardsAndCreditsResponseData(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBody(bytes.Repeat([]byte("x"), 1024))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	hc.MaxResponseBodySize = 16

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/skip"))
	resp.SkipBody = true
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if len(resp.Body()) != 0 {
		t.Fatalf("body length = %d, want 0", len(resp.Body()))
	}
}

func TestClientNoBodyResponseAllowsContentLength(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.SetContentLength(1234)
			if string(ctx.Path()) == "/not-modified" {
				ctx.SetStatusCode(fasthttp.StatusNotModified)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "HEAD", method: fasthttp.MethodHead, path: "/head", status: fasthttp.StatusOK},
		{name: "304", method: fasthttp.MethodGet, path: "/not-modified", status: fasthttp.StatusNotModified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.Header.SetMethod(test.method)
			req.SetRequestURI(testServer.URL(test.path))
			if err := hc.Do(req, resp); err != nil {
				t.Fatalf("Do() error: %v", err)
			}
			if resp.StatusCode() != test.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode(), test.status)
			}
			if resp.Header.ContentLength() != 1234 {
				t.Fatalf("content-length = %d, want 1234", resp.Header.ContentLength())
			}
			if len(resp.Body()) != 0 {
				t.Fatalf("body length = %d, want 0", len(resp.Body()))
			}
		})
	}
}

// A content-length on a 204 (RFC 9113 §8.1.1) or on a 2xx CONNECT response
// (RFC 9110 §9.3.6) is tolerated and ignored, as HTTP/1's client ignores it:
// the 204 completes without a reset and the tunnel carries data both ways.
func TestResponseContentLengthSemantics(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1})
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "204", "content-length", "5")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("204 with content-length: %v", err)
	}
	if resp.StatusCode() != fasthttp.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode())
	}
	if resets := countResets(peer.sync(), request.streamID); resets != 0 {
		t.Fatalf("the 204 drew %d RST_STREAM frames", resets)
	}

	opened := make(chan fasthttp.StreamConn, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		tunnelResp := fasthttp.AcquireResponse()
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI("http://" + hc.Addr + "/ws")
		conn, err := hc.OpenStream(req, tunnelResp)
		if err != nil {
			t.Errorf("OpenStream() error: %v", err)
		}
		opened <- conn
	}()
	request = peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200", "content-length", "0")
	conn := <-opened
	if conn == nil {
		t.FailNow()
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	frames := peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameData && frame.streamID == request.streamID })
	if got := string(frames[len(frames)-1].data); got != "ping" {
		t.Fatalf("tunnel data = %q, want ping", got)
	}
	peer.writeData(request.streamID, false, []byte("pong"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("Read() = %q, %v; want pong", buf, err)
	}
	if resets := countResets(peer.sync(), request.streamID); resets != 0 {
		t.Fatalf("the tunnel drew %d RST_STREAM frames", resets)
	}
}

// A second content-length field in a response is malformed as HTTP/1's
// parser treats it, identical values included; one on a bodyless status is
// still accepted.
func TestClientDuplicateContentLengthMatchesHTTP1(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h1      string
		h2      func(peer *clientPeer, streamID uint32)
		wantErr bool
	}{
		{"duplicate", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Length: 2\r\n\r\nok", func(peer *clientPeer, streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200", "content-length", "2", "content-length", "2")
			peer.writeData(streamID, true, []byte("ok"))
		}, true},
		{"duplicate zero", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n", func(peer *clientPeer, streamID uint32) {
			peer.writeHeaders(streamID, true, ":status", "200", "content-length", "0", "content-length", "0")
		}, true},
		{"single", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", func(peer *clientPeer, streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200", "content-length", "2")
			peer.writeData(streamID, true, []byte("ok"))
		}, false},
		{"204 with content-length", "HTTP/1.1 204 No Content\r\nContent-Length: 5\r\n\r\n", func(peer *clientPeer, streamID uint32) {
			peer.writeHeaders(streamID, true, ":status", "204", "content-length", "5")
		}, false},
	} {
		for _, h2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/h2=%v", tc.name, h2), func(t *testing.T) {
				ln := listenPeer(t)
				hc := &fasthttp.HostClient{Addr: ln.Addr().String(), MaxIdemponentCallAttempts: 1}
				if h2 {
					if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
						t.Fatalf("ConfigureHostClient() error: %v", err)
					}
				}
				t.Cleanup(hc.CloseIdleConnections)
				done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
				if h2 {
					peer := acceptPeer(t, ln)
					tc.h2(peer, peer.nextHeaders().streamID)
				} else {
					serveHTTP1Attempts(t, ln, tc.h1)
				}
				err := waitResultOf(t, done)
				if (err != nil) != tc.wantErr {
					t.Fatalf("Do() error = %v, want error %v", err, tc.wantErr)
				}
				if h2 && tc.wantErr && !errors.Is(err, errInvalidResponseHeaders) {
					t.Fatalf("Do() error = %v, want %v", err, errInvalidResponseHeaders)
				}
			})
		}
	}
}

// A promised request with two content-length fields is reset with
// PROTOCOL_ERROR, like one indicating content.
func TestClientRejectsPushWithDuplicateContentLength(t *testing.T) {
	ln := listenPeer(t)
	pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	peer.wmu.Lock()
	err := peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
		StreamID: parent.streamID, PromiseID: 2, EndHeaders: true,
		BlockFragment: peer.encode(":method", "GET", ":scheme", "http", ":authority", parent.header(":authority"),
			":path", "/pushed", "content-length", "0", "content-length", "0"),
	})
	peer.wmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	frames := peer.sync()
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		if frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 && frame.errCode == xhttp2.ErrCodeProtocol {
			return
		}
	}
	select {
	case path := <-pushes.acceptedPath:
		t.Fatalf("the promise reached PushHandler.Accept for %q", path)
	default:
		t.Fatal("no PROTOCOL_ERROR reset for a promised request with two content-length fields")
	}
}

func TestClientAcceptsServerEnablePushZero(t *testing.T) {
	var wire bytes.Buffer
	writer := xhttp2.NewFramer(&wire, nil)
	if err := writer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingEnablePush, Val: 0}); err != nil {
		t.Fatal(err)
	}
	frame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	output := bufio.NewWriter(io.Discard)
	conn := &clientConn{
		writeSem:       make(chan struct{}, 1),
		config:         clientConfig{maxEncoderTableSize: defaultHeaderTableSize},
		framer:         xhttp2.NewFramer(output, nil),
		bufferedWriter: output,
		streams:        make(map[uint32]*clientStream),
	}
	installTestWriter(t, conn)
	if err := conn.processSettings(frame.(*xhttp2.SettingsFrame)); err != nil { //nolint:forcetypeassert
		t.Fatalf("processSettings() error: %v", err)
	}
}

func TestClientRepeatedHeaderTableSizeSettingsSignalTheSmallest(t *testing.T) {
	var wire bytes.Buffer
	writer := xhttp2.NewFramer(&wire, nil)
	if err := writer.WriteSettings(
		xhttp2.Setting{ID: xhttp2.SettingHeaderTableSize, Val: 0},
		xhttp2.Setting{ID: xhttp2.SettingHeaderTableSize, Val: defaultHeaderTableSize},
	); err != nil {
		t.Fatal(err)
	}
	frame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	output := bufio.NewWriter(io.Discard)
	conn := &clientConn{
		writeSem:       make(chan struct{}, 1),
		config:         clientConfig{maxEncoderTableSize: defaultHeaderTableSize},
		framer:         xhttp2.NewFramer(output, nil),
		bufferedWriter: output,
		streams:        make(map[uint32]*clientStream),
	}
	conn.initHeaderEncoder(defaultHeaderTableSize)
	installTestWriter(t, conn)
	if err := conn.processSettings(frame.(*xhttp2.SettingsFrame)); err != nil { //nolint:forcetypeassert
		t.Fatalf("processSettings() error: %v", err)
	}
	drainSender(t, conn) // the sender resizes the encoder as it acks
	conn.buffer.Reset()
	if err := conn.encoder.WriteField(hpack.HeaderField{Name: "x-indexed", Value: "value"}); err != nil {
		t.Fatal(err)
	}
	if block := conn.buffer.Bytes(); !bytes.HasPrefix(block, []byte{0x20, 0x3f, 0xe1, 0x1f}) {
		t.Fatalf("header block = %x, want size updates to 0 then %d first", block, defaultHeaderTableSize)
	}
}

func TestProcessResponseDataReturnsConnectionCreditForClosedStream(t *testing.T) {
	const (
		windowSize = int64(1024)
		flowLength = 120
	)
	var wire bytes.Buffer
	writer := bufio.NewWriter(&wire)
	conn := &clientConn{
		writeSem: make(chan struct{}, 1),
		config: clientConfig{
			connectionWindowSize: int32(windowSize),
			streamWindowSize:     int32(windowSize),
		},
		framer:         xhttp2.NewFramer(writer, nil),
		bufferedWriter: writer,
		streams:        make(map[uint32]*clientStream),
		nextStreamID:   3,
		connFlowState:  connFlowState{recv: recvWindow{window: windowSize}},
	}
	installTestWriter(t, conn)
	frame := makeClientTestDataFrame(t, 1, bytes.Repeat([]byte{'x'}, flowLength))

	if err := conn.processResponseData(frame); err != nil {
		t.Fatalf("processResponseData() error: %v", err)
	}
	drainSender(t, conn)
	// Granted once the WINDOW_UPDATE went out.
	conn.mu.Lock()
	window := conn.recv.window
	conn.mu.Unlock()
	if window != windowSize {
		t.Fatalf("connection receive window = %d, want %d", window, windowSize)
	}
	requireConnectionWindowUpdate(t, wire.Bytes(), flowLength)
}

func TestClosedResponseBodyCallbacksCannotAffectLaterStream(t *testing.T) {
	conn := installTestWriter(t, &clientConn{streams: make(map[uint32]*clientStream)})
	later := &clientStream{id: 3, conn: conn}
	conn.streams[later.id] = later
	body := newResponseBody(conn, 1)
	if err := body.Close(); err != nil {
		t.Fatalf("old response body Close() error: %v", err)
	}
	if conn.streams[later.id] != later || later.err != nil {
		t.Fatalf("old response body affected later stream: stream=%p err=%v", conn.streams[later.id], later.err)
	}
}

func TestClientRefusedStreamReturnsTypedError(t *testing.T) {
	pool := &clientPool{
		hc: &fasthttp.HostClient{},
	}
	conn := &clientConn{
		writeSem:      make(chan struct{}, 1),
		pool:          pool,
		hc:            pool.hc,
		streams:       make(map[uint32]*clientStream),
		nextStreamID:  5,
		activeStreams: 2,
		connFlowState: connFlowState{receivedSettings: true},
	}
	installTestWriter(t, conn)
	pool.conns = []*clientConn{conn}
	result := make(chan clientResult, 1)
	conn.streams[1] = &clientStream{id: 1, conn: conn, result: result}
	conn.streams[3] = &clientStream{id: 3, conn: conn}

	var wire bytes.Buffer
	if err := xhttp2.NewFramer(&wire, nil).WriteRSTStream(1, xhttp2.ErrCodeRefusedStream); err != nil {
		t.Fatal(err)
	}
	frame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.processFrame(frame); err != nil {
		t.Fatalf("processFrame() error: %v", err)
	}
	got := <-result
	if !errors.Is(got.err, ErrRefusedStream) {
		t.Fatalf("stream error = %v, want ErrRefusedStream", got.err)
	}
	var streamErr *StreamError
	if !errors.As(got.err, &streamErr) || streamErr.StreamID != 1 || streamErr.ErrCode != uint32(xhttp2.ErrCodeRefusedStream) {
		t.Fatalf("stream error details = %#v", streamErr)
	}
}

func TestClientGoAwayReturnsTypedError(t *testing.T) {
	pool := &clientPool{
		hc: &fasthttp.HostClient{},
	}
	conn := &clientConn{
		writeSem:      make(chan struct{}, 1),
		pool:          pool,
		hc:            pool.hc,
		streams:       make(map[uint32]*clientStream),
		nextStreamID:  5,
		activeStreams: 2,
	}
	installTestWriter(t, conn)
	pool.conns = []*clientConn{conn}
	result := make(chan clientResult, 1)
	conn.streams[1] = &clientStream{id: 1, conn: conn}
	conn.streams[3] = &clientStream{id: 3, conn: conn, result: result}

	if err := conn.processGoAway(makeClientTestGoAwayFrame(t, 1, xhttp2.ErrCodeNo)); err != nil {
		t.Fatalf("processGoAway() error: %v", err)
	}
	got := <-result
	if !errors.Is(got.err, ErrConnectionDraining) {
		t.Fatalf("GOAWAY stream error = %v, want ErrConnectionDraining", got.err)
	}
	var goAwayErr *GoAwayError
	if !errors.As(got.err, &goAwayErr) || goAwayErr.LastStreamID != 1 {
		t.Fatalf("GOAWAY error details = %#v", goAwayErr)
	}
}

func TestClientRejectsIncreasingGoAwayLastStreamID(t *testing.T) {
	conn := &clientConn{
		writeSem: make(chan struct{}, 1),
		pool: &clientPool{
			hc: &fasthttp.HostClient{},
		},
		streams:       map[uint32]*clientStream{1: {id: 1}},
		activeStreams: 1,
	}
	conn.hc = conn.pool.hc
	if err := conn.processGoAway(makeClientTestGoAwayFrame(t, 1, xhttp2.ErrCodeNo)); err != nil {
		t.Fatalf("first processGoAway() error: %v", err)
	}
	if err := conn.processGoAway(makeClientTestGoAwayFrame(t, 1, xhttp2.ErrCodeNo)); err != nil {
		t.Fatalf("equal processGoAway() error: %v", err)
	}
	if err := conn.processGoAway(makeClientTestGoAwayFrame(t, 3, xhttp2.ErrCodeNo)); err == nil {
		t.Fatal("processGoAway() accepted an increasing last stream ID")
	}
}

func makeClientTestGoAwayFrame(t *testing.T, lastStreamID uint32, code xhttp2.ErrCode) *xhttp2.GoAwayFrame {
	t.Helper()
	var wire bytes.Buffer
	if err := xhttp2.NewFramer(&wire, nil).WriteGoAway(lastStreamID, code, nil); err != nil {
		t.Fatal(err)
	}
	readFrame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	return readFrame.(*xhttp2.GoAwayFrame) //nolint:forcetypeassert
}

func TestRejectedPushPromiseKeepsHPACKDecoderInSync(t *testing.T) {
	var encoded bytes.Buffer
	encoder := hpack.NewEncoder(&encoded)
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-push-before", Value: "before"}); err != nil {
		t.Fatal(err)
	}
	split := encoded.Len()
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-push-after", Value: "after"}); err != nil {
		t.Fatal(err)
	}
	block := bytes.Clone(encoded.Bytes())

	var wire bytes.Buffer
	peer := xhttp2.NewFramer(&wire, nil)
	if err := peer.WritePushPromise(xhttp2.PushPromiseParam{
		StreamID:      1,
		PromiseID:     2,
		BlockFragment: block[:split],
	}); err != nil {
		t.Fatal(err)
	}
	if err := peer.WriteContinuation(1, true, block[split:]); err != nil {
		t.Fatal(err)
	}

	writer := bufio.NewWriter(io.Discard)
	conn := &clientConn{
		writeSem:       make(chan struct{}, 1),
		config:         clientConfig{maxHeaderListSize: 64 << 10},
		framer:         xhttp2.NewFramer(writer, &wire),
		bufferedWriter: writer,
		headerDecoder:  newHeaderCodec(defaultHeaderTableSize, 64<<10),
		streams:        make(map[uint32]*clientStream),
	}
	installTestWriter(t, conn)
	frame, err := conn.framer.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.processPushPromise(frame.(*xhttp2.PushPromiseFrame)); err == nil { //nolint:forcetypeassert
		t.Fatal("push promise received while push is disabled did not fail the connection")
	}

	encoded.Reset()
	if err := encoder.WriteField(hpack.HeaderField{Name: "x-push-after", Value: "after"}); err != nil {
		t.Fatal(err)
	}
	fields, truncated, invalid, err := conn.headerDecoder.decodeComplete(bytes.Clone(encoded.Bytes()), nil)
	if err != nil || truncated || invalid != nil {
		t.Fatalf("decoding probe block: fields=%v truncated=%v invalid=%v err=%v", fields, truncated, invalid, err)
	}
	if len(fields) != 1 || fields[0].Name != "x-push-after" || fields[0].Value != "after" {
		t.Fatalf("probe fields = %#v", fields)
	}
}

func makeClientTestDataFrame(t testing.TB, streamID uint32, data []byte) *xhttp2.DataFrame {
	t.Helper()
	var wire bytes.Buffer
	framer := xhttp2.NewFramer(&wire, nil)
	if err := framer.WriteData(streamID, false, data); err != nil {
		t.Fatalf("writing DATA frame: %v", err)
	}
	frame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatalf("reading DATA frame: %v", err)
	}
	dataFrame, ok := frame.(*xhttp2.DataFrame)
	if !ok {
		t.Fatalf("frame type = %T, want *http2.DataFrame", frame)
	}
	return dataFrame
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

	hc.StreamResponseBody = true
	streamReq := fasthttp.AcquireRequest()
	streamResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(streamReq)
	defer fasthttp.ReleaseResponse(streamResp)
	streamReq.Header.SetMethod(fasthttp.MethodPost)
	streamReq.SetRequestURI("http://" + listener.Addr().String() + "/stream")
	streamReq.SetBody(body)
	if err := hc.Do(streamReq, streamResp); err != nil {
		t.Fatalf("streaming Do() error: %v", err)
	}
	if got := streamResp.Header.Peek("X-Checksum"); len(got) != 0 {
		t.Fatalf("streaming response trailer before EOF = %q, want empty", got)
	}
	streamed := make([]byte, 0, len(body))
	buffer := make([]byte, 4096)
	for {
		n, readErr := streamResp.BodyStream().Read(buffer)
		streamed = append(streamed, buffer[:n]...)
		if readErr == nil {
			if got := streamResp.Header.Peek("X-Checksum"); len(got) != 0 {
				t.Fatalf("streaming response trailer before EOF = %q, want empty", got)
			}
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			t.Fatalf("reading streaming response: %v", readErr)
		}
		break
	}
	if !bytes.Equal(streamed, body) {
		t.Fatalf("streaming response body length = %d, want %d", len(streamed), len(body))
	}
	if got := string(streamResp.Header.Peek("X-Checksum")); got != "ok" {
		t.Fatalf("streaming response trailer after EOF = %q, want ok", got)
	}
}

func TestConnectSuccessHasNoContentLength(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusOK)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.Header.SetMethod(fasthttp.MethodConnect)
	req.SetRequestURI(testServer.URL("/"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := resp.StatusCode(); got != fasthttp.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := resp.Header.Peek(fasthttp.HeaderContentLength); len(got) != 0 {
		t.Fatalf("2xx CONNECT response carried content-length = %q", got)
	}
}

func TestConnectSuccessRejectsContentLength(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusOK)
			ctx.Response.Header.SetContentLength(0)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.writeHeaders(1, true,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":authority", "example.com"},
	)
	counts, kind := peer.waitForAny(2*time.Second, "rst_INTERNAL_ERROR", "headers")
	if kind != "rst_INTERNAL_ERROR" {
		t.Fatalf("waitForAny() = %q (%v), want rst_INTERNAL_ERROR", kind, counts)
	}
}

func TestResponseCarriesConnectionAddrs(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
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
	if resp.RemoteAddr() == nil {
		t.Fatal("Response.RemoteAddr() is nil")
	}
	if resp.LocalAddr() == nil {
		t.Fatal("Response.LocalAddr() is nil")
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
	first := bytes.Repeat([]byte{'a'}, 400_000)
	second := bytes.Repeat([]byte{'b'}, 400_000)
	startWrites := make(chan struct{})
	writeErrors := make(chan error, 2)
	for _, message := range [][]byte{first, second} {
		go func() {
			<-startWrites
			_, writeErr := conn.Write(message)
			writeErrors <- writeErr
		}()
	}
	close(startWrites)
	for range 2 {
		if writeErr := <-writeErrors; writeErr != nil {
			t.Fatalf("Write() error: %v", writeErr)
		}
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("ReadAll() error: %v", err)
	}
	wantFirstThenSecond := append(bytes.Clone(first), second...)
	wantSecondThenFirst := append(bytes.Clone(second), first...)
	if !bytes.Equal(got, wantFirstThenSecond) && !bytes.Equal(got, wantSecondThenFirst) {
		t.Fatal("concurrent StreamConn.Write calls interleaved their payloads")
	}
}

func TestResetStreamByIDRacingFinalization(t *testing.T) {
	// A late reset by ID, from a timer for example, races the caller
	// finishing the stream, which then returns it to the pool.
	c := &clientConn{
		streams: make(map[uint32]*clientStream),
		pool:    &clientPool{},
	}
	installTestWriter(t, c)
	for id := uint32(1); id <= 4000; id += 2 {
		c.mu.Lock()
		c.activeStreams = 2
		c.streams[id] = &clientStream{
			id: id, conn: c, isStreaming: true, localClosed: true, remoteClosed: true,
			callerDone: true, poolable: true, resultSent: true, requestStarted: true,
			result: make(chan clientResult, 1),
		}
		done := make(chan struct{}, 2)
		go func() {
			c.resetStream(id, xhttp2.ErrCodeCancel, errClientStreamClosed, failureLocal)
			done <- struct{}{}
		}()
		go func() {
			c.responseBodyDone(id)
			done <- struct{}{}
		}()
		time.Sleep(time.Microsecond)
		c.mu.Unlock()
		for range 2 {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatalf("stream %d: reset blocked on a pooled stream", id)
			}
		}
	}
}

func TestClientStreamConnClearedWriteDeadlineReachesBlockedWrite(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHandler()
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
					t.Errorf("Read() error: %v", err)
				}
				close(arrived)
				<-release
				_, _ = io.Copy(io.Discard, conn)
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		},
	}
	// A small upload window blocks the write once it is on the wire.
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxUploadBufferPerStream: 16})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, EnableExtendedConnect: true}); err != nil {
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

	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	payload := make([]byte, 64)
	written := make(chan error, 1)
	go func() {
		n, err := conn.Write(payload)
		if err == nil && n != len(payload) {
			err = fmt.Errorf("wrote %d of %d bytes", n, len(payload))
		}
		written <- err
	}()
	<-arrived
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	select {
	case err := <-written:
		t.Fatalf("Write() ended after its deadline was cleared: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	releaseHandler()
	if err := <-written; err != nil {
		t.Fatalf("Write() error: %v", err)
	}
}

func TestExtendedConnectRequiresPeerSetting(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if len(ctx.Request.Header.ConnectProtocol()) != 0 {
				t.Error("server received extended CONNECT without advertising support")
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
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
	if _, err := hc.OpenStream(req, resp); !errors.Is(err, fasthttp.ErrProtocolNotSupported) {
		t.Fatalf("OpenStream() error = %v, want protocol not supported", err)
	}

	req.Reset()
	resp.Reset()
	req.SetRequestURI(testServer.URL("/regular"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("regular request after rejected CONNECT: %v", err)
	}
	if got := string(resp.Body()); got != "ok" {
		t.Fatalf("regular response body = %q, want ok", got)
	}
}

func TestRejectedExtendedConnectDoesNotRunStreamHandler(t *testing.T) {
	streamHandlerCalled := make(chan struct{}, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(fasthttp.StreamConn) {
				streamHandlerCalled <- struct{}{}
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
			ctx.SetStatusCode(fasthttp.StatusForbidden)
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
	if _, err := hc.OpenStream(req, resp); err == nil {
		t.Fatal("OpenStream() succeeded for a rejected extended CONNECT")
	}
	select {
	case <-streamHandlerCalled:
		t.Fatal("stream handler ran for a non-2xx CONNECT response")
	case <-time.After(20 * time.Millisecond):
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
			InsecureSkipVerify: true,
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{}); err != nil {
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
			InsecureSkipVerify: true,
		},
		DialTimeout: func(addr string, timeout time.Duration) (net.Conn, error) {
			dials.Add(1)
			return net.DialTimeout("tcp", addr, timeout)
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
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
		// Drain until the client closes. net.Pipe has no buffer, so closing
		// immediately would deadlock both sides' close_notify writes until
		// crypto/tls's internal five-second write timeout.
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
	}()
	hc := &fasthttp.HostClient{
		Addr:  "localhost:443",
		IsTLS: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DialTimeout: func(string, time.Duration) (net.Conn, error) {
			return clientConn, nil
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: RequireHTTP2}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("https://localhost/")
	if err := hc.Do(req, resp); !errors.Is(err, ErrHTTP2Required) {
		t.Fatalf("Do() error = %v, want %v", err, ErrHTTP2Required)
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

func TestReserveStreamHonorsConfiguredConcurrencyCap(t *testing.T) {
	conn := &clientConn{
		hc:            &fasthttp.HostClient{},
		config:        clientConfig{maxConcurrentStreams: 1},
		connFlowState: connFlowState{peerMaxConcurrentStreams: 100},
		nextStreamID:  1,
		streams:       map[uint32]*clientStream{},
		activeStreams: 1,
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	if stream := conn.reserveStream(req, resp, false, time.Time{}, time.Now()); stream != nil {
		t.Fatal("reserveStream() exceeded ClientConfig.MaxConcurrentStreams")
	}
	conn.activeStreams = 0
	stream := conn.reserveStream(req, resp, false, time.Time{}, time.Now())
	if stream == nil {
		t.Fatal("reserveStream() refused a stream under the configured cap")
	}
	releaseClientResultChannel(stream.result)
	releaseClientStream(stream)
}

func TestPoolRemoveWakesMaxConnsWaiters(t *testing.T) {
	pool := &clientPool{
		hc:        &fasthttp.HostClient{},
		transport: &Transport{},
	}
	waiter := &poolWaiter{ready: make(chan struct{}, 1)}
	pool.startWaiting(waiter)
	pool.mu.Lock()
	pool.retireLocked(nil)
	pool.mu.Unlock()
	pool.retired()
	select {
	case <-waiter.ready:
	default:
		t.Fatal("retired() didn't signal waiters blocked on MaxConns")
	}
}

// A freed stream wakes one waiter, in arrival order, and a wakeup a waiter
// leaves without taking passes to the next; bulk capacity wakes them all.
func TestPoolWaiterWakeups(t *testing.T) {
	pool := &clientPool{hc: &fasthttp.HostClient{}, transport: &Transport{}}
	waiters := make([]*poolWaiter, 3)
	for i := range waiters {
		waiters[i] = &poolWaiter{ready: make(chan struct{}, 1)}
		pool.startWaiting(waiters[i])
	}
	woken := func() []int {
		var indexes []int
		for i, waiter := range waiters {
			if len(waiter.ready) != 0 {
				indexes = append(indexes, i)
			}
		}
		return indexes
	}
	pool.streamFreed()
	if got := woken(); !slices.Equal(got, []int{0}) {
		t.Fatalf("woken after a freed stream = %v, want [0]", got)
	}
	pool.stopWaiting(waiters[0])
	if got := woken(); !slices.Equal(got, []int{1}) {
		t.Fatalf("woken after the first left = %v, want [1]", got)
	}
	<-waiters[1].ready
	pool.capacityChanged()
	if got := woken(); !slices.Equal(got, []int{2}) || pool.waiters.Load() != 0 {
		t.Fatalf("woken after a capacity change = %v with %d queued", got, pool.waiters.Load())
	}
}

// newHTTP1OnlyTLSServer serves HTTP/1.1 only over TLS and reports the
// protocol a request arrived with; dials counts the connections it accepted.
func newHTTP1OnlyTLSServer(t *testing.T) string {
	t.Helper()
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
	return listener.Addr().String()
}

func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	return ln.Addr().String()
}

// A pool whose dial failed holds nothing and is unregistered at once; the
// next request registers a fresh one.
func TestPoolRemovedAfterFailedDial(t *testing.T) {
	hc := &fasthttp.HostClient{Addr: closedPort(t)}
	transport := NewTransport(ClientConfig{Mode: PriorKnowledge})
	if err := configureHostClientTransport(hc, transport); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); err == nil {
			t.Fatal("Do() succeeded against a closed port")
		}
		if n := transport.poolCount(); n != 0 {
			t.Fatalf("pools after a failed dial = %d, want 0", n)
		}
	}
}

// The last waiter to leave a pool unregisters it when it holds nothing: a
// dial that fails while a request is queued leaves no pool behind, and a
// waiter leaving a pool that recorded the HTTP/1 fallback keeps it.
func TestPoolRemovedWhenLastWaiterLeaves(t *testing.T) {
	t.Run("queued request", func(t *testing.T) {
		hc := &fasthttp.HostClient{Addr: closedPort(t)}
		release := make(chan struct{})
		errDial := errors.New("dial failed")
		hc.Dial = func(string) (net.Conn, error) {
			<-release
			return nil, errDial
		}
		transport := NewTransport(ClientConfig{Mode: PriorKnowledge})
		if err := configureHostClientTransport(hc, transport); err != nil {
			t.Fatal(err)
		}
		first := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		waitUntil(t, "the first request dials", func() bool {
			value, ok := transport.pools.Load(hc)
			if !ok {
				return false
			}
			pool := value.(*clientPool) //nolint:forcetypeassert
			pool.mu.Lock()
			defer pool.mu.Unlock()
			return pool.dialing
		})
		pool := clientPoolFor(t, transport, hc)
		second := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		waitUntil(t, "the second request waits", func() bool { return pool.waiters.Load() == 1 })
		close(release)
		for _, done := range []<-chan error{first, second} {
			if err := waitResultOf(t, done); !errors.Is(err, errDial) {
				t.Fatalf("Do() error = %v, want %v", err, errDial)
			}
		}
		if n := transport.poolCount(); n != 0 {
			t.Fatalf("pools after the failed dial = %d, want 0", n)
		}
	})
	t.Run("stopWaiting", func(t *testing.T) {
		transport := NewTransport(ClientConfig{Mode: PriorKnowledge})
		hc := &fasthttp.HostClient{Addr: "127.0.0.1:1"}
		pool, err := transport.poolFor(hc)
		if err != nil {
			t.Fatal(err)
		}
		waiter := &poolWaiter{ready: make(chan struct{}, 1)}
		pool.startWaiting(waiter)
		pool.stopWaiting(waiter)
		if _, ok := transport.pools.Load(hc); ok {
			t.Fatal("an empty pool survived its last waiter")
		}
		fallback, err := transport.poolFor(hc)
		if err != nil {
			t.Fatal(err)
		}
		fallback.mu.Lock()
		fallback.h1Only = true
		fallback.mu.Unlock()
		fallback.startWaiting(waiter)
		fallback.stopWaiting(waiter)
		if _, ok := transport.pools.Load(hc); !ok {
			t.Fatal("a waiter leaving removed the HTTP/1 fallback pool")
		}
		transport.CloseIdleConnections(hc)
		if _, ok := transport.pools.Load(hc); ok {
			t.Fatal("CloseIdleConnections retained the HTTP/1 fallback pool")
		}
	})
}

// An explicit CloseIdleConnections that finds a connection still retiring
// (its lease is released, so the Client's cleaner may already have dropped the
// HostClient) is completed by the last retirement, also for a pool that
// recorded the HTTP/1 fallback; without it the retirement keeps that pool.
func TestPoolExplicitCloseCompletesAfterRetirement(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		transport := NewTransport(ClientConfig{})
		hc := &fasthttp.HostClient{}
		pool, err := transport.poolFor(hc)
		if err != nil {
			t.Fatal(err)
		}
		pool.mu.Lock()
		pool.h1Only = true
		pool.retiring = 1
		pool.mu.Unlock()
		if explicit {
			transport.CloseIdleConnections(hc)
			if n := transport.poolCount(); n != 1 {
				t.Fatalf("pools removed while a connection retires = %d, want 1 retained", n)
			}
		}
		pool.retired()
		if n := transport.poolCount(); n != map[bool]int{false: 1, true: 0}[explicit] {
			t.Fatalf("explicit=%v: pools after the last retirement = %d", explicit, n)
		}
	}
}

// A pool that recorded the ALPN fallback to HTTP/1 stays while the HostClient
// lives, so the fallback isn't renegotiated, and goes with its
// CloseIdleConnections.
func TestPoolKeepsHTTP1FallbackUntilCloseIdleConnections(t *testing.T) {
	addr := newHTTP1OnlyTLSServer(t)
	var dials atomic.Int32
	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
		DialTimeout: func(addr string, timeout time.Duration) (net.Conn, error) {
			dials.Add(1)
			return net.DialTimeout("tcp", addr, timeout)
		},
	}
	transport := NewTransport(ClientConfig{})
	if err := configureHostClientTransport(hc, transport); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	do := func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("https://" + addr + "/")
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		if got := string(resp.Body()); got != "HTTP/1.1" {
			t.Fatalf("body = %q, want HTTP/1.1", got)
		}
	}
	do()
	pool := clientPoolFor(t, transport, hc)
	pool.mu.Lock()
	h1Only := pool.h1Only
	pool.mu.Unlock()
	if !h1Only {
		t.Fatal("the pool didn't record the HTTP/1 fallback")
	}
	waiter := &poolWaiter{ready: make(chan struct{}, 1)}
	pool.startWaiting(waiter)
	pool.stopWaiting(waiter)
	if n := transport.poolCount(); n != 1 {
		t.Fatalf("pools after a waiter left = %d, want 1", n)
	}
	do()
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1: the fallback was renegotiated", got)
	}
	hc.CloseIdleConnections()
	if n := transport.poolCount(); n != 0 {
		t.Fatalf("pools after CloseIdleConnections = %d, want 0", n)
	}
}

// A Client's cleaner closes the HostClients it drops, which releases their
// pools, the HTTP/1 fallback one included; a later request registers one again.
func TestClientCleanerReleasesTransportPools(t *testing.T) {
	addr := newHTTP1OnlyTLSServer(t)
	transport := NewTransport(ClientConfig{})
	c := &fasthttp.Client{
		TLSConfig:           &tls.Config{InsecureSkipVerify: true},
		MaxIdleConnDuration: 100 * time.Millisecond,
	}
	c.ConfigureClient = func(hc *fasthttp.HostClient) error { return configureHostClientTransport(hc, transport) }
	t.Cleanup(c.CloseIdleConnections)
	do := func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("https://" + addr + "/")
		if err := c.Do(req, resp); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
	}
	do()
	if n := transport.poolCount(); n != 1 {
		t.Fatalf("pools = %d, want 1", n)
	}
	waitUntil(t, "the cleaner dropped the HostClient", func() bool { return transport.poolCount() == 0 })
	do()
	if n := transport.poolCount(); n != 1 {
		t.Fatalf("pools after a repeat request = %d, want 1", n)
	}
}

func TestServerPushDoesNotConsumeClientStreamAllowance(t *testing.T) {
	releasePush := make(chan struct{})
	var pushStarted atomic.Bool
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			switch string(ctx.Path()) {
			case "/pushed":
				pushStarted.Store(true)
				<-releasePush
				ctx.SetBodyString("pushed response")
			case "/parent":
				if err := ctx.Push("/pushed", nil); err != nil {
					t.Errorf("Push() error: %v", err)
				}
				ctx.SetBodyString("parent response")
			default:
				ctx.SetBodyString("second response")
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnablePush: true, MaxConcurrentStreams: 1})
	defer close(releasePush)

	dials := new(atomic.Int64)
	hc := &fasthttp.HostClient{
		Addr: testServer.listener.Addr().String(),
		Dial: func(addr string) (net.Conn, error) {
			dials.Add(1)
			return fasthttp.DialTimeout(addr, time.Second)
		},
	}
	pushHandler := &testPushHandler{
		t:            t,
		acceptedPath: make(chan string, 1),
		responseBody: make(chan string, 1),
	}
	if err := ConfigureHostClient(hc, ClientConfig{
		Mode:        PriorKnowledge,
		PushHandler: pushHandler,
	}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/parent"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do(/parent) error: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for !pushStarted.Load() {
		if time.Now().After(deadline) {
			t.Fatal("push stream never reached its handler")
		}
		time.Sleep(time.Millisecond)
	}

	req.Reset()
	resp.Reset()
	req.SetRequestURI(testServer.URL("/second"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do(/second) with an active push: %v", err)
	}
	if got := string(resp.Body()); got != "second response" {
		t.Fatalf("second body = %q", got)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1: the refused stream was retried on a new connection", got)
	}
}

func TestClientRedialsAtStreamIDExhaustion(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
	}
	testServer := newTestServer(t, server, ServerConfig{})
	dials := new(atomic.Int64)
	hc := &fasthttp.HostClient{
		Addr: testServer.listener.Addr().String(),
		Dial: func(addr string) (net.Conn, error) {
			dials.Add(1)
			return fasthttp.DialTimeout(addr, time.Second)
		},
	}
	transport := NewTransport(ClientConfig{Mode: PriorKnowledge})
	if err := hc.RegisterProtocolTransport(transport); err != nil {
		t.Fatalf("RegisterProtocolTransport() error: %v", err)
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

	poolValue, _ := transport.pools.Load(hc)
	pool, ok := poolValue.(*clientPool)
	if !ok {
		t.Fatalf("pool for %p is %T, expecting *clientPool", hc, poolValue)
	}
	pool.mu.Lock()
	if len(pool.conns) != 1 {
		pool.mu.Unlock()
		t.Fatalf("pool connections = %d, want 1", len(pool.conns))
	}
	first := pool.conns[0]
	first.mu.Lock()
	first.nextStreamID = math.MaxInt32 - 3
	first.mu.Unlock()
	pool.mu.Unlock()

	for i := range 8 {
		req.Reset()
		resp.Reset()
		req.SetRequestURI(testServer.URL("/"))
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() #%d across exhaustion: %v", i, err)
		}
		if string(resp.Body()) != "ok" {
			t.Fatalf("Do() #%d body = %q", i, resp.Body())
		}
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want 2: exhaustion must redial exactly once", got)
	}
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

type rejectingPushHandler struct{ declined atomic.Int64 }

func (h *rejectingPushHandler) Accept(_, _ *fasthttp.Request) bool {
	h.declined.Add(1)
	return false
}

func (*rejectingPushHandler) Handle(*fasthttp.Request, *fasthttp.Response) {
	panic("rejected push was handled")
}

func TestDeclinedPushKeepsConnectionReusable(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/pushed" {
				ctx.SetBodyString("pushed")
				return
			}
			if err := ctx.Push("/pushed", nil); err != nil {
				t.Errorf("Push() error: %v", err)
			}
			ctx.SetBodyString("parent")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnablePush: true})
	pushes := &rejectingPushHandler{}
	var dials atomic.Int64
	hc := &fasthttp.HostClient{
		Addr: testServer.listener.Addr().String(),
		Dial: func(addr string) (net.Conn, error) {
			dials.Add(1)
			return net.Dial("tcp", addr)
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, PushHandler: pushes}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	for i := range 6 {
		req.Reset()
		resp.Reset()
		req.SetRequestURI(testServer.URL("/"))
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() #%d error: %v", i, err)
		}
	}
	if got := pushes.declined.Load(); got != 6 {
		t.Fatalf("declined pushes = %d, want 6", got)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("connection dials = %d, want one reusable connection", got)
	}
}

// stalledPeer completes the HTTP/2 handshake, advertises a huge window so the
// client is limited only by TCP, then stops reading. Its writer goroutine parks
// in conn.Write, which is what used to wedge every stream on the connection.
func stalledPeer(t testing.TB) (net.Listener, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopPeer := func() {
		stopOnce.Do(func() { close(stop) })
	}
	t.Cleanup(stopPeer)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		preface := make([]byte, len(clientPreface))
		if _, err := io.ReadFull(conn, preface); err != nil {
			return
		}
		framer := xhttp2.NewFramer(conn, conn)
		framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
		_ = framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
		_ = framer.WriteWindowUpdate(0, 1<<31-1-65535)
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if _, ok := frame.(*xhttp2.MetaHeadersFrame); ok {
				<-stop // stop reading while the client keeps writing
				return
			}
		}
	}()
	return listener, stopPeer
}

// Both transports must bound a physical write that makes no progress, otherwise
// a peer that stops reading parks the writer forever and every producer waiting
// for the connection's write slot inherits that stall.
func TestWriteByteTimeoutHasNonZeroDefault(t *testing.T) {
	clientConfig, err := normalizeClientConfig(&fasthttp.HostClient{}, &ClientConfig{})
	if err != nil {
		t.Fatalf("normalizeClientConfig() error: %v", err)
	}
	if clientConfig.writeByteTimeout <= 0 {
		t.Fatalf("client writeByteTimeout = %v, want a non-zero default", clientConfig.writeByteTimeout)
	}
	serverConfig, err := normalizeServerConfig(&fasthttp.Server{}, &ServerConfig{})
	if err != nil {
		t.Fatalf("normalizeServerConfig() error: %v", err)
	}
	if serverConfig.writeByteTimeout <= 0 {
		t.Fatalf("server writeByteTimeout = %v, want a non-zero default", serverConfig.writeByteTimeout)
	}
}

// With the stock configuration a stalled peer must not make another stream's
// per-request deadline ineffective.
func TestClientDeadlineSurvivesStalledPeerWithDefaultConfig(t *testing.T) {
	listener, stopPeer := stalledPeer(t)
	hc := newPriorKnowledgeHostClient(t, listener.Addr().String())

	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		request := fasthttp.AcquireRequest()
		response := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(request)
		defer fasthttp.ReleaseResponse(response)
		request.SetRequestURI("http://" + listener.Addr().String() + "/upload")
		request.Header.SetMethod(fasthttp.MethodPost)
		request.SetBodyStream(io.LimitReader(neverEndingReader{}, 64<<20), -1)
		_ = hc.Do(request, response)
	}()
	time.Sleep(300 * time.Millisecond)

	returned := make(chan time.Duration, 1)
	go func() {
		request := fasthttp.AcquireRequest()
		response := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(request)
		defer fasthttp.ReleaseResponse(response)
		request.SetRequestURI("http://" + listener.Addr().String() + "/second")
		start := time.Now()
		_ = hc.DoTimeout(request, response, 200*time.Millisecond)
		returned <- time.Since(start)
	}()

	select {
	case elapsed := <-returned:
		if elapsed > 2*time.Second {
			t.Fatalf("DoTimeout(200ms) returned after %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DoTimeout(200ms) never returned while another stream's peer stalled")
	}
	stopPeer()
	<-uploadDone
}

// A stream whose deadline expires while it waits for the connection write slot
// must not consume a stream ID or emit any frame: IDs are assigned under the
// write slot, so the wire sequence of request HEADERS is increasing by
// construction and a canceled waiter simply never appears on it.
func TestStreamCanceledBeforeWriteSlotConsumesNoStreamID(t *testing.T) {
	var wire bytes.Buffer
	bufferedWriter := bufio.NewWriter(&wire)
	pool := &clientPool{}
	conn := &clientConn{
		pool:           pool,
		hc:             &fasthttp.HostClient{},
		writeSem:       make(chan struct{}, 1),
		bufferedWriter: bufferedWriter,
		streams:        make(map[uint32]*clientStream),
		nextStreamID:   5,
		activeStreams:  2,
		connFlowState: connFlowState{
			peerMaxFrameSize:        defaultMaxFrameSize,
			peerMaxHeaderListSize:   1<<32 - 1,
			peerInitialStreamWindow: 65535,
			send:                    sendWindow{window: 65535},
			recv:                    recvWindow{window: 65535},
		},
	}
	installTestWriter(t, conn)
	conn.framer = xhttp2.NewFramer(bufferedWriter, nil)
	conn.initHeaderEncoder(defaultHeaderTableSize)

	canceledRequest := &fasthttp.Request{}
	canceledRequest.Header.SetMethod(fasthttp.MethodGet)
	canceledRequest.SetRequestURI("https://example.com/canceled")
	survivorRequest := &fasthttp.Request{}
	survivorRequest.Header.SetMethod(fasthttp.MethodGet)
	survivorRequest.SetRequestURI("https://example.com/survivor")
	canceled := &clientStream{
		conn:   conn,
		req:    canceledRequest,
		result: make(chan clientResult, 1),
	}
	survivor := &clientStream{
		conn:   conn,
		req:    survivorRequest,
		result: make(chan clientResult, 1),
	}

	// Occupy the write slot so the first stream times out while waiting for
	// it. Before IDs moved under the write slot, this was the window in which
	// a canceled stream left a hole in the HEADERS sequence.
	conn.writeSem <- struct{}{}
	canceledDone := make(chan error, 1)
	go func() {
		canceledDone <- conn.writeRequestHeaders(canceled, true, canceledRequest, nil, time.Now().Add(100*time.Millisecond))
	}()
	if err := <-canceledDone; !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("canceled stream write error = %v, want timeout", err)
	}

	conn.mu.Lock()
	if canceled.id != 0 || conn.nextStreamID != 5 || len(conn.streams) != 0 {
		conn.mu.Unlock()
		t.Fatalf("canceled waiter consumed state: id=%d nextStreamID=%d streams=%d",
			canceled.id, conn.nextStreamID, len(conn.streams))
	}
	conn.mu.Unlock()

	<-conn.writeSem
	if err := conn.writeRequestHeaders(survivor, true, survivorRequest, nil, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("survivor write error: %v", err)
	}

	reader := xhttp2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame() error: %v", err)
	}
	headers, ok := frame.(*xhttp2.HeadersFrame)
	if !ok || headers.StreamID != 5 {
		t.Fatalf("first frame = %#v, want HEADERS for stream 5", frame)
	}
	if frame, err = reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected frame after survivor HEADERS: frame=%#v err=%v", frame, err)
	}
}

// resettingStalledPeer resets the request stream, which drops the client's
// active stream count to zero without failing the connection, and only then
// stops reading. That leaves the connection idle while its writer is parked --
// the state where an unbounded graceful drain used to stall CloseIdleConnections.
func resettingStalledPeer(t testing.TB) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		preface := make([]byte, len(clientPreface))
		if _, err := io.ReadFull(conn, preface); err != nil {
			return
		}
		framer := xhttp2.NewFramer(conn, conn)
		framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
		_ = framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
		_ = framer.WriteWindowUpdate(0, 1<<31-1-65535)
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok {
				time.Sleep(100 * time.Millisecond) // let the body fill the queue
				_ = framer.WriteRSTStream(headers.StreamID, xhttp2.ErrCodeCancel)
				<-stop // stop reading while the writer stays parked
				return
			}
		}
	}()
	return listener
}

// CloseIdleConnections runs while Client holds a lock covering every host, so a
// stalled peer must not be able to stall unrelated hosts. The contract for a
// request without a deadline is WriteByteTimeout: a producer parked on the full
// write queue of a non-reading peer cannot observe its stream dying, so its
// wait is bounded by the no-progress timeout rather than by stream state.
func TestCloseIdleConnectionsDoesNotBlockOnStalledPeer(t *testing.T) {
	listener := resettingStalledPeer(t)
	hc := &fasthttp.HostClient{Addr: listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{
		Mode:             PriorKnowledge,
		WriteByteTimeout: 500 * time.Millisecond,
	}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	// Measure the whole sequence: neither the reset request nor the subsequent
	// idle teardown may outlive the configured no-progress bound by much.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		request := fasthttp.AcquireRequest()
		response := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(request)
		defer fasthttp.ReleaseResponse(response)
		request.SetRequestURI("http://" + listener.Addr().String() + "/upload")
		request.Header.SetMethod(fasthttp.MethodPost)
		request.SetBodyStream(io.LimitReader(neverEndingReader{}, 512<<20), -1)
		_ = hc.Do(request, response)
		hc.CloseIdleConnections()
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		if elapsed > 5*time.Second {
			t.Fatalf("a stalled peer held the client for %v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled peer blocked the client indefinitely")
	}
}

type neverEndingReader struct{}

func (neverEndingReader) Read(p []byte) (int, error) { return len(p), nil }

// With a streaming response body Do returns once the headers arrive, after which
// the caller owns the request again and may release it. The read loop must work
// from snapshots instead of dereferencing it. Meaningful under -race.
func TestStreamingResponseBodyDoesNotTouchCallerRequest(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyStream(bytes.NewReader(bytes.Repeat([]byte("d"), 128<<10)), 128<<10)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())

	for range 20 {
		request := fasthttp.AcquireRequest()
		response := fasthttp.AcquireResponse()
		response.StreamBody = true
		request.SetRequestURI(testServer.URL("/stream"))
		if err := hc.Do(request, response); err != nil {
			fasthttp.ReleaseRequest(request)
			fasthttp.ReleaseResponse(response)
			t.Fatalf("Do() error: %v", err)
		}
		// The documented pattern: the caller owns request again once Do returns.
		fasthttp.ReleaseRequest(request)
		if _, err := io.Copy(io.Discard, response.BodyStream()); err != nil {
			t.Fatalf("reading streamed body: %v", err)
		}
		fasthttp.ReleaseResponse(response)
	}
}

// The request deadline timer holds a raw clientStream pointer, because a stream
// has no ID until it reaches the connection write slot. Stopping a timer does
// not wait for a callback that already began, and the callback is kept off a
// completed stream by lifecycleReleased -- a flag that only survives while the
// object does. Finalization must therefore refuse to pool a stream whose
// callback may still be in flight; pooling it would zero that flag and hand
// the object to another request while the callback still points at it.
func TestFinalizeKeepsStreamOutOfPoolWhileDeadlineCallbackRuns(t *testing.T) {
	pool := &clientPool{}
	conn := &clientConn{
		writeSem: make(chan struct{}, 1),
		pool:     pool,
		hc:       &fasthttp.HostClient{},
		streams:  make(map[uint32]*clientStream),
		notify:   make(chan struct{}),
	}
	installTestWriter(t, conn)
	stream := &clientStream{
		conn:         conn,
		result:       make(chan clientResult, 1),
		poolable:     true,
		callerDone:   true,
		localClosed:  true,
		remoteClosed: true,
	}

	// A timer that has already fired: Stop reports false, exactly as it does
	// for a callback parked on the connection lock.
	fired := make(chan struct{})
	stream.timer = time.AfterFunc(time.Nanosecond, func() { close(fired) })
	<-fired

	conn.mu.Lock()
	conn.maybeFinalizeStreamLocked(stream)
	conn.mu.Unlock()

	if stream.poolable {
		t.Fatal("finalization pooled a stream whose deadline callback may still be in flight")
	}
	if stream.result == nil || !stream.lifecycleReleased {
		t.Fatal("finalization zeroed the stream the callback still points at")
	}

	// The late callback now finds intact state and declines to act.
	conn.expireRequestDeadline(stream)
	if stream.err != nil || stream.resultSent {
		t.Fatalf("late deadline callback acted on a finalized stream: err=%v resultSent=%v",
			stream.err, stream.resultSent)
	}
}

// A connection reaching MaxIdleConnDuration closes itself, including after
// going idle repeatedly.
func TestIdleConnectionClosesAfterMaxIdleConnDuration(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := &fasthttp.HostClient{
		Addr:                testServer.listener.Addr().String(),
		MaxIdleConnDuration: 150 * time.Millisecond,
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)

	// Each request leaves the connection idle and re-arms the timer.
	for range 3 {
		request := fasthttp.AcquireRequest()
		response := fasthttp.AcquireResponse()
		request.SetRequestURI(testServer.URL("/"))
		if err := hc.Do(request, response); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		fasthttp.ReleaseRequest(request)
		fasthttp.ReleaseResponse(response)
		if got := hc.ConnsCount(); got != 1 {
			t.Fatalf("connections = %d after a request, want 1", got)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for hc.ConnsCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle connection outlived MaxIdleConnDuration")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type pacedReader struct {
	r io.Reader
}

func (p *pacedReader) Read(b []byte) (int, error) {
	time.Sleep(time.Millisecond)
	return p.r.Read(b[:min(len(b), 16<<10)])
}

// A response that starts before the request body is sent must not cut the
// upload short, with or without a declared length.
func TestClientStreamedBodyOutlivesResponseHeaders(t *testing.T) {
	const size = 1 << 20
	for _, length := range []int{size, -1} {
		server := &fasthttp.Server{
			StreamRequestBody: true,
			Handler: func(ctx *fasthttp.RequestCtx) {
				body := ctx.RequestBodyStream()
				ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
					n, _ := io.Copy(io.Discard, body)
					_, _ = w.WriteString(strconv.FormatInt(n, 10))
				})
			},
		}
		testServer := newTestServer(t, server, ServerConfig{})
		hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
		var req fasthttp.Request
		var resp fasthttp.Response
		req.SetRequestURI(testServer.URL("/upload"))
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetBodyStream(&pacedReader{r: bytes.NewReader(make([]byte, size))}, length)
		if err := hc.DoTimeout(&req, &resp, 10*time.Second); err != nil {
			t.Fatalf("length %d: DoTimeout() error: %v", length, err)
		}
		if got := string(resp.Body()); got != strconv.Itoa(size) {
			t.Fatalf("length %d: server read %s bytes, want %d", length, got, size)
		}
	}
}

// A server may answer without reading the body; the client then returns the
// response instead of waiting for flow control that never comes.
func TestClientReturnsResponseToUnreadBody(t *testing.T) {
	server := &fasthttp.Server{
		StreamRequestBody: true,
		// Above the declared length, so the handler decides.
		MaxRequestBodySize: 16 << 20,
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
	var req fasthttp.Request
	var resp fasthttp.Response
	req.SetRequestURI(testServer.URL("/upload"))
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBody(make([]byte, 8<<20))
	if err := hc.DoTimeout(&req, &resp, 5*time.Second); err != nil {
		t.Fatalf("DoTimeout() error: %v", err)
	}
	if resp.StatusCode() != fasthttp.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode())
	}
}

// Deadlines set while an OpenStream read or write is blocked must end it.
func TestOpenStreamDeadlineSetWhileBlocked(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(fasthttp.StreamConn) { <-release })
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, EnableExtendedConnect: true}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	open := func() fasthttp.StreamConn {
		var req fasthttp.Request
		var resp fasthttp.Response
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI(testServer.URL("/ws"))
		conn, err := hc.OpenStream(&req, &resp)
		if err != nil {
			t.Fatalf("OpenStream() error: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	for _, tc := range []struct {
		name string
		op   func(fasthttp.StreamConn) error
		set  func(fasthttp.StreamConn) error
	}{
		{"read", func(c fasthttp.StreamConn) error {
			_, err := c.Read(make([]byte, 1))
			return err
		}, func(c fasthttp.StreamConn) error { return c.SetReadDeadline(time.Now()) }},
		{"write", func(c fasthttp.StreamConn) error {
			_, err := c.Write(make([]byte, 8<<20))
			return err
		}, func(c fasthttp.StreamConn) error { return c.SetWriteDeadline(time.Now()) }},
	} {
		conn := open()
		done := make(chan error, 1)
		go func() { done <- tc.op(conn) }()
		time.Sleep(50 * time.Millisecond)
		_ = tc.set(conn)
		select {
		case err := <-done:
			if !isTimeout(err) {
				t.Fatalf("%s error = %v, want timeout", tc.name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("a blocked %s ignored a deadline set after it began", tc.name)
		}
	}
}

// A connection past MaxConnDuration gives up its slot, so the next request
// dials a replacement instead of failing with ErrNoFreeConns.
func TestClientReplacesExpiredConnection(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
	testServer := newTestServer(t, server, ServerConfig{})
	var dials atomic.Int64
	hc := &fasthttp.HostClient{
		Addr:            testServer.listener.Addr().String(),
		MaxConns:        1,
		MaxConnDuration: 50 * time.Millisecond,
		Dial: func(addr string) (net.Conn, error) {
			dials.Add(1)
			return net.Dial("tcp", addr)
		},
	}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	for i := range 2 {
		var req fasthttp.Request
		var resp fasthttp.Response
		req.SetRequestURI(testServer.URL("/"))
		if err := hc.DoTimeout(&req, &resp, 2*time.Second); err != nil {
			t.Fatalf("request %d error: %v", i, err)
		}
		time.Sleep(80 * time.Millisecond)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials = %d, want a replacement for the expired connection", got)
	}
}

// runPushPromise feeds one PUSH_PROMISE for authority on parentID and returns
// the result, how often the handler was asked, and the frames written back.
func runPushPromise(t *testing.T, parentID uint32, parentURI, authority string) (int64, []xhttp2.Frame, error) {
	t.Helper()
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", authority}, {":path", "/pushed"}} {
		if err := encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			t.Fatal(err)
		}
	}
	var wire, out bytes.Buffer
	if err := xhttp2.NewFramer(&wire, nil).WritePushPromise(xhttp2.PushPromiseParam{
		StreamID:      parentID,
		PromiseID:     4,
		BlockFragment: block.Bytes(),
		EndHeaders:    true,
	}); err != nil {
		t.Fatal(err)
	}
	parent := fasthttp.AcquireRequest()
	t.Cleanup(func() { fasthttp.ReleaseRequest(parent) })
	parent.SetRequestURI(parentURI)
	pushes := &rejectingPushHandler{}
	writer := bufio.NewWriter(&out)
	conn := &clientConn{
		writeSem:       make(chan struct{}, 1),
		config:         clientConfig{maxHeaderListSize: 64 << 10, maxConcurrentStreams: 8, pushHandler: pushes},
		framer:         xhttp2.NewFramer(writer, &wire),
		bufferedWriter: writer,
		headerDecoder:  newHeaderCodec(defaultHeaderTableSize, 64<<10),
		streams:        map[uint32]*clientStream{parentID: {id: parentID, parentRequest: parent, isPush: parentID&1 == 0}},
	}
	installTestWriter(t, conn)
	frame, err := conn.framer.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	err = conn.processPushPromise(frame.(*xhttp2.PushPromiseFrame)) //nolint:forcetypeassert
	// Resets are written by the sender.
	drainSender(t, conn)
	var written []xhttp2.Frame
	reader := xhttp2.NewFramer(nil, &out)
	for {
		frame, readErr := reader.ReadFrame()
		if readErr != nil {
			break
		}
		written = append(written, frame)
	}
	return pushes.declined.Load(), written, err
}

func TestPushPromiseForAnotherOriginIsRefused(t *testing.T) {
	asked, written, err := runPushPromise(t, 1, "http://example.com/", "victim.example")
	if err != nil {
		t.Fatalf("processPushPromise() error: %v", err)
	}
	if asked != 0 {
		t.Fatal("a cross-origin push reached the push handler")
	}
	if len(written) != 1 {
		t.Fatalf("wrote %d frames, want one RST_STREAM", len(written))
	}
	if reset, ok := written[0].(*xhttp2.RSTStreamFrame); !ok || reset.StreamID != 4 || reset.ErrCode != xhttp2.ErrCodeProtocol {
		t.Fatalf("wrote %v, want RST_STREAM(4, PROTOCOL_ERROR)", written[0])
	}

	if asked, _, _ := runPushPromise(t, 1, "http://example.com/", "Example.com"); asked != 1 {
		t.Fatal("a same-origin push didn't reach the push handler")
	}
}

func TestPushPromiseOnPushedStreamFailsConnection(t *testing.T) {
	asked, _, err := runPushPromise(t, 2, "http://example.com/", "example.com")
	if connErr, ok := errors.AsType[xhttp2.ConnectionError](err); !ok || xhttp2.ErrCode(connErr) != xhttp2.ErrCodeProtocol {
		t.Fatalf("processPushPromise() error = %v, want PROTOCOL_ERROR connection error", err)
	}
	if asked != 0 {
		t.Fatal("a promise on a pushed stream reached the push handler")
	}
}

// A connection that negotiated neither h2 nor HTTP/1.1 is not spoken to.
func TestClientRejectsOtherNegotiatedProtocol(t *testing.T) {
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
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"custom"},
		}))
	}()
	t.Cleanup(func() {
		_ = server.Shutdown()
		<-done
	})
	hc := &fasthttp.HostClient{
		Addr:      listener.Addr().String(),
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"custom"}},
	}
	if err := ConfigureHostClient(hc, ClientConfig{}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	var req fasthttp.Request
	var resp fasthttp.Response
	req.SetRequestURI("https://" + listener.Addr().String() + "/")
	if err := hc.DoTimeout(&req, &resp, 2*time.Second); !errors.Is(err, errUnsupportedProtocol) {
		t.Fatalf("Do() error = %v, want %v", err, errUnsupportedProtocol)
	}
}

// A write cut short by its deadline reports the bytes that did go out.
func TestOpenStreamWriteReportsPartialProgress(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			_ = ctx.AcceptStream(func(fasthttp.StreamConn) { <-release })
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
	if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, EnableExtendedConnect: true}); err != nil {
		t.Fatalf("ConfigureHostClient() error: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	var req fasthttp.Request
	var resp fasthttp.Response
	req.Header.SetMethod(fasthttp.MethodConnect)
	req.Header.SetConnectProtocol("websocket")
	req.SetRequestURI(testServer.URL("/ws"))
	conn, err := hc.OpenStream(&req, &resp)
	if err != nil {
		t.Fatalf("OpenStream() error: %v", err)
	}
	defer conn.Close()
	payload := make([]byte, 8<<20)
	_ = conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	n, err := conn.Write(payload)
	if !isTimeout(err) {
		t.Fatalf("Write() error = %v, want timeout", err)
	}
	if n <= 0 || n >= len(payload) {
		t.Fatalf("Write() = %d, want the bytes the flow-control window let through", n)
	}
}

// clientPeer is a hand-driven HTTP/2 server connection for client tests. Its
// reader acknowledges SETTINGS, and PING unless noPingAck is set, and
// delivers every other frame on frames.
type clientPeer struct {
	t         testing.TB
	conn      net.Conn
	wmu       sync.Mutex
	framer    *xhttp2.Framer
	encoder   *hpack.Encoder
	block     bytes.Buffer
	frames    chan peerFrame
	noPingAck atomic.Bool
	pingSeq   uint64
	// deferSettingsAck leaves the client's SETTINGS unacknowledged until
	// ackSettings.
	deferSettingsAck bool
}

type peerFrame struct {
	typ          xhttp2.FrameType
	streamID     uint32
	endStream    bool
	ack          bool
	errCode      xhttp2.ErrCode
	increment    uint32
	lastStreamID uint32
	data         []byte
	fields       []hpack.HeaderField
}

func setAcceptDeadline(ln net.Listener, deadline time.Time) {
	if tcp, ok := ln.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(deadline)
	}
}

func listenPeer(t testing.TB) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// acceptPeer accepts one prior-knowledge client connection and sends settings.
func acceptPeer(t testing.TB, ln net.Listener, settings ...xhttp2.Setting) *clientPeer {
	t.Helper()
	return startPeer(t, ln, false, settings...)
}

func startPeer(t testing.TB, ln net.Listener, deferSettingsAck bool, settings ...xhttp2.Setting) *clientPeer {
	t.Helper()
	setAcceptDeadline(ln, time.Now().Add(5*time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	preface := make([]byte, len(clientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		t.Fatalf("reading preface: %v", err)
	}
	peer := &clientPeer{
		t:                t,
		conn:             conn,
		framer:           xhttp2.NewFramer(conn, conn),
		frames:           make(chan peerFrame, 4096),
		deferSettingsAck: deferSettingsAck,
	}
	peer.framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
	peer.encoder = hpack.NewEncoder(&peer.block)
	peer.write(func(framer *xhttp2.Framer) error { return framer.WriteSettings(settings...) })
	go peer.readLoop()
	return peer
}

func (p *clientPeer) readLoop() {
	defer close(p.frames)
	for {
		frame, err := p.framer.ReadFrame()
		if err != nil {
			return
		}
		header := frame.Header()
		copied := peerFrame{typ: header.Type, streamID: header.StreamID}
		switch frame := frame.(type) {
		case *xhttp2.SettingsFrame:
			if !frame.IsAck() && !p.deferSettingsAck {
				p.ackSettings()
			}
			continue
		case *xhttp2.MetaHeadersFrame:
			copied.endStream = frame.StreamEnded()
			copied.fields = append([]hpack.HeaderField(nil), frame.Fields...)
		case *xhttp2.DataFrame:
			copied.endStream = frame.StreamEnded()
			copied.data = bytes.Clone(frame.Data())
		case *xhttp2.RSTStreamFrame:
			copied.errCode = frame.ErrCode
		case *xhttp2.WindowUpdateFrame:
			copied.increment = frame.Increment
		case *xhttp2.GoAwayFrame:
			copied.errCode = frame.ErrCode
			copied.lastStreamID = frame.LastStreamID
		case *xhttp2.PingFrame:
			copied.ack = frame.IsAck()
			copied.data = bytes.Clone(frame.Data[:])
		}
		// Delivered before the ack, so a test may hold the write lock to
		// finish a frame of its own first.
		p.frames <- copied
		if ping, ok := frame.(*xhttp2.PingFrame); ok && !ping.IsAck() && !p.noPingAck.Load() {
			data := ping.Data
			p.write(func(framer *xhttp2.Framer) error { return framer.WritePing(true, data) })
		}
	}
}

func (p *clientPeer) ackSettings() {
	p.write(func(framer *xhttp2.Framer) error { return framer.WriteSettingsAck() })
}

func (p *clientPeer) write(write func(*xhttp2.Framer) error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = write(p.framer)
}

func (p *clientPeer) encode(fields ...string) []byte {
	p.block.Reset()
	for i := 0; i+1 < len(fields); i += 2 {
		_ = p.encoder.WriteField(hpack.HeaderField{Name: fields[i], Value: fields[i+1]})
	}
	return bytes.Clone(p.block.Bytes())
}

func (p *clientPeer) writeHeaders(streamID uint32, endStream bool, fields ...string) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: p.encode(fields...),
		EndStream:     endStream,
		EndHeaders:    true,
	})
}

func (p *clientPeer) writeData(streamID uint32, endStream bool, data []byte) {
	p.write(func(framer *xhttp2.Framer) error { return framer.WriteData(streamID, endStream, data) })
}

// next returns the next frame the client sent.
func (p *clientPeer) next() peerFrame {
	p.t.Helper()
	select {
	case frame, ok := <-p.frames:
		if !ok {
			p.t.Fatal("client connection closed")
		}
		return frame
	case <-time.After(5 * time.Second):
		p.t.Fatal("timed out waiting for a client frame")
	}
	return peerFrame{}
}

// until returns the frames up to and including the first that matches.
func (p *clientPeer) until(match func(peerFrame) bool) []peerFrame {
	p.t.Helper()
	var frames []peerFrame
	for {
		frame := p.next()
		frames = append(frames, frame)
		if match(frame) {
			return frames
		}
	}
}

func (p *clientPeer) nextHeaders() peerFrame {
	p.t.Helper()
	frames := p.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameHeaders })
	return frames[len(frames)-1]
}

// sync PINGs the client and returns the frames it sent before the ack: the
// client acks after it handled, and answered, every earlier frame.
func (p *clientPeer) sync() []peerFrame {
	p.t.Helper()
	p.pingSeq++
	var data [8]byte
	data[0] = 's'
	data[7] = byte(p.pingSeq)
	p.write(func(framer *xhttp2.Framer) error { return framer.WritePing(false, data) })
	frames := p.until(func(frame peerFrame) bool {
		return frame.typ == xhttp2.FramePing && frame.ack && bytes.Equal(frame.data, data[:])
	})
	return frames[:len(frames)-1]
}

func (f peerFrame) header(name string) string {
	for _, field := range f.fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

func countResets(frames []peerFrame, streamID uint32) int {
	resets := 0
	for _, frame := range frames {
		if frame.typ == xhttp2.FrameRSTStream && frame.streamID == streamID {
			resets++
		}
	}
	return resets
}

func newPeerClient(t testing.TB, ln net.Listener, config *ClientConfig) (*fasthttp.HostClient, *Transport) {
	t.Helper()
	config.Mode = PriorKnowledge
	hc := &fasthttp.HostClient{Addr: ln.Addr().String()}
	transport := NewTransport(*config)
	if err := configureHostClientTransport(hc, transport); err != nil {
		t.Fatalf("configuring the client: %v", err)
	}
	t.Cleanup(hc.CloseIdleConnections)
	return hc, transport
}

// poolCount reports the pools transport holds.
func (t *Transport) poolCount() int {
	n := 0
	t.pools.Range(func(any, any) bool {
		n++
		return true
	})
	return n
}

func poolConns(transport *Transport, hc *fasthttp.HostClient) []*clientConn {
	value, ok := transport.pools.Load(hc)
	if !ok {
		return nil
	}
	pool := value.(*clientPool) //nolint:forcetypeassert
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return append([]*clientConn(nil), pool.conns...)
}

// doAsync runs Do on a fresh request for path and reports its error.
func doAsync(hc *fasthttp.HostClient, method, path string, body []byte, resp *fasthttp.Response) <-chan error {
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(method)
		req.SetRequestURI("http://" + hc.Addr + path)
		if body != nil {
			req.SetBody(body)
		}
		if resp == nil {
			resp = fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
		}
		done <- hc.Do(req, resp)
	}()
	return done
}

func waitResultOf(t testing.TB, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("request didn't finish")
	}
	return nil
}

func waitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// RFC 9113 §5.1: frames on a stream the client reset are ignored, not
// answered; their flow still counts against the connection window and their
// header blocks against HPACK state.
func TestClientIgnoresFramesOnStreamItReset(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.StreamResponseBody = true
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/download", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if err := resp.CloseBodyStream(); err != nil {
		t.Fatalf("CloseBodyStream() error: %v", err)
	}
	peer.until(func(frame peerFrame) bool {
		return frame.typ == xhttp2.FrameRSTStream && frame.streamID == request.streamID
	})

	// What the server had in flight when the reset crossed it.
	const inFlight, frameSize = 20, 1000
	for range inFlight {
		peer.writeData(request.streamID, false, bytes.Repeat([]byte{'d'}, frameSize))
	}
	peer.writeHeaders(request.streamID, true, "x-late", "late")
	frames := peer.sync()
	if resets := countResets(frames, request.streamID); resets != 0 {
		t.Fatalf("client answered late frames on the stream it reset with %d RST_STREAM frames", resets)
	}
	// The credit sender frames the credit; the PING ACK may overtake it.
	credit := 0
	countCredit := func(frame peerFrame) {
		if frame.typ == xhttp2.FrameWindowUpdate && frame.streamID == 0 {
			credit += int(frame.increment)
		}
	}
	for _, frame := range frames {
		countCredit(frame)
	}
	for credit < inFlight*frameSize {
		countCredit(peer.next())
	}
	if credit != inFlight*frameSize {
		t.Fatalf("connection credit returned for ignored DATA = %d, want %d", credit, inFlight*frameSize)
	}

	// The ignored trailers' x-late entry is in the peer's dynamic table; the
	// next response refers to it by index.
	next := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(next)
	hc.StreamResponseBody = false
	done = doAsync(hc, fasthttp.MethodGet, "/next", nil, next)
	request = peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200", "x-late", "late")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() after ignored frames: %v", err)
	}
	if got := string(next.Header.Peek("x-late")); got != "late" {
		t.Fatalf("x-late = %q: HPACK state diverged", got)
	}
}

// A late response to a timed-out request is ignored like any frame on a
// stream the client reset.
func TestClientIgnoresLateResponseAfterTimeout(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + hc.Addr + "/slow")
		done <- hc.DoTimeout(req, resp, 100*time.Millisecond)
	}()
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	if err := waitResultOf(t, done); !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("DoTimeout() error = %v, want timeout", err)
	}
	peer.until(func(frame peerFrame) bool {
		return frame.typ == xhttp2.FrameRSTStream && frame.streamID == request.streamID
	})
	peer.writeHeaders(request.streamID, false, ":status", "200")
	for range 4 {
		peer.writeData(request.streamID, false, []byte("late"))
	}
	if resets := countResets(peer.sync(), request.streamID); resets != 0 {
		t.Fatalf("client answered a late response with %d RST_STREAM frames", resets)
	}
}

// RFC 9113 §5.4.2: an endpoint must not answer RST_STREAM with RST_STREAM,
// here while the request body is still being uploaded.
func TestClientDoesNotAnswerPeerResetWithReset(t *testing.T) {
	for _, code := range []xhttp2.ErrCode{xhttp2.ErrCodeNo, xhttp2.ErrCodeRefusedStream, xhttp2.ErrCodeCancel} {
		t.Run(code.String(), func(t *testing.T) {
			ln := listenPeer(t)
			hc, _ := newPeerClient(t, ln, &ClientConfig{})
			body, bodyWriter := io.Pipe()
			defer bodyWriter.Close()
			done := make(chan error, 1)
			go func() {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(resp)
				req.Header.SetMethod(fasthttp.MethodPost)
				req.SetRequestURI("http://" + hc.Addr + "/upload")
				req.SetBodyStream(body, -1)
				done <- hc.Do(req, resp)
			}()
			go func() { _, _ = bodyWriter.Write([]byte("first chunk")) }()
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			peer.until(func(frame peerFrame) bool {
				return frame.typ == xhttp2.FrameData && frame.streamID == request.streamID
			})
			if code == xhttp2.ErrCodeNo {
				// RFC 9113 §8.1: a complete response, then a request to stop
				// the upload.
				peer.writeHeaders(request.streamID, true, ":status", "200")
			}
			peer.write(func(framer *xhttp2.Framer) error { return framer.WriteRSTStream(request.streamID, code) })
			peer.sync()

			// The uploader produces more and only now finds the stream gone.
			go func() { _, _ = bodyWriter.Write([]byte("second chunk")) }()
			err := waitResultOf(t, done)
			if code == xhttp2.ErrCodeNo && err != nil {
				t.Fatalf("Do() discarded a complete response: %v", err)
			}
			if code != xhttp2.ErrCodeNo && err == nil {
				t.Fatal("Do() succeeded on a reset stream")
			}
			if resets := countResets(peer.sync(), request.streamID); resets != 0 {
				t.Fatalf("client answered RST_STREAM(%v) with %d RST_STREAM frames", code, resets)
			}
		})
	}
}

// RFC 9113 §5.1: no RST_STREAM on a stream both sides closed with END_STREAM,
// which is where a streamed response sits while its body is unread.
func TestClientNoResetOnStreamClosedInBothDirections(t *testing.T) {
	for _, timeout := range []string{"request deadline", "read timeout"} {
		t.Run(timeout, func(t *testing.T) {
			ln := listenPeer(t)
			hc, transport := newPeerClient(t, ln, &ClientConfig{})
			hc.StreamResponseBody = true
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			done := make(chan error, 1)
			go func() {
				req := fasthttp.AcquireRequest()
				defer fasthttp.ReleaseRequest(req)
				req.SetRequestURI("http://" + hc.Addr + "/")
				if timeout == "read timeout" {
					hc.ReadTimeout = 100 * time.Millisecond
					done <- hc.Do(req, resp)
					return
				}
				done <- hc.DoTimeout(req, resp, 100*time.Millisecond)
			}()
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			if !request.endStream {
				t.Fatal("request HEADERS without END_STREAM")
			}
			peer.writeHeaders(request.streamID, false, ":status", "200", "content-length", "5")
			peer.writeData(request.streamID, true, []byte("hello"))
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("Do() error: %v", err)
			}
			// The timer retires the stream while its body is still unread.
			conn := poolConns(transport, hc)[0]
			waitUntil(t, "the timer retired the stream", func() bool {
				conn.mu.Lock()
				defer conn.mu.Unlock()
				return len(conn.streams) == 0
			})
			if resets := countResets(peer.sync(), request.streamID); resets != 0 {
				t.Fatalf("client sent RST_STREAM on a stream closed in both directions")
			}
			body, err := io.ReadAll(resp.BodyStream())
			if err != nil || string(body) != "hello" {
				t.Fatalf("body = %q, %v", body, err)
			}
			_ = resp.CloseBodyStream()
		})
	}
}

// gatedConn holds every write until gate closes, reporting the first on
// entered.
type gatedConn struct {
	net.Conn

	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.gate
	return c.Conn.Write(p)
}

// newGatedClientConn returns a client connection whose writer is parked with
// a full queue, so the next frame a producer flushes stays in its hands until
// open. finish drains the connection and returns its HEADERS and RST_STREAM
// frames.
func newGatedClientConn(t *testing.T) (conn *clientConn, open func(), finish func() []xhttp2.Frame) {
	t.Helper()
	local, remote := net.Pipe()
	var wire bytes.Buffer
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&wire, remote)
		close(copied)
	}()
	gated := &gatedConn{Conn: local, gate: make(chan struct{}), entered: make(chan struct{})}
	conn = &clientConn{
		pool:    &clientPool{},
		hc:      &fasthttp.HostClient{},
		conn:    gated,
		streams: make(map[uint32]*clientStream),
		connFlowState: connFlowState{
			peerMaxFrameSize:        defaultMaxFrameSize,
			peerMaxHeaderListSize:   1<<32 - 1,
			peerInitialStreamWindow: 65535,
			send:                    sendWindow{window: 65535},
			recv:                    recvWindow{window: 65535},
		},
		nextStreamID: 1,
		notify:       make(chan struct{}),
	}
	installTestWriter(t, conn)
	conn.initHeaderEncoder(defaultHeaderTableSize)
	ping := func() {
		if err := conn.writeControl(func() error { return conn.framer.WritePing(false, [8]byte{}) }); err != nil {
			t.Fatalf("writeControl() error: %v", err)
		}
	}
	ping()
	<-gated.entered
	for len(conn.writer.queue) < cap(conn.writer.queue) {
		ping()
	}
	open = func() { close(gated.gate) }
	finish = func() []xhttp2.Frame {
		drainSender(t, conn)
		conn.shutdownWriter(nil, true)
		_ = local.Close()
		<-copied
		var frames []xhttp2.Frame
		reader := xhttp2.NewFramer(nil, &wire)
		for {
			frame, err := reader.ReadFrame()
			if err != nil {
				return frames
			}
			switch frame := frame.(type) {
			case *xhttp2.HeadersFrame:
				copied := *frame
				frames = append(frames, &copied)
			case *xhttp2.RSTStreamFrame:
				copied := *frame
				frames = append(frames, &copied)
			}
		}
	}
	return conn, open, finish
}

// A stream canceled after its HEADERS were committed, while they were still
// on their way to the wire, must be reset once they are out.
func TestClientCancelAfterHeadersCommitResets(t *testing.T) {
	conn, open, finish := newGatedClientConn(t)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI("http://example.com/")
	stream := &clientStream{conn: conn, req: req, result: make(chan clientResult, 1), expectedResponseBytes: -1}
	// One more than the test's stream, so finalizing it arms no idle timer
	// on a connection without a lease.
	conn.activeStreams = 2
	written := make(chan error, 1)
	go func() { written <- conn.writeRequestHeaders(stream, true, req, nil, time.Time{}) }()
	waitUntil(t, "the stream was registered", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.streams[1] == stream
	})
	canceled := make(chan struct{})
	go func() {
		conn.expireRequestDeadline(stream)
		close(canceled)
	}()
	waitUntil(t, "the stream was canceled", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return stream.err != nil
	})
	open()
	if err := waitResultOf(t, written); err != nil {
		t.Fatalf("writeRequestHeaders() error: %v", err)
	}
	<-canceled
	headers, reset := -1, -1
	for i, frame := range finish() {
		switch frame := frame.(type) {
		case *xhttp2.HeadersFrame:
			if frame.StreamID == 1 {
				headers = i
			}
		case *xhttp2.RSTStreamFrame:
			if frame.StreamID == 1 {
				reset = i
			}
		}
	}
	if headers < 0 || reset < headers {
		t.Fatalf("HEADERS at %d, RST_STREAM at %d: HEADERS reached the wire with no reset after them", headers, reset)
	}
}

// A stream canceled while its block was encoded is already finalized; the
// block still goes out to keep HPACK in sync, followed by RST_STREAM.
func TestClientCommitAfterCancelWritesHeadersThenReset(t *testing.T) {
	conn, open, finish := newGatedClientConn(t)
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	req.SetRequestURI("http://example.com/")
	stream := &clientStream{conn: conn, req: req, result: make(chan clientResult, 1), expectedResponseBytes: -1}
	// One more than the test's stream, so finalizing it arms no idle timer
	// on a connection without a lease.
	conn.activeStreams = 2
	conn.expireRequestDeadline(stream)
	if !stream.lifecycleReleased {
		t.Fatal("canceled stream wasn't finalized")
	}
	block, err := conn.encodeRequestHeaders(req, nil, math.MaxUint32, false)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		if err := conn.lockWrite(time.Time{}, nil); err != nil {
			written <- err
			return
		}
		written <- conn.commitRequestHeaders(stream, true, block)
	}()
	waitUntil(t, "the block was committed", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.nextStreamID == 3
	})
	open()
	if err := waitResultOf(t, written); !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("commitRequestHeaders() error = %v, want the cancel's", err)
	}
	frames := finish()
	conn.mu.Lock()
	registered, nextID := len(conn.streams), conn.nextStreamID
	conn.mu.Unlock()
	if registered != 0 || nextID != 3 {
		t.Fatalf("streams = %d, nextStreamID = %d; want 0 and 3", registered, nextID)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want HEADERS then RST_STREAM", len(frames))
	}
	headers, ok := frames[0].(*xhttp2.HeadersFrame)
	reset, ok2 := frames[1].(*xhttp2.RSTStreamFrame)
	if !ok || !ok2 || headers.StreamID != 1 || reset.StreamID != 1 || reset.ErrCode != xhttp2.ErrCodeCancel {
		t.Fatalf("frames = %#v, %#v", frames[0], frames[1])
	}
}

// ReadIdleTimeout must never cut a frame read short: a frame that arrives
// slower than the timeout is still parsed whole.
func TestClientReadIdleTimeoutDoesNotInterruptFrame(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{
		ReadIdleTimeout: 50 * time.Millisecond,
		PingTimeout:     5 * time.Second,
	})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")

	// The body hides what looks like a DATA header ending the stream, which a
	// reader resuming mid-payload would parse.
	payload := bytes.Repeat([]byte{'x'}, 300)
	var fake bytes.Buffer
	if err := xhttp2.NewFramer(&fake, nil).WriteData(request.streamID, true, bytes.Repeat([]byte{'y'}, 691)); err != nil {
		t.Fatal(err)
	}
	payload = append(payload, fake.Bytes()...)
	var frame bytes.Buffer
	if err := xhttp2.NewFramer(&frame, nil).WriteData(request.streamID, true, payload); err != nil {
		t.Fatal(err)
	}
	raw := frame.Bytes()
	peer.wmu.Lock()
	_, _ = peer.conn.Write(raw[:9+300])
	// The idle check fires in the middle of the frame.
	peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FramePing && !frame.ack })
	_, _ = peer.conn.Write(raw[9+300:])
	peer.wmu.Unlock()

	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !bytes.Equal(resp.Body(), payload) {
		t.Fatalf("body = %d bytes, want the %d the server sent", len(resp.Body()), len(payload))
	}
}

// An unanswered health-check PING fails the connection after PingTimeout.
func TestClientHealthCheckFailsConnectionOnUnansweredPing(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{
		ReadIdleTimeout: 20 * time.Millisecond,
		PingTimeout:     50 * time.Millisecond,
	})
	// POST, so the request isn't retried on a new connection.
	done := doAsync(hc, fasthttp.MethodPost, "/", nil, nil)
	peer := acceptPeer(t, ln)
	peer.noPingAck.Store(true)
	peer.nextHeaders()
	peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FramePing && !frame.ack })
	if err := waitResultOf(t, done); !errors.Is(err, errPingTimeout) {
		t.Fatalf("Do() error = %v, want %v", err, errPingTimeout)
	}
	waitUntil(t, "the connection left the pool", func() bool { return len(poolConns(transport, hc)) == 0 })
}

// A request that reached the peer before the connection dropped follows
// HostClient's retry policy: an idempotent one is retried.
func TestClientRetriesIdempotentRequestLostWithConnection(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	first := acceptPeer(t, ln)
	first.nextHeaders()
	_ = first.conn.Close() // no GOAWAY, no response
	second := acceptPeer(t, ln)
	request := second.nextHeaders()
	second.writeHeaders(request.streamID, false, ":status", "200")
	second.writeData(request.streamID, true, []byte("retried"))
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if string(resp.Body()) != "retried" {
		t.Fatalf("body = %q", resp.Body())
	}
}

// The same loss is final for a non-idempotent request the peer may have
// processed.
func TestClientDoesNotRetryNonIdempotentRequestLostWithConnection(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	done := doAsync(hc, fasthttp.MethodPost, "/", []byte("body"), nil)
	first := acceptPeer(t, ln)
	first.nextHeaders()
	_ = first.conn.Close()
	if err := waitResultOf(t, done); err == nil {
		t.Fatal("Do() succeeded without a response")
	}
	setAcceptDeadline(ln, time.Now())
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("a POST the peer may have processed was retried")
	}
}

// A request whose HEADERS never reached a connection that failed is offered
// to HostClient's retry policy: idempotent methods, or RetryIfErr.
func TestClientRetriesUnsentRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		retryIf fasthttp.RetryIfErrFunc
		retried bool
	}{
		{name: "GET", method: fasthttp.MethodGet, retried: true},
		{name: "POST", method: fasthttp.MethodPost, retried: false},
		{
			name:    "POST with RetryIfErr",
			method:  fasthttp.MethodPost,
			retryIf: func(*fasthttp.Request, int, error) (bool, bool) { return false, true },
			retried: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			hc, transport := newPeerClient(t, ln, &ClientConfig{})
			hc.RetryIfErr = tc.retryIf
			done := doAsync(hc, fasthttp.MethodGet, "/warmup", nil, nil)
			first := acceptPeer(t, ln)
			request := first.nextHeaders()
			first.writeHeaders(request.streamID, true, ":status", "200")
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("warm-up Do() error: %v", err)
			}
			conn := poolConns(transport, hc)[0]

			// The request gets a stream but not the write slot.
			conn.writeSem <- struct{}{}
			done = doAsync(hc, tc.method, "/unsent", []byte("payload"), nil)
			waitUntil(t, "the request reserved a stream", func() bool {
				conn.mu.Lock()
				defer conn.mu.Unlock()
				return conn.activeStreams == 1
			})
			_ = first.conn.Close()
			waitUntil(t, "the connection failed", func() bool {
				conn.mu.Lock()
				defer conn.mu.Unlock()
				return conn.closed
			})
			<-conn.writeSem

			if !tc.retried {
				if err := waitResultOf(t, done); err == nil {
					t.Fatal("Do() succeeded without a connection")
				}
				setAcceptDeadline(ln, time.Now())
				if conn, err := ln.Accept(); err == nil {
					_ = conn.Close()
					t.Fatal("request retried against HostClient's policy")
				}
				return
			}
			second := acceptPeer(t, ln)
			request = second.nextHeaders()
			if method := request.header(":method"); method != tc.method {
				t.Fatalf(":method = %q", method)
			}
			second.writeHeaders(request.streamID, true, ":status", "200")
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("unsent request wasn't retried: %v", err)
			}
		})
	}
}

// A request REFUSED_STREAM or GOAWAY kept from being processed reports a
// retryable ErrRefusedStream or ErrConnectionDraining, and HostClient's policy
// decides within its one attempt budget.
func TestClientRefusedRequestFollowsRetryPolicy(t *testing.T) {
	refuse := func(peer *clientPeer, streamID uint32) {
		peer.write(func(framer *xhttp2.Framer) error {
			return framer.WriteRSTStream(streamID, xhttp2.ErrCodeRefusedStream)
		})
	}
	retryRefused := func(_ *fasthttp.Request, _ int, err error) (bool, bool) {
		return false, errors.Is(err, ErrRefusedStream) || errors.Is(err, ErrConnectionDraining)
	}
	t.Run("POST not retried by default", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		done := doAsync(hc, fasthttp.MethodPost, "/", []byte("payload"), nil)
		peer := acceptPeer(t, ln)
		refuse(peer, peer.nextHeaders().streamID)
		if err := waitResultOf(t, done); !errors.Is(err, ErrRefusedStream) {
			t.Fatalf("Do() error = %v, want ErrRefusedStream", err)
		}
		for _, frame := range peer.sync() {
			if frame.typ == xhttp2.FrameHeaders {
				t.Fatal("refused POST was retried by default")
			}
		}
	})
	t.Run("POST retried by RetryIfErr after REFUSED_STREAM", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		hc.RetryIfErr = retryRefused
		done := doAsync(hc, fasthttp.MethodPost, "/", []byte("payload"), nil)
		peer := acceptPeer(t, ln)
		refused := peer.nextHeaders()
		refuse(peer, refused.streamID)
		retried := peer.nextHeaders()
		if retried.streamID == refused.streamID {
			t.Fatal("retry reused the refused stream")
		}
		peer.writeHeaders(retried.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
	})
	t.Run("POST retried by RetryIfErr after GOAWAY", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		hc.RetryIfErr = retryRefused
		done := doAsync(hc, fasthttp.MethodPost, "/", []byte("payload"), nil)
		first := acceptPeer(t, ln)
		first.nextHeaders()
		first.write(func(framer *xhttp2.Framer) error { return framer.WriteGoAway(0, xhttp2.ErrCodeNo, nil) })
		second := acceptPeer(t, ln)
		request := second.nextHeaders()
		second.writeHeaders(request.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
	})
	t.Run("attempts bounded by MaxIdemponentCallAttempts", func(t *testing.T) {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		hc.RetryIfErr = retryRefused
		hc.MaxIdemponentCallAttempts = 3
		done := doAsync(hc, fasthttp.MethodPost, "/", []byte("payload"), nil)
		peer := acceptPeer(t, ln)
		for range hc.MaxIdemponentCallAttempts {
			refuse(peer, peer.nextHeaders().streamID)
		}
		if err := waitResultOf(t, done); !errors.Is(err, ErrRefusedStream) {
			t.Fatalf("Do() error = %v, want ErrRefusedStream", err)
		}
		for _, frame := range peer.sync() {
			if frame.typ == xhttp2.FrameHeaders {
				t.Fatal("request was sent more than MaxIdemponentCallAttempts times")
			}
		}
	})
}

// A body stream that went out in part can't be replayed, refused or not.
func TestClientDoesNotReplayStartedBodyStream(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(fasthttp.MethodPut)
		req.SetRequestURI("http://" + hc.Addr + "/")
		req.SetBodyStream(io.MultiReader(strings.NewReader("streamed body")), -1)
		done <- hc.Do(req, nil)
	}()
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameData && frame.endStream })
	peer.write(func(framer *xhttp2.Framer) error {
		return framer.WriteRSTStream(request.streamID, xhttp2.ErrCodeRefusedStream)
	})
	if err := waitResultOf(t, done); !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("Do() error = %v, want ErrRefusedStream", err)
	}
	for _, frame := range peer.sync() {
		if frame.typ == xhttp2.FrameHeaders {
			t.Fatal("a consumed body stream was replayed")
		}
	}
}

func clientPoolFor(t testing.TB, transport *Transport, hc *fasthttp.HostClient) *clientPool {
	t.Helper()
	value, ok := transport.pools.Load(hc)
	if !ok {
		t.Fatal("no pool for the HostClient")
	}
	return value.(*clientPool) //nolint:forcetypeassert
}

// Requests waiting under MaxConns take capacity a peer's SETTINGS frame adds.
func TestMaxConnsWaitersWakeWhenPeerRaisesStreamLimit(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = 3 * time.Second
	held := doAsync(hc, fasthttp.MethodGet, "/held", nil, nil)
	peer := acceptPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingMaxConcurrentStreams, Val: 1})
	heldRequest := peer.nextHeaders()
	conn := poolConns(transport, hc)[0]
	waitUntil(t, "the client applied the stream limit", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.peerMaxConcurrentStreams == 1
	})
	waiting := []<-chan error{
		doAsync(hc, fasthttp.MethodGet, "/w0", nil, nil),
		doAsync(hc, fasthttp.MethodGet, "/w1", nil, nil),
	}
	pool := clientPoolFor(t, transport, hc)
	waitUntil(t, "both requests wait", func() bool { return pool.waiters.Load() == 2 })

	peer.write(func(framer *xhttp2.Framer) error {
		return framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingMaxConcurrentStreams, Val: 10})
	})
	for range waiting {
		request := peer.nextHeaders()
		peer.writeHeaders(request.streamID, true, ":status", "200")
	}
	for _, done := range waiting {
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("waiter error: %v", err)
		}
	}
	peer.writeHeaders(heldRequest.streamID, true, ":status", "200")
	if err := waitResultOf(t, held); err != nil {
		t.Fatalf("held request error: %v", err)
	}
}

// Every waiter sees the capacity of a connection dialed after a drained one
// closed, not just the one that dialed it.
func TestMaxConnsWaitersAllWakeAfterRedial(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = 3 * time.Second
	held := doAsync(hc, fasthttp.MethodGet, "/held", nil, nil)
	first := acceptPeer(t, ln)
	heldRequest := first.nextHeaders()
	first.write(func(framer *xhttp2.Framer) error {
		return framer.WriteGoAway(heldRequest.streamID, xhttp2.ErrCodeNo, nil)
	})
	conn := poolConns(transport, hc)[0]
	waitUntil(t, "the client saw GOAWAY", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.goAway
	})
	waiting := []<-chan error{
		doAsync(hc, fasthttp.MethodGet, "/w0", nil, nil),
		doAsync(hc, fasthttp.MethodGet, "/w1", nil, nil),
	}
	pool := clientPoolFor(t, transport, hc)
	waitUntil(t, "both requests wait", func() bool { return pool.waiters.Load() == 2 })

	first.writeHeaders(heldRequest.streamID, true, ":status", "200")
	if err := waitResultOf(t, held); err != nil {
		t.Fatalf("held request error: %v", err)
	}
	second := acceptPeer(t, ln)
	// Both reach the new connection before either is answered.
	requests := []peerFrame{second.nextHeaders(), second.nextHeaders()}
	for _, request := range requests {
		second.writeHeaders(request.streamID, true, ":status", "200")
	}
	for _, done := range waiting {
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("waiter error: %v", err)
		}
	}
}

// RFC 9113 §6.9 and §6.9.1: a zero increment, or an overflow, on a stream's
// WINDOW_UPDATE is a stream error, and the connection's other streams live on.
func TestClientInvalidStreamWindowUpdateResetsOnlyItsStream(t *testing.T) {
	for _, tc := range []struct {
		name      string
		increment uint32
		code      xhttp2.ErrCode
	}{
		{"zero increment", 0, xhttp2.ErrCodeProtocol},
		{"overflow", 1<<31 - 1, xhttp2.ErrCodeFlowControl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			hc, _ := newPeerClient(t, ln, &ClientConfig{})
			// The peer's bad frame is a retryable failure; one attempt
			// keeps the bad request from coming back.
			hc.MaxIdemponentCallAttempts = 1
			bad := doAsync(hc, fasthttp.MethodGet, "/bad", nil, nil)
			peer := acceptPeer(t, ln)
			badRequest := peer.nextHeaders()
			good := doAsync(hc, fasthttp.MethodGet, "/good", nil, nil)
			goodRequest := peer.nextHeaders()
			peer.write(func(framer *xhttp2.Framer) error {
				framer.AllowIllegalWrites = true
				defer func() { framer.AllowIllegalWrites = false }()
				return framer.WriteWindowUpdate(badRequest.streamID, tc.increment)
			})
			frames := peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameRSTStream })
			if reset := frames[len(frames)-1]; reset.streamID != badRequest.streamID || reset.errCode != tc.code {
				t.Fatalf("RST_STREAM(%d, %v), want (%d, %v)", reset.streamID, reset.errCode, badRequest.streamID, tc.code)
			}
			peer.writeHeaders(goodRequest.streamID, true, ":status", "200")
			if err := waitResultOf(t, good); err != nil {
				t.Fatalf("unrelated stream failed: %v", err)
			}
			if err := waitResultOf(t, bad); err == nil {
				t.Fatal("reset stream succeeded")
			}
		})
	}
}

func promisePush(peer *clientPeer, parentID, promisedID uint32, method, authority, path string) {
	peer.wmu.Lock()
	defer peer.wmu.Unlock()
	_ = peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
		StreamID:      parentID,
		PromiseID:     promisedID,
		BlockFragment: peer.encode(":method", method, ":scheme", "http", ":authority", authority, ":path", path),
		EndHeaders:    true,
	})
}

// GOAWAY's last stream ID covers client-initiated streams, so it doesn't end
// a push in progress.
func TestClientGoAwayKeepsPushInProgress(t *testing.T) {
	ln := listenPeer(t)
	pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
	peer.write(func(framer *xhttp2.Framer) error {
		return framer.WriteGoAway(parent.streamID, xhttp2.ErrCodeNo, nil)
	})
	peer.writeHeaders(2, false, ":status", "200")
	peer.writeData(2, true, []byte("pushed"))
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	select {
	case body := <-pushes.responseBody:
		if body != "pushed" {
			t.Fatalf("pushed body = %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GOAWAY dropped the push in progress")
	}
}

// A pushed HEAD response may carry the Content-Length of the GET it mirrors.
func TestClientHandlesPushedHeadResponseWithContentLength(t *testing.T) {
	ln := listenPeer(t)
	pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	promisePush(peer, parent.streamID, 2, fasthttp.MethodHead, parent.header(":authority"), "/pushed")
	peer.writeHeaders(2, true, ":status", "200", "content-length", "10")
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	select {
	case body := <-pushes.responseBody:
		if body != "" {
			t.Fatalf("pushed HEAD body = %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pushed HEAD response with Content-Length was dropped")
	}
}

// syncOrGoAway PINGs the client and returns the frames it sent before the
// ack, or up to and including a GOAWAY that precedes it: a failed connection
// never acks.
func (p *clientPeer) syncOrGoAway() []peerFrame {
	p.t.Helper()
	p.pingSeq++
	var data [8]byte
	data[0] = 'g'
	data[7] = byte(p.pingSeq)
	p.write(func(framer *xhttp2.Framer) error { return framer.WritePing(false, data) })
	frames := p.until(func(frame peerFrame) bool {
		return frame.typ == xhttp2.FrameGoAway ||
			frame.typ == xhttp2.FramePing && frame.ack && bytes.Equal(frame.data, data[:])
	})
	if frames[len(frames)-1].typ == xhttp2.FrameGoAway {
		return frames
	}
	return frames[:len(frames)-1]
}

func hasGoAway(frames []peerFrame, code xhttp2.ErrCode) bool {
	for _, frame := range frames {
		if frame.typ == xhttp2.FrameGoAway && frame.errCode == code {
			return true
		}
	}
	return false
}

// RFC 9113 §5.1: a promised stream is reserved (remote) until its first
// HEADERS; DATA or WINDOW_UPDATE on it is a connection error. An informational
// HEADERS already ends the reserved state.
func TestClientReservedPushStreamRejectsDataAndWindowUpdate(t *testing.T) {
	windowUpdate := func(peer *clientPeer) {
		peer.write(func(framer *xhttp2.Framer) error { return framer.WriteWindowUpdate(2, 1) })
	}
	data := func(peer *clientPeer) { peer.writeData(2, false, []byte("x")) }
	for _, tc := range []struct {
		name  string
		frame func(peer *clientPeer)
	}{
		{"window update", windowUpdate},
		{"data", data},
	} {
		t.Run(tc.name+" on reserved stream", func(t *testing.T) {
			ln := listenPeer(t)
			pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
			hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
			hc.MaxIdemponentCallAttempts = 1
			done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
			peer := acceptPeer(t, ln)
			parent := peer.nextHeaders()
			promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
			tc.frame(peer)
			if frames := peer.syncOrGoAway(); !hasGoAway(frames, xhttp2.ErrCodeProtocol) {
				t.Fatalf("frames after %s on a reserved stream: %+v, want GOAWAY(PROTOCOL_ERROR)", tc.name, frames)
			}
			if err := waitResultOf(t, done); err == nil {
				t.Fatal("the request on the failed connection succeeded")
			}
		})
	}
	t.Run("window update after informational headers", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		parent := peer.nextHeaders()
		promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
		peer.writeHeaders(2, false, ":status", "103")
		windowUpdate(peer)
		for _, frame := range peer.syncOrGoAway() {
			if frame.typ == xhttp2.FrameGoAway || frame.typ == xhttp2.FrameRSTStream {
				t.Fatalf("WINDOW_UPDATE after an informational response drew %+v", frame)
			}
		}
		peer.writeHeaders(2, false, ":status", "200")
		peer.writeData(2, true, []byte("pushed"))
		peer.writeHeaders(parent.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		select {
		case body := <-pushes.responseBody:
			if body != "pushed" {
				t.Fatalf("pushed body = %q", body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the pushed response was dropped")
		}
	})
	t.Run("data after informational headers", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		parent := peer.nextHeaders()
		promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
		peer.writeHeaders(2, false, ":status", "103")
		data(peer)
		frames := peer.syncOrGoAway()
		if hasGoAway(frames, xhttp2.ErrCodeProtocol) {
			t.Fatalf("DATA before the final response failed the connection: %+v", frames)
		}
		reset := false
		for _, frame := range frames {
			if frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 && frame.errCode == xhttp2.ErrCodeProtocol {
				reset = true
			}
		}
		if !reset {
			t.Fatalf("frames after DATA before the final response: %+v, want RST_STREAM(2, PROTOCOL_ERROR)", frames)
		}
		peer.writeHeaders(parent.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
	})
}

// RFC 9113 §6.6: a PUSH_PROMISE on a stream the client never opened is a
// connection error; one on a closed parent is declined with CANCEL.
func TestClientPushPromiseOnMissingParent(t *testing.T) {
	t.Run("idle parent", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		hc.MaxIdemponentCallAttempts = 1
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		parent := peer.nextHeaders()
		promisePush(peer, parent.streamID+2, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
		if frames := peer.syncOrGoAway(); !hasGoAway(frames, xhttp2.ErrCodeProtocol) {
			t.Fatalf("frames after a promise on an idle parent: %+v, want GOAWAY(PROTOCOL_ERROR)", frames)
		}
		if err := waitResultOf(t, done); err == nil {
			t.Fatal("the request on the failed connection succeeded")
		}
		select {
		case path := <-pushes.acceptedPath:
			t.Fatalf("the promise reached PushHandler.Accept for %q", path)
		default:
		}
	})
	t.Run("closed parent", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		closed := peer.nextHeaders()
		peer.writeHeaders(closed.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		done = doAsync(hc, fasthttp.MethodGet, "/next", nil, nil)
		next := peer.nextHeaders()
		promisePush(peer, closed.streamID, 2, fasthttp.MethodGet, closed.header(":authority"), "/pushed")
		frames := peer.syncOrGoAway()
		if hasGoAway(frames, xhttp2.ErrCodeProtocol) {
			t.Fatalf("a promise on a closed parent failed the connection: %+v", frames)
		}
		reset := false
		for _, frame := range frames {
			if frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 && frame.errCode == xhttp2.ErrCodeCancel {
				reset = true
			}
		}
		if !reset {
			t.Fatalf("frames after a promise on a closed parent: %+v, want RST_STREAM(2, CANCEL)", frames)
		}
		peer.writeHeaders(next.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error after the declined promise: %v", err)
		}
	})
}

// recordingPushHandler accepts every promise and records the framing
// metadata of the promised request.
type recordingPushHandler struct {
	framing chan string
}

func (h *recordingPushHandler) Accept(_, promised *fasthttp.Request) bool {
	h.framing <- fmt.Sprintf("%s cl=%d te=%q", promised.URI().Path(), promised.Header.ContentLength(), promised.Header.Peek(fasthttp.HeaderTransferEncoding))
	return true
}

func (*recordingPushHandler) Handle(*fasthttp.Request, *fasthttp.Response) {}

// A promised request without a content-length field reports the length an
// HTTP/1 request without one does, -2, and no Transfer-Encoding entry; a
// declared zero length is kept.
func TestClientPromisedRequestContentLength(t *testing.T) {
	ln := listenPeer(t)
	pushes := &recordingPushHandler{framing: make(chan string, 2)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	authority := parent.header(":authority")
	promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, authority, "/undeclared")
	peer.wmu.Lock()
	err := peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
		StreamID: parent.streamID, PromiseID: 4, EndHeaders: true,
		BlockFragment: peer.encode(":method", "GET", ":scheme", "http", ":authority", authority, ":path", "/zero", "content-length", "0"),
	})
	peer.wmu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`/undeclared cl=-2 te=""`, `/zero cl=0 te=""`} {
		select {
		case got := <-pushes.framing:
			if got != want {
				t.Fatalf("promised request framing = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the promise for %q didn't reach PushHandler.Accept", want)
		}
	}
	peer.writeHeaders(2, true, ":status", "204")
	peer.writeHeaders(4, true, ":status", "204")
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
}

// A final 204 or 304 under StreamResponseBody has no body to stream: Do
// returns at END_STREAM without a body stream, and the connection is idle at
// once, as over HTTP/1.
func TestClientStreamResponseBodyBodylessStatus(t *testing.T) {
	for _, status := range []int{fasthttp.StatusNoContent, fasthttp.StatusNotModified} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ln := listenPeer(t)
			hc, transport := newPeerClient(t, ln, &ClientConfig{})
			hc.StreamResponseBody = true
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			peer.writeHeaders(request.streamID, false, ":status", strconv.Itoa(status))
			peer.sync()
			select {
			case err := <-done:
				t.Fatalf("Do() returned %v before END_STREAM", err)
			case <-time.After(50 * time.Millisecond):
			}
			peer.writeData(request.streamID, true, nil)
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("Do() error: %v", err)
			}
			if resp.StatusCode() != status || resp.IsBodyStream() || len(resp.Body()) != 0 {
				t.Fatalf("status %d, body stream %v, body %q; want %d, no stream, empty",
					resp.StatusCode(), resp.IsBodyStream(), resp.Body(), status)
			}
			if conn := poolConns(transport, hc)[0]; !conn.isIdle() {
				t.Fatal("the connection isn't idle after Do returned")
			}
		})
	}
}

// Which responses under StreamResponseBody arrive as a body stream matches
// HTTP/1: a 200 with a body does; HEAD, SkipBody, 204 and 304 don't.
func TestClientStreamResponseBodyBodylessParity(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/204":
			ctx.SetStatusCode(fasthttp.StatusNoContent)
		case "/304":
			ctx.SetStatusCode(fasthttp.StatusNotModified)
		default:
			ctx.SetBodyString("body")
		}
	}}, ServerConfig{})
	addr := testServer.listener.Addr().String()
	h1 := &fasthttp.HostClient{Addr: addr, StreamResponseBody: true}
	t.Cleanup(h1.CloseIdleConnections)
	h2 := newPriorKnowledgeHostClient(t, addr)
	h2.StreamResponseBody = true
	for _, path := range []string{"/head", "/skip", "/204", "/304", "/body"} {
		var views [2]string
		for i, hc := range []*fasthttp.HostClient{h1, h2} {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			if path == "/head" {
				req.Header.SetMethod(fasthttp.MethodHead)
			}
			resp.SkipBody = path == "/skip"
			req.SetRequestURI("http://" + addr + path)
			if err := hc.Do(req, resp); err != nil {
				t.Fatalf("%s over protocol %d: %v", path, i+1, err)
			}
			streamed := resp.IsBodyStream()
			var body []byte
			if streamed {
				read, err := io.ReadAll(resp.BodyStream())
				if err != nil {
					t.Fatalf("%s over protocol %d: reading the body: %v", path, i+1, err)
				}
				body = read
			} else {
				body = resp.Body()
			}
			views[i] = fmt.Sprintf("status=%d stream=%v body=%q", resp.StatusCode(), streamed, body)
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
		}
		if views[0] != views[1] {
			t.Errorf("%s: HTTP/1 %s, HTTP/2 %s", path, views[0], views[1])
		}
		if wantStream := path == "/body"; strings.Contains(views[1], fmt.Sprintf("stream=%v", !wantStream)) {
			t.Errorf("%s over HTTP/2: %s, want stream=%v", path, views[1], wantStream)
		}
	}
}

// closeFailingReader is a request body whose Close fails, after release is
// closed when set.
type closeFailingReader struct {
	io.Reader

	release <-chan struct{}
	err     error
}

func (r *closeFailingReader) Close() error {
	if r.release != nil {
		<-r.release
	}
	return r.err
}

// A request body stream's Close error is the request's, as Request.Write
// reports it over HTTP/1, and the connection stays usable.
func TestClientRequestBodyCloseErrorMatchesHTTP1(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyString("ok")
	}}, ServerConfig{})
	addr := testServer.listener.Addr().String()
	// An error wrapping io.EOF keeps its identity: it isn't the connection's.
	causes := []error{errors.New("close failed"), fmt.Errorf("close failed: %w", io.EOF)}
	for _, streaming := range []bool{false, true} {
		h1 := &fasthttp.HostClient{Addr: addr, StreamResponseBody: streaming}
		t.Cleanup(h1.CloseIdleConnections)
		h2 := newPriorKnowledgeHostClient(t, addr)
		h2.StreamResponseBody = streaming
		for _, cause := range causes {
			for i, hc := range []*fasthttp.HostClient{h1, h2} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				req.Header.SetMethod(fasthttp.MethodPost)
				req.SetRequestURI("http://" + addr + "/")
				req.SetBodyStream(&closeFailingReader{Reader: strings.NewReader("body"), err: cause}, 4)
				if err := hc.Do(req, resp); !errors.Is(err, cause) {
					t.Fatalf("streaming=%v protocol %d: Do() error = %v, want %v", streaming, i+1, err, cause)
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
				if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); err != nil {
					t.Fatalf("streaming=%v protocol %d: request after the Close error: %v", streaming, i+1, err)
				}
			}
		}
	}
}

// lastBytesReader returns data with err on its first Read, then err alone.
type lastBytesReader struct {
	data string
	err  error
	sent bool
}

func (r *lastBytesReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, r.err
	}
	r.sent = true
	return copy(p, r.data), r.err
}

// A request body reader returning its last bytes with an error wrapping
// io.EOF fails the request with that error, as Request.Write does, and the
// DATA carrying those bytes has no END_STREAM; a bare io.EOF completes the
// body.
func TestClientRequestReaderWrappedEOFMatchesHTTP1(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetBody(ctx.Request.Body())
	}}, ServerConfig{})
	addr := testServer.listener.Addr().String()
	cause := fmt.Errorf("reader failed: %w", io.EOF)
	for _, size := range []int{4, -1} {
		for _, readErr := range []error{io.EOF, cause} {
			for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				req.Header.SetMethod(fasthttp.MethodPost)
				req.SetRequestURI("http://" + addr + "/")
				req.SetBodyStream(&lastBytesReader{data: "body", err: readErr}, size)
				err := hc.Do(req, resp)
				switch {
				case readErr == io.EOF && (err != nil || string(resp.Body()) != "body"): //nolint:errorlint // the bare sentinel
					t.Fatalf("size %d protocol %d, bare EOF: Do() = %v, body %q", size, i+1, err, resp.Body())
				case readErr != io.EOF && !errors.Is(err, cause): //nolint:errorlint // the bare sentinel
					t.Fatalf("size %d protocol %d, wrapped EOF: Do() error = %v, want %v", size, i+1, err, cause)
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
				if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); err != nil {
					t.Fatalf("size %d protocol %d: request after the body: %v", size, i+1, err)
				}
			}
		}
	}
	// On the wire: the bytes go out without END_STREAM, then the stream is reset.
	for _, size := range []int{4, -1} {
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		done := make(chan error, 1)
		go func() {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetRequestURI("http://" + hc.Addr + "/")
			req.SetBodyStream(&lastBytesReader{data: "body", err: cause}, size)
			done <- hc.Do(req, resp)
		}()
		peer := acceptPeer(t, ln)
		request := peer.nextHeaders()
		if err := waitResultOf(t, done); !errors.Is(err, cause) {
			t.Fatalf("size %d: Do() error = %v, want %v", size, err, cause)
		}
		frames := peer.until(func(frame peerFrame) bool {
			return frame.typ == xhttp2.FrameRSTStream && frame.streamID == request.streamID
		})
		data := 0
		for _, frame := range frames {
			if frame.typ == xhttp2.FrameData && frame.streamID == request.streamID {
				data += len(frame.data)
				if frame.endStream {
					t.Fatalf("size %d: DATA with END_STREAM after the reader's error", size)
				}
			}
		}
		if data != 4 {
			t.Fatalf("size %d: %d body bytes on the wire, want 4", size, data)
		}
	}
}

// A sized request body stream follows Request.Write's rules on both
// protocols: a bare *os.File stops at the declared length, another reader
// with bytes left over fails, a short reader fails with the fixed-size error.
func TestClientSizedBodyStreamMatchesHTTP1(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetBody(ctx.Request.Body())
	}}, ServerConfig{})
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		name string
		body func(t *testing.T) io.Reader
		size int
		want string // the echoed body; "" for a failure
	}{
		{"file longer than declared", func(t *testing.T) io.Reader {
			file, err := os.CreateTemp(t.TempDir(), "body")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if _, err := file.WriteString("1234567890"); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			return file
		}, 4, "1234"},
		{"reader longer than declared", func(*testing.T) io.Reader { return strings.NewReader("1234567") }, 4, ""},
		{"reader shorter than declared", func(*testing.T) io.Reader { return strings.NewReader("123") }, 6, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errs [2]string
			for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				req.Header.SetMethod(fasthttp.MethodPost)
				req.SetRequestURI("http://" + addr + "/")
				req.SetBodyStream(tc.body(t), tc.size)
				err := hc.Do(req, resp)
				switch {
				case tc.want != "" && (err != nil || string(resp.Body()) != tc.want):
					t.Fatalf("protocol %d: Do() = %v, body %q, want %q", i+1, err, resp.Body(), tc.want)
				case tc.want == "" && err == nil:
					t.Fatalf("protocol %d: Do() succeeded with body %q", i+1, resp.Body())
				case err != nil:
					errs[i] = err.Error()
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
				if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); err != nil {
					t.Fatalf("protocol %d: request after the body: %v", i+1, err)
				}
			}
			if errs[0] != errs[1] {
				t.Fatalf("HTTP/1 error %q, HTTP/2 error %q", errs[0], errs[1])
			}
		})
	}
}

// A Close error that follows the response wins over it, with its identity,
// also when it wraps io.EOF: buffered or streamed, complete or still open.
// Only an open stream is reset.
func TestClientRequestBodyCloseErrorReplacesQueuedResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
		endStream bool
		wrapsEOF  bool
	}{
		{"buffered response", false, true, false},
		{"buffered response, EOF close error", false, true, true},
		{"streamed response complete", true, true, false},
		{"streamed response complete, EOF close error", true, true, true},
		{"streamed response open", true, false, false},
		{"streamed response open, EOF close error", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			hc, transport := newPeerClient(t, ln, &ClientConfig{})
			hc.StreamResponseBody = tc.streaming
			errClose := errors.New("close failed")
			if tc.wrapsEOF {
				errClose = fmt.Errorf("close failed: %w", io.EOF)
			}
			release := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(resp)
				req.Header.SetMethod(fasthttp.MethodPost)
				req.SetRequestURI("http://" + hc.Addr + "/")
				req.SetBodyStream(&closeFailingReader{Reader: strings.NewReader("body"), release: release, err: errClose}, 4)
				done <- hc.Do(req, resp)
			}()
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameData && frame.endStream })
			peer.writeHeaders(request.streamID, tc.endStream, ":status", "200")
			// The response is processed, and its result queued, before the ack.
			peer.sync()
			close(release)
			if err := waitResultOf(t, done); !errors.Is(err, errClose) {
				t.Fatalf("Do() error = %v, want the Close error", err)
			}
			if conn := poolConns(transport, hc)[0]; !conn.isIdle() {
				t.Fatal("the connection isn't idle after Do returned")
			}
			resets := 0
			for _, frame := range peer.sync() {
				if frame.typ == xhttp2.FrameRSTStream && frame.streamID == request.streamID {
					if frame.errCode != xhttp2.ErrCodeCancel {
						t.Fatalf("RST_STREAM code = %v, want CANCEL", frame.errCode)
					}
					resets++
				}
			}
			want := 0
			if !tc.endStream {
				want = 1
			}
			if resets != want {
				t.Fatalf("%d resets of stream %d, want %d", resets, request.streamID, want)
			}
			next := doAsync(hc, fasthttp.MethodGet, "/next", nil, nil)
			peer.writeHeaders(peer.nextHeaders().streamID, true, ":status", "200")
			if err := waitResultOf(t, next); err != nil {
				t.Fatalf("request after the Close error: %v", err)
			}
		})
	}
}

// serveHTTP1Closing accepts connections and closes each one after reading a
// request, without answering.
func serveHTTP1Closing(t *testing.T, ln net.Listener) *atomic.Int32 {
	t.Helper()
	var attempts atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			go func() {
				var req fasthttp.Request
				_ = req.Read(bufio.NewReader(conn))
				_ = conn.Close()
			}()
		}
	}()
	return &attempts
}

// Failures before the response headers are offered to HostClient's retry
// policy as HTTP/1's write and read errors are: an idempotent request is
// retried up to MaxIdemponentCallAttempts, a POST isn't by default, and a body
// stream that went out is never replayed.
func TestClientPreHeaderFailuresFollowRetryPolicy(t *testing.T) {
	const maxAttempts = 3
	invalidStatus := func(peer *clientPeer, streamID uint32) { peer.writeHeaders(streamID, true, ":status", "abc") }
	stalledWindow := []xhttp2.Setting{{ID: xhttp2.SettingInitialWindowSize, Val: 0}}
	writeTimeout := func(hc *fasthttp.HostClient) { hc.WriteTimeout = 50 * time.Millisecond }
	isTimeout := func(_ bool, err error) bool { return errors.Is(err, fasthttp.ErrTimeout) }
	cases := []struct {
		name       string
		method     string
		bodyStream bool
		h2Only     bool
		// settings are applied by a warm-up request before the one under test.
		settings []xhttp2.Setting
		setup    func(hc *fasthttp.HostClient)
		h1       string
		h1Closes bool
		h2       func(peer *clientPeer, streamID uint32)
		attempts int32
		want     func(h2 bool, err error) bool
	}{
		{
			name:     "invalid status",
			method:   fasthttp.MethodGet,
			h1:       "HTTP/1.1 abc OK\r\nContent-Length: 0\r\n\r\n",
			h2:       invalidStatus,
			attempts: maxAttempts,
		},
		{
			name:     "invalid status POST",
			method:   fasthttp.MethodPost,
			h1:       "HTTP/1.1 abc OK\r\nContent-Length: 0\r\n\r\n",
			h2:       invalidStatus,
			attempts: 1,
		},
		{
			name:   "connection-specific response field",
			method: fasthttp.MethodGet,
			h1:     "HTTP/1.1 200 OK\r\nContent-Length: abc\r\n\r\n",
			h2: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, true, ":status", "200", "transfer-encoding", "chunked")
			},
			attempts: maxAttempts,
		},
		{
			name:     "peer reset before headers",
			method:   fasthttp.MethodGet,
			h1Closes: true,
			h2: func(peer *clientPeer, streamID uint32) {
				peer.write(func(framer *xhttp2.Framer) error {
					return framer.WriteRSTStream(streamID, xhttp2.ErrCodeInternal)
				})
			},
			attempts: maxAttempts,
			want: func(h2 bool, err error) bool {
				if !h2 {
					return errors.Is(err, fasthttp.ErrConnectionClosed)
				}
				_, ok := errors.AsType[*StreamError](err)
				return ok
			},
		},
		{
			name:   "invalid response data",
			method: fasthttp.MethodGet,
			h1:     "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n",
			h2: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, false, ":status", "200", "content-length", "2")
				peer.writeData(streamID, true, []byte("abc"))
			},
			attempts: maxAttempts,
		},
		{
			name:   "read timeout before headers, streaming response",
			method: fasthttp.MethodGet,
			setup: func(hc *fasthttp.HostClient) {
				hc.StreamResponseBody = true
				hc.ReadTimeout = 40 * time.Millisecond
			},
			attempts: maxAttempts,
			want:     isTimeout,
		},
		{
			name:     "write timeout on a stalled window",
			method:   fasthttp.MethodPut,
			h2Only:   true,
			settings: stalledWindow,
			setup:    writeTimeout,
			attempts: maxAttempts,
			want:     isTimeout,
		},
		{
			name:       "write timeout on a stalled window, body stream",
			method:     fasthttp.MethodPut,
			bodyStream: true,
			h2Only:     true,
			settings:   stalledWindow,
			setup:      writeTimeout,
			attempts:   1,
			want:       isTimeout,
		},
	}
	for _, tc := range cases {
		for _, h2 := range []bool{false, true} {
			if tc.h2Only && !h2 {
				continue
			}
			t.Run(fmt.Sprintf("%s/h2=%v", tc.name, h2), func(t *testing.T) {
				ln := listenPeer(t)
				hc := &fasthttp.HostClient{Addr: ln.Addr().String(), MaxIdemponentCallAttempts: maxAttempts}
				if h2 {
					if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
						t.Fatalf("ConfigureHostClient() error: %v", err)
					}
				}
				t.Cleanup(hc.CloseIdleConnections)
				if tc.setup != nil {
					tc.setup(hc)
				}
				var peer *clientPeer
				if h2 && len(tc.settings) != 0 {
					warm := doAsync(hc, fasthttp.MethodGet, "/warm", nil, nil)
					peer = acceptPeer(t, ln, tc.settings...)
					peer.writeHeaders(peer.nextHeaders().streamID, true, ":status", "200")
					if err := waitResultOf(t, warm); err != nil {
						t.Fatalf("warm-up Do() error: %v", err)
					}
				}
				done := make(chan error, 1)
				go func() {
					req := fasthttp.AcquireRequest()
					resp := fasthttp.AcquireResponse()
					defer fasthttp.ReleaseRequest(req)
					defer fasthttp.ReleaseResponse(resp)
					req.Header.SetMethod(tc.method)
					req.SetRequestURI("http://" + hc.Addr + "/")
					switch {
					case tc.bodyStream:
						req.SetBodyStream(strings.NewReader("payload"), 7)
					case tc.method != fasthttp.MethodGet:
						req.SetBodyString("payload")
					}
					done <- hc.Do(req, resp)
				}()
				var attempts int32
				var err error
				if !h2 {
					var counter *atomic.Int32
					if tc.h1Closes {
						counter = serveHTTP1Closing(t, ln)
					} else {
						counter = serveHTTP1Attempts(t, ln, tc.h1)
					}
					err = waitResultOf(t, done)
					attempts = counter.Load()
				} else {
					if peer == nil {
						peer = acceptPeer(t, ln)
					}
				loop:
					for {
						select {
						case err = <-done:
							break loop
						case frame, ok := <-peer.frames:
							if !ok {
								peer = acceptPeer(t, ln, tc.settings...)
								continue
							}
							if frame.typ == xhttp2.FrameHeaders {
								attempts++
								if tc.h2 != nil {
									tc.h2(peer, frame.streamID)
								}
							}
						case <-time.After(5 * time.Second):
							t.Fatal("request didn't finish")
						}
					}
				}
				if err == nil {
					t.Fatal("Do() succeeded")
				}
				if tc.want != nil && !tc.want(h2, err) {
					t.Fatalf("Do() error = %v", err)
				}
				if attempts != tc.attempts {
					t.Fatalf("attempts = %d, want %d", attempts, tc.attempts)
				}
			})
		}
	}
}

// A write deadline passed waiting for the write slot, held by an upload
// parked against a stalled peer, is offered to the retry policy as a write
// timeout is over HTTP/1.
func TestClientWriteSlotTimeoutFollowsRetryPolicy(t *testing.T) {
	const maxAttempts = 3
	ln := listenPeer(t)
	// WriteByteTimeout would otherwise follow WriteTimeout and fail the
	// stalled connection itself.
	hc, transport := newPeerClient(t, ln, &ClientConfig{WriteByteTimeout: 10 * time.Second})
	hc.MaxIdemponentCallAttempts = maxAttempts
	hc.WriteTimeout = 50 * time.Millisecond
	var offered atomic.Int32
	hc.RetryIfErr = func(_ *fasthttp.Request, _ int, err error) (bool, bool) {
		offered.Add(1)
		return false, errors.Is(err, fasthttp.ErrTimeout)
	}
	_, conn := warmParkingPeer(t, ln, hc, transport)
	parkUpload(t, hc, conn)
	if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("Do() error = %v, want ErrTimeout", err)
	}
	if got := offered.Load(); got != maxAttempts-1 {
		t.Fatalf("the retry policy was consulted %d times, want %d", got, maxAttempts-1)
	}
}

// firePerStreamReadTimer makes the per-stream read timer of streamID fire at
// once, so that it alone fails the request.
func firePerStreamReadTimer(t *testing.T, c *clientConn, streamID uint32) {
	t.Helper()
	var timer *time.Timer
	waitUntil(t, "the per-stream read timer is armed", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		stream := c.streams[streamID]
		if stream == nil || stream.readTimer == nil {
			return false
		}
		timer = stream.readTimer
		return true
	})
	timer.Reset(0)
}

// The caller's deadline isn't offered to the retry policy; a ReadTimeout
// expiry is, from waitResult's timer and from a streaming request's
// per-stream timer alike. A policy resetting the timeout would otherwise
// carry the request past its deadline.
func TestClientDeadlineExpiryRetryEligibility(t *testing.T) {
	const maxAttempts = 3
	const short = 60 * time.Millisecond
	for _, tc := range []struct {
		name        string
		streaming   bool
		timeout     time.Duration
		readTimeout time.Duration
		// firePerStream fires the per-stream timer at once, before
		// waitResult's timer can.
		firePerStream bool
		offered       int32
	}{
		{name: "caller deadline, buffered", timeout: short},
		{name: "caller deadline, streaming", streaming: true, timeout: short},
		{name: "read timeout, buffered", readTimeout: short, offered: maxAttempts - 1},
		{name: "read timeout, streaming", streaming: true, readTimeout: short, offered: maxAttempts - 1},
		{
			name: "per-stream timer, read timeout", streaming: true, readTimeout: 5 * time.Second,
			firePerStream: true, offered: maxAttempts - 1,
		},
		{
			name: "per-stream timer, caller deadline", streaming: true, timeout: 2 * time.Second,
			readTimeout: 5 * time.Second, firePerStream: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			hc, transport := newPeerClient(t, ln, &ClientConfig{})
			hc.StreamResponseBody = tc.streaming
			hc.MaxIdemponentCallAttempts = maxAttempts
			hc.ReadTimeout = tc.readTimeout
			var offered atomic.Int32
			hc.RetryIfErr = func(*fasthttp.Request, int, error) (bool, bool) {
				offered.Add(1)
				return true, true
			}
			done := make(chan error, 1)
			go func() {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(resp)
				req.SetRequestURI("http://" + hc.Addr + "/")
				if tc.timeout > 0 {
					req.SetTimeout(tc.timeout)
				}
				done <- hc.Do(req, resp)
			}()
			peer := acceptPeer(t, ln)
			attempts := int32(0)
			var err error
		loop:
			for {
				select {
				case err = <-done:
					break loop
				case frame, ok := <-peer.frames:
					if !ok {
						t.Fatal("the client closed the connection")
					}
					if frame.typ != xhttp2.FrameHeaders {
						continue
					}
					attempts++
					if tc.firePerStream {
						firePerStreamReadTimer(t, poolConns(transport, hc)[0], frame.streamID)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("request didn't finish")
				}
			}
			if !errors.Is(err, fasthttp.ErrTimeout) {
				t.Fatalf("Do() error = %v, want ErrTimeout", err)
			}
			if got := offered.Load(); got != tc.offered {
				t.Fatalf("the retry policy was consulted %d times, want %d", got, tc.offered)
			}
			if attempts != tc.offered+1 {
				t.Fatalf("attempts = %d, want %d", attempts, tc.offered+1)
			}
		})
	}
}

// A connection out of stream IDs is retired, so with MaxConns 1 it frees the
// only slot for a new connection.
func TestClientRetiresExhaustedConnectionUnderMaxConns(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
	}
	testServer := newTestServer(t, server, ServerConfig{})
	hc, transport := newPeerClient(t, testServer.listener, &ClientConfig{})
	hc.MaxConns = 1
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(testServer.URL("/"))
	if err := hc.Do(req, resp); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	conn := poolConns(transport, hc)[0]
	conn.mu.Lock()
	conn.nextStreamID = math.MaxInt32
	conn.mu.Unlock()
	for i := range 4 {
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() #%d around ID exhaustion: %v", i, err)
		}
		if string(resp.Body()) != "ok" {
			t.Fatalf("Do() #%d body = %q", i, resp.Body())
		}
	}
}

// RFC 9113 §5.4.1: a connection error the peer caused is reported with
// GOAWAY before the connection closes.
func TestClientSendsGoAwayOnPeerConnectionError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config ClientConfig
		cause  func(peer *clientPeer, streamID uint32)
		code   xhttp2.ErrCode
	}{
		{
			name:  "DATA on idle stream",
			cause: func(peer *clientPeer, _ uint32) { peer.writeData(99, false, []byte("x")) },
			code:  xhttp2.ErrCodeProtocol,
		},
		{
			name:   "connection window exceeded",
			config: ClientConfig{MaxResponseBufferPerConnection: 65535},
			cause: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, false, ":status", "200")
				for range 5 {
					peer.writeData(streamID, false, make([]byte, defaultMaxFrameSize))
				}
			},
			code: xhttp2.ErrCodeFlowControl,
		},
		{
			name: "undecodable header block",
			cause: func(peer *clientPeer, streamID uint32) {
				peer.write(func(framer *xhttp2.Framer) error {
					return framer.WriteHeaders(xhttp2.HeadersFrameParam{
						StreamID:      streamID,
						BlockFragment: []byte{0xff, 0xff, 0xff, 0xff, 0x0f},
						EndHeaders:    true,
					})
				})
			},
			code: xhttp2.ErrCodeCompression,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			hc, _ := newPeerClient(t, ln, &tc.config)
			hc.StreamResponseBody = true
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			done := doAsync(hc, fasthttp.MethodPost, "/", nil, resp)
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			tc.cause(peer, request.streamID)
			frames := peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameGoAway })
			if goAway := frames[len(frames)-1]; goAway.errCode != tc.code {
				t.Fatalf("GOAWAY code = %v, want %v", goAway.errCode, tc.code)
			}
			_ = waitResultOf(t, done)
		})
	}
}

// The GOAWAY is best effort: a wedged write path doesn't delay the teardown.
func TestClientGoAwayOnWedgedConnectionIsBounded(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetRequestURI("http://" + hc.Addr + "/upload")
		req.SetBodyStream(io.LimitReader(neverEndingReader{}, 1<<30), -1)
		done <- hc.Do(req, nil)
	}()
	setAcceptDeadline(ln, time.Now().Add(5*time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.ReadFull(conn, make([]byte, len(clientPreface))); err != nil {
		t.Fatal(err)
	}
	framer := xhttp2.NewFramer(conn, conn)
	_ = framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
	_ = framer.WriteWindowUpdate(0, 1<<31-1-65535)
	// From here on nothing is read, so the client's writer wedges.
	var client *clientConn
	waitUntil(t, "the client's write queue filled", func() bool {
		if conns := poolConns(transport, hc); len(conns) != 0 {
			client = conns[0]
		}
		return client != nil && len(client.writer.queue) == cap(client.writer.queue)
	})
	start := time.Now()
	_ = framer.WriteData(99, false, []byte("x"))
	_ = waitResultOf(t, done)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("teardown took %v", elapsed)
	}
}

// RFC 9113 §6.5.3: until the peer acknowledges SETTINGS its encoder may still
// use the default 4096-byte HPACK table; a smaller advertised size applies
// from the ACK on.
func TestClientHPACKTableSizeAppliesOnSettingsAck(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{MaxDecoderHeaderTableSize: 256})
	big := strings.Repeat("v", 1000)
	var peer *clientPeer
	exchange := func(fields ...string) *fasthttp.Response {
		t.Helper()
		resp := fasthttp.AcquireResponse()
		t.Cleanup(func() { fasthttp.ReleaseResponse(resp) })
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
		if peer == nil {
			peer = startPeer(t, ln, true)
		}
		request := peer.nextHeaders()
		peer.writeHeaders(request.streamID, true, append([]string{":status", "200"}, fields...)...)
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		return resp
	}
	// The peer's encoder indexes x-big in its default table, then refers to
	// it by index.
	exchange("x-big", big)
	if got := string(exchange("x-big", big).Header.Peek("x-big")); got != big {
		t.Fatalf("x-big = %d bytes, want %d", len(got), len(big))
	}

	peer.ackSettings()
	peer.sync()
	peer.wmu.Lock()
	peer.encoder.SetMaxDynamicTableSize(256)
	peer.wmu.Unlock()
	if got := string(exchange("x-small", "s").Header.Peek("x-small")); got != "s" {
		t.Fatalf("x-small = %q after the ACK", got)
	}
}

// A rejected extended CONNECT whose response ended the stream gets the
// client's END_STREAM, so the peer's side of the stream closes too.
func TestClientClosesRejectedOpenStream(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true})
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI("http://" + hc.Addr + "/ws")
		conn, err := hc.OpenStream(req, resp)
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	peer := acceptPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1})
	request := peer.nextHeaders()
	if request.endStream {
		t.Fatal("extended CONNECT HEADERS ended the stream")
	}
	peer.writeHeaders(request.streamID, true, ":status", "403")
	if err := waitResultOf(t, done); err == nil {
		t.Fatal("OpenStream() succeeded on a 403")
	}
	frames := peer.sync()
	ended := false
	for _, frame := range frames {
		switch {
		case frame.typ == xhttp2.FrameRSTStream && frame.streamID == request.streamID:
			t.Fatalf("client reset a stream the peer had already ended: %v", frame.errCode)
		case frame.typ == xhttp2.FrameData && frame.streamID == request.streamID:
			ended = frame.endStream && len(frame.data) == 0
		}
	}
	if !ended {
		t.Fatal("client left its side of the rejected stream open")
	}
}

// Any frame after the health-check PING shows the peer is alive, the ack
// or not.
func TestClientHealthCheckAcceptsAnyFrameAsLiveness(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{
		ReadIdleTimeout: 20 * time.Millisecond,
		PingTimeout:     100 * time.Millisecond,
	})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	peer.noPingAck.Store(true)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	isPing := func(frame peerFrame) bool { return frame.typ == xhttp2.FramePing && !frame.ack }
	peer.until(isPing)
	peer.write(func(framer *xhttp2.Framer) error { return framer.WriteWindowUpdate(0, 1) })
	// The connection lives on and checks again once idle.
	peer.until(isPing)
}

// The PING timeout runs from when the PING is queued: waiting for the write
// slot isn't the peer's delay.
func TestClientHealthCheckTimesPingFromQueuing(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{
		ReadIdleTimeout: 20 * time.Millisecond,
		PingTimeout:     50 * time.Millisecond,
	})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	conn := poolConns(transport, hc)[0]
	conn.writeSem <- struct{}{}
	// Several health checks' worth: the idle check fires and its PING waits
	// for the slot.
	time.Sleep(200 * time.Millisecond)
	<-conn.writeSem
	isPing := func(frame peerFrame) bool { return frame.typ == xhttp2.FramePing && !frame.ack }
	peer.until(isPing)
	peer.until(isPing)
}

// A connection drained by GOAWAY frees the only MaxConns slot for the retry
// of the request it refused, with no MaxConnWaitTimeout to hide the wait.
func TestClientRetryAfterGoAwayUnderMaxConnsOne(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = 0
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	first := acceptPeer(t, ln)
	first.nextHeaders()
	first.write(func(framer *xhttp2.Framer) error { return framer.WriteGoAway(0, xhttp2.ErrCodeNo, nil) })
	second := acceptPeer(t, ln)
	request := second.nextHeaders()
	second.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("GET refused by GOAWAY wasn't retried: %v", err)
	}
}

// slowCloseConn holds Close until release is closed, as a TLS close_notify
// against a stalled peer would.
type slowCloseConn struct {
	net.Conn

	release <-chan struct{}
}

func (c *slowCloseConn) Close() error {
	<-c.release
	return c.Conn.Close()
}

// A retired connection holds its MaxConns slot until its socket is closed. A
// request arriving in that window waits for the slot instead of failing fast,
// also with MaxConnWaitTimeout zero: the retirement always completes.
func TestClientWaitsForRetiringConnectionUnderMaxConns(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = 0
	release := make(chan struct{})
	var dials atomic.Int64
	hc.Dial = func(addr string) (net.Conn, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil || dials.Add(1) != 1 {
			return conn, err
		}
		return &slowCloseConn{Conn: conn, release: release}, nil
	}
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	first := acceptPeer(t, ln)
	request := first.nextHeaders()
	first.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	conn := poolConns(transport, hc)[0]
	first.write(func(framer *xhttp2.Framer) error {
		return framer.WriteGoAway(request.streamID, xhttp2.ErrCodeNo, nil)
	})
	waitUntil(t, "the connection is retired", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.closed
	})
	if got := hc.ConnsCount(); got != 1 {
		t.Fatalf("ConnsCount() = %d while the socket is still closing, want 1", got)
	}

	done = doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	select {
	case err := <-done:
		close(release)
		t.Fatalf("Do() = %v before the retiring connection freed its slot", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	second := acceptPeer(t, ln)
	request = second.nextHeaders()
	second.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() after the retirement: %v", err)
	}
}

// GOAWAY on an idle connection races the next request for the only slot.
func TestClientRedialsAfterGoAwayUnderMaxConnsOne(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	hc.MaxConnWaitTimeout = 0
	for i := range 100 {
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		request := peer.nextHeaders()
		peer.write(func(framer *xhttp2.Framer) error {
			return framer.WriteGoAway(request.streamID, xhttp2.ErrCodeNo, nil)
		})
		peer.writeHeaders(request.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
}

// Meaningful under -race: the pool reads the connection limit SetMaxConns
// may change concurrently.
func TestClientReadsMaxConnsUnderLock(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			default:
				hc.SetMaxConns(10)
			}
		}
	}()
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200")
	err := waitResultOf(t, done)
	close(stop)
	<-finished
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
}

// pausingPeer answers every request with 200 and stops reading the socket
// while paused, as a peer that stalls for a while does.
type pausingPeer struct {
	pause  chan struct{}
	resume chan struct{}
}

func startPausingPeer(t *testing.T, ln net.Listener) *pausingPeer {
	t.Helper()
	peer := &pausingPeer{pause: make(chan struct{}), resume: make(chan struct{})}
	go func() {
		setAcceptDeadline(ln, time.Now().Add(5*time.Second))
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.ReadFull(conn, make([]byte, len(clientPreface))); err != nil {
			return
		}
		framer := xhttp2.NewFramer(conn, conn)
		framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
		var block bytes.Buffer
		encoder := hpack.NewEncoder(&block)
		_ = framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
		_ = framer.WriteWindowUpdate(0, 1<<31-1-65535)
		for {
			select {
			case <-peer.pause:
				<-peer.resume
			default:
			}
			frame, err := framer.ReadFrame()
			if err != nil {
				return
			}
			switch frame := frame.(type) {
			case *xhttp2.SettingsFrame:
				if !frame.IsAck() {
					_ = framer.WriteSettingsAck()
				}
			case *xhttp2.MetaHeadersFrame:
				block.Reset()
				_ = encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				_ = framer.WriteHeaders(xhttp2.HeadersFrameParam{
					StreamID:      frame.StreamID,
					BlockFragment: block.Bytes(),
					EndStream:     true,
					EndHeaders:    true,
				})
			}
		}
	}()
	return peer
}

// A request deadline bounds the waits a request can abandon, never the
// writer's queue: a short DoTimeout that lands in a full queue while the peer
// pauses must not fail the connection and the upload sharing it.
func TestRequestDeadlineDoesNotFailPausedConnection(t *testing.T) {
	ln := listenPeer(t)
	peer := startPausingPeer(t, ln)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/", nil, nil)); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	conn := poolConns(transport, hc)[0]
	close(peer.pause)

	// Fill the writer queue under the slot until it stays full: the socket
	// buffers are full and the paused peer drains nothing.
	conn.writeSem <- struct{}{}
	chunk := make([]byte, 16<<10)
	queueFull := func() bool { return len(conn.writer.queue) == cap(conn.writer.queue) }
	for deadline := time.Now().Add(10 * time.Second); ; {
		if time.Now().After(deadline) {
			<-conn.writeSem
			t.Fatal("the writer queue never filled")
		}
		if queueFull() {
			time.Sleep(100 * time.Millisecond)
			if queueFull() {
				break
			}
		}
		if err := conn.framer.WriteData(math.MaxInt32, false, chunk); err != nil {
			<-conn.writeSem
			t.Fatalf("WriteData() error: %v", err)
		}
		if err := conn.bufferedWriter.Flush(); err != nil {
			<-conn.writeSem
			t.Fatalf("Flush() error: %v", err)
		}
	}
	<-conn.writeSem

	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + hc.Addr + "/short")
		done <- hc.DoTimeout(req, resp, 50*time.Millisecond)
	}()
	time.Sleep(300 * time.Millisecond)
	conn.mu.Lock()
	closed, cause := conn.closed, conn.err
	conn.mu.Unlock()
	if closed {
		close(peer.resume)
		t.Fatalf("a 50ms request deadline failed the connection: %v", cause)
	}
	close(peer.resume)
	_ = waitResultOf(t, done)
	if err := waitResultOf(t, doAsync(hc, fasthttp.MethodGet, "/after", nil, nil)); err != nil {
		t.Fatalf("Do() after the peer resumed: %v", err)
	}
	if conns := poolConns(transport, hc); len(conns) != 1 || conns[0] != conn {
		t.Fatalf("the connection was replaced: %v", conns)
	}
}

// A request whose deadline already passed takes no write slot, even a free
// one: nothing of it reaches the wire.
func TestExpiredDeadlineTakesNoFreeWriteSlot(t *testing.T) {
	var wire bytes.Buffer
	bufferedWriter := bufio.NewWriter(&wire)
	conn := &clientConn{
		pool:           &clientPool{},
		hc:             &fasthttp.HostClient{},
		bufferedWriter: bufferedWriter,
		streams:        make(map[uint32]*clientStream),
		nextStreamID:   5,
		activeStreams:  2,
		connFlowState: connFlowState{
			peerMaxFrameSize:        defaultMaxFrameSize,
			peerMaxHeaderListSize:   1<<32 - 1,
			peerInitialStreamWindow: 65535,
			send:                    sendWindow{window: 65535},
			recv:                    recvWindow{window: 65535},
		},
	}
	installTestWriter(t, conn)
	conn.framer = xhttp2.NewFramer(bufferedWriter, nil)
	conn.initHeaderEncoder(defaultHeaderTableSize)
	req := &fasthttp.Request{}
	req.SetRequestURI("https://example.com/")
	stream := &clientStream{conn: conn, req: req, result: make(chan clientResult, 1)}
	err := conn.writeRequestHeaders(stream, true, req, nil, time.Now().Add(-time.Second))
	if !errors.Is(err, fasthttp.ErrTimeout) {
		t.Fatalf("writeRequestHeaders() past the deadline = %v, want timeout", err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if wire.Len() != 0 || stream.id != 0 || conn.nextStreamID != 5 {
		t.Fatalf("an expired request took the free write slot: wire=%d id=%d nextStreamID=%d",
			wire.Len(), stream.id, conn.nextStreamID)
	}
}

// A 204 or 304 doesn't mark the caller's Response SkipBody: it is reused for
// the next request, whose body HTTP/1 keeps.
func TestClientReusedResponseAfterBodylessStatusKeepsNextBody(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/empty" {
				ctx.SetStatusCode(fasthttp.StatusNoContent)
				return
			}
			ctx.SetBodyString("hello")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	run := func(t *testing.T, hc *fasthttp.HostClient) {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + addr + "/empty")
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("first Do() error: %v", err)
		}
		if resp.StatusCode() != fasthttp.StatusNoContent || resp.SkipBody {
			t.Fatalf("status = %d, SkipBody = %v", resp.StatusCode(), resp.SkipBody)
		}
		req.SetRequestURI("http://" + addr + "/body")
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("second Do() error: %v", err)
		}
		if got := string(resp.Body()); got != "hello" {
			t.Fatalf("second body = %q, want %q", got, "hello")
		}
	}
	t.Run("http1", func(t *testing.T) { run(t, &fasthttp.HostClient{Addr: addr}) })
	t.Run("http2", func(t *testing.T) { run(t, newPriorKnowledgeHostClient(t, addr)) })
}

// ackBeforeFlushReturn lets the peer answer the PING before the producer's
// Flush returns: a legal schedule the health check must survive.
type ackBeforeFlushReturn struct {
	flushWriter

	c *clientConn
	t *testing.T
}

func (w *ackBeforeFlushReturn) Flush() error {
	if err := w.flushWriter.Flush(); err != nil {
		return err
	}
	for until := time.Now().Add(time.Second); w.c.lastFrameAt.Load() < 0; {
		if time.Now().After(until) {
			w.t.Error("the peer didn't acknowledge the PING")
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func TestClientHealthCheckAcceptsAckBeforeWriteReturns(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{ReadIdleTimeout: time.Hour, PingTimeout: 30 * time.Millisecond})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	c := poolConns(transport, hc)[0]
	c.mu.Lock()
	c.healthTimer.Stop()
	c.mu.Unlock()
	c.writeSem <- struct{}{}
	c.bufferedWriter = &ackBeforeFlushReturn{flushWriter: c.bufferedWriter, c: c, t: t}
	c.lastFrameAt.Store(-int64(2 * time.Hour)) // idle for long: the check PINGs
	<-c.writeSem
	c.checkHealth()
	time.Sleep(100 * time.Millisecond) // past PingTimeout: the timer re-checks
	c.mu.Lock()
	closed, err := c.closed, c.err
	c.mu.Unlock()
	if closed {
		t.Fatalf("a connection whose peer acknowledged the PING was closed: %v", err)
	}
}

// Without a Content-Length on the wire ContentLength reports the body
// collected, as HTTP/1 does after reading an identity body; a streaming body
// stays -1 and HEAD keeps the length it was given.
func TestClientContentLengthWithoutWireLength(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	peer.writeData(request.streamID, true, []byte("hello"))
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := resp.Header.ContentLength(); got != -1 || string(resp.Body()) != "hello" {
		t.Fatalf("ContentLength() = %d with body %q, want -1", got, resp.Body())
	}

	resp.Reset()
	done = doAsync(hc, fasthttp.MethodHead, "/", nil, resp)
	request = peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "200", "content-length", "10")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("HEAD error: %v", err)
	}
	if got := resp.Header.ContentLength(); got != 10 || len(resp.Body()) != 0 {
		t.Fatalf("HEAD ContentLength() = %d with body %q, want 10", got, resp.Body())
	}

	resp.Reset()
	resp.StreamBody = true
	done = doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	request = peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("streaming Do() error: %v", err)
	}
	peer.writeData(request.streamID, true, []byte("hello"))
	if got := resp.Header.ContentLength(); got != -1 {
		t.Fatalf("streaming ContentLength() = %d, want -1", got)
	}
	if body, err := io.ReadAll(resp.BodyStream()); err != nil || string(body) != "hello" {
		t.Fatalf("streaming body = %q, %v", body, err)
	}
}

// echoRequestServer answers with what the server saw of the request.
func echoRequestServer(t *testing.T) *testServer {
	t.Helper()
	server := &fasthttp.Server{
		StreamRequestBody: false,
		Handler: func(ctx *fasthttp.RequestCtx) {
			header := &ctx.Request.Header
			ctx.SetBodyString(strings.Join([]string{
				string(header.Host()),
				string(header.Peek(fasthttp.HeaderAuthorization)),
				string(header.Peek(fasthttp.HeaderContentLength)),
				string(header.Peek("X-Sum")),
				string(ctx.Request.Body()),
			}, "|"))
		},
	}
	return newTestServer(t, server, ServerConfig{})
}

// The request goes out as Request.Write settles it: the body from PostArgs,
// the authority from the URI unless UseHostHeader, userinfo as Authorization
// and the Content-Length of the body sent.
func TestClientRequestEncodingMatchesHTTP1(t *testing.T) {
	testServer := echoRequestServer(t)
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		name string
		make func(req *fasthttp.Request)
		want string
	}{
		{"post args", func(req *fasthttp.Request) {
			req.SetRequestURI("http://" + addr + "/form")
			req.Header.SetMethod(fasthttp.MethodPost)
			req.Header.SetContentType("application/x-www-form-urlencoded")
			req.PostArgs().Set("k", "v")
		}, addr + "||3||k=v"},
		{"stale host", func(req *fasthttp.Request) {
			req.Header.SetHost("stale.example")
			req.SetRequestURI("http://" + addr + "/")
		}, addr + "||||"},
		{"host header kept", func(req *fasthttp.Request) {
			req.Header.SetHost("virtual.example")
			req.SetRequestURI("http://" + addr + "/")
			req.UseHostHeader = true
		}, "virtual.example||||"},
		{"userinfo", func(req *fasthttp.Request) {
			req.SetRequestURI("http://user:pass@" + addr + "/secret")
		}, addr + "|Basic dXNlcjpwYXNz|||"},
		{"stale content-length", func(req *fasthttp.Request) {
			req.SetRequestURI("http://" + addr + "/put")
			req.Header.SetMethod(fasthttp.MethodPut)
			req.SetBodyString("hello")
			req.Header.SetContentLength(2)
		}, addr + "||5||hello"},
		{"empty post", func(req *fasthttp.Request) {
			req.SetRequestURI("http://" + addr + "/empty")
			req.Header.SetMethod(fasthttp.MethodPost)
		}, addr + "||0||"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got [2]string
			for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				tc.make(req)
				if err := hc.Do(req, resp); err != nil {
					t.Fatalf("Do() error: %v", err)
				}
				got[i] = string(resp.Body())
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			}
			if got[0] != tc.want {
				t.Fatalf("HTTP/1 server saw %q, want %q", got[0], tc.want)
			}
			if got[1] != got[0] {
				t.Fatalf("HTTP/2 server saw %q, HTTP/1 saw %q", got[1], got[0])
			}
		})
	}
}

// Declared request trailers reach the server after buffered, streamed and
// empty bodies, as they do over HTTP/1.
func TestClientSendsRequestTrailers(t *testing.T) {
	testServer := echoRequestServer(t)
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		name string
		body func(req *fasthttp.Request)
		want string
	}{
		{"buffered", func(req *fasthttp.Request) { req.SetBodyString("payload") }, "payload"},
		{"streamed", func(req *fasthttp.Request) { req.SetBodyStream(strings.NewReader("streamed"), -1) }, "streamed"},
		{"empty", func(*fasthttp.Request) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				req.SetRequestURI("http://" + addr + "/trailer")
				req.Header.SetMethod(fasthttp.MethodPost)
				if err := req.Header.SetTrailer("X-Sum"); err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Sum", "42")
				tc.body(req)
				if err := hc.Do(req, resp); err != nil {
					t.Fatalf("Do() error: %v", err)
				}
				fields := strings.Split(string(resp.Body()), "|")
				if fields[3] != "42" || fields[4] != tc.want {
					t.Fatalf("server saw trailer %q and body %q, want 42 and %q", fields[3], fields[4], tc.want)
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			}
		})
	}
}

// On the wire a declared trailer keeps END_STREAM off the last DATA; the
// trailing HEADERS carries it, also when no trailer value was set.
func TestClientRequestTrailerFrames(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	var peer *clientPeer
	for _, value := range []string{"42", ""} {
		done := make(chan error, 1)
		go func() {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI("http://" + hc.Addr + "/trailer")
			req.Header.SetMethod(fasthttp.MethodPost)
			_ = req.Header.SetTrailer("X-Sum")
			if value != "" {
				req.Header.Set("X-Sum", value)
			}
			req.SetBodyString("payload")
			done <- hc.Do(req, resp)
		}()
		if peer == nil {
			peer = acceptPeer(t, ln)
		}
		headers := peer.nextHeaders()
		if headers.endStream || headers.header("trailer") != "X-Sum" || headers.header("x-sum") != "" {
			t.Fatalf("x-sum %q: HEADERS endStream=%v trailer=%q x-sum=%q",
				value, headers.endStream, headers.header("trailer"), headers.header("x-sum"))
		}
		frames := peer.until(func(frame peerFrame) bool { return frame.endStream })
		data, trailers := frames[0], frames[len(frames)-1]
		if data.typ != xhttp2.FrameData || data.endStream || string(data.data) != "payload" {
			t.Fatalf("x-sum %q: DATA = %+v, want the body without END_STREAM", value, data)
		}
		if trailers.typ != xhttp2.FrameHeaders || trailers.header("x-sum") != value {
			t.Fatalf("x-sum %q: trailing HEADERS = %+v, want it with END_STREAM", value, trailers)
		}
		peer.writeHeaders(headers.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("x-sum %q: Do() error: %v", value, err)
		}
	}
}

// openTunnel opens an extended CONNECT through hc against a clientPeer and
// returns the StreamConn once the peer answered 200.
func openTunnel(t *testing.T, hc *fasthttp.HostClient, ln net.Listener, peer **clientPeer) (fasthttp.StreamConn, uint32) {
	t.Helper()
	opened := make(chan fasthttp.StreamConn, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI("http://" + hc.Addr + "/ws")
		conn, err := hc.OpenStream(req, resp)
		if err != nil {
			t.Errorf("OpenStream() error: %v", err)
		}
		opened <- conn
	}()
	if *peer == nil {
		*peer = acceptPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1})
	}
	request := (*peer).nextHeaders()
	(*peer).writeHeaders(request.streamID, false, ":status", "200")
	conn := <-opened
	if conn == nil {
		t.FailNow()
	}
	return conn, request.streamID
}

// Close sends END_STREAM and waits a bounded time for the peer to end its
// side; a silent peer gets the stream reset, so the slot and the connection
// are freed. A peer that ends promptly isn't reset.
func TestClientStreamConnCloseBoundsSilentPeer(t *testing.T) {
	const grace = 100 * time.Millisecond
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true})
	var peer *clientPeer
	isDataEnd := func(frame peerFrame) bool { return frame.typ == xhttp2.FrameData && frame.endStream }

	// Silent peer.
	conn, streamID := openTunnel(t, hc, ln, &peer)
	c := poolConns(transport, hc)[0]
	c.mu.Lock()
	c.closeGrace = grace
	c.mu.Unlock()
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	peer.until(isDataEnd)
	frames := peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameRSTStream })
	if reset := frames[len(frames)-1]; reset.streamID != streamID || reset.errCode != xhttp2.ErrCodeCancel {
		t.Fatalf("reset = %+v, want CANCEL on stream %d", reset, streamID)
	}
	waitUntil(t, "the stream is released", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.activeStreams == 0
	})

	// Prompt peer, after a CloseWrite and then Close.
	conn, streamID = openTunnel(t, hc, ln, &peer)
	if err := conn.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}
	peer.until(isDataEnd)
	peer.writeData(streamID, true, nil)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	time.Sleep(3 * grace)
	if resets := countResets(peer.sync(), streamID); resets != 0 {
		t.Fatalf("a peer that ended its side was reset %d times", resets)
	}

	hc.CloseIdleConnections()
	waitUntil(t, "the connection is closed", func() bool { return hc.ConnsCount() == 0 })
}

// An accepted push whose response never comes is bounded by ReadTimeout, after
// which the connection goes idle; without a ReadTimeout CloseIdleConnections
// cancels it and closes the connection.
func TestClientBoundsUnansweredPush(t *testing.T) {
	t.Run("read timeout", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		hc.ReadTimeout = 100 * time.Millisecond
		hc.MaxIdleConnDuration = 50 * time.Millisecond
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		parent := peer.nextHeaders()
		promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
		peer.writeHeaders(parent.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		<-pushes.acceptedPath
		frames := peer.until(func(frame peerFrame) bool { return frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 })
		if reset := frames[len(frames)-1]; reset.errCode != xhttp2.ErrCodeCancel {
			t.Fatalf("unanswered push reset with %v, want CANCEL", reset.errCode)
		}
		waitUntil(t, "the idle connection is closed", func() bool { return hc.ConnsCount() == 0 })
	})
	t.Run("close idle", func(t *testing.T) {
		ln := listenPeer(t)
		pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
		hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		parent := peer.nextHeaders()
		promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
		peer.writeHeaders(parent.streamID, true, ":status", "200")
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		<-pushes.acceptedPath
		peer.sync()
		hc.CloseIdleConnections()
		if got := hc.ConnsCount(); got != 0 {
			t.Fatalf("ConnsCount() = %d after CloseIdleConnections with only an unanswered push, want 0", got)
		}
	})
}

// A streaming response the peer's TCP close cuts short reads as truncated,
// not as complete.
func TestClientStreamingResponseCutByPeerCloseIsTruncated(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.StreamResponseBody = true
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	peer.writeData(request.streamID, false, []byte("part"))
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	body := resp.BodyStream()
	if got, err := io.ReadFull(body, make([]byte, 4)); err != nil || got != 4 {
		t.Fatalf("ReadFull() = %d, %v", got, err)
	}
	peer.sync() // nothing of the client's is left unread: the close is clean
	_ = peer.conn.Close()
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read() after the peer closed = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

// A buffered request whose connection the peer closes before any response
// gets ErrConnectionClosed, as HTTP/1 reports it, already where the retry
// policy sees it.
func TestClientRequestCutByPeerCloseIsConnectionClosed(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	var offered error
	hc.RetryIfErr = func(_ *fasthttp.Request, _ int, err error) (bool, bool) {
		offered = err
		return false, false
	}
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	peer.nextHeaders()
	peer.sync() // nothing of the client's is left unread: the close is clean
	_ = peer.conn.Close()
	err := waitResultOf(t, done)
	if !errors.Is(err, fasthttp.ErrConnectionClosed) || !errors.Is(offered, fasthttp.ErrConnectionClosed) {
		t.Fatalf("Do() = %v, offered %v to RetryIfErr; want %v", err, offered, fasthttp.ErrConnectionClosed)
	}
}

// A header name is lowered as ASCII only: U+212A (KELVIN SIGN) must not turn
// into "k" and go out as a valid name.
func TestClientRejectsNonASCIIHeaderName(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + hc.Addr + "/")
		req.Header.Set("X-Key", "v")
		done <- hc.Do(req, resp)
	}()
	acceptPeer(t, ln)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Do() = nil, want an invalid header name error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request with a U+212A header name went out")
	}
}

// A bodied request without a Content-Type goes out with HTTP/1's default,
// application/octet-stream, unless NoDefaultContentType; a GET gets none.
// The default Content-Type reaches the wire as over HTTP/1 and, as there, is
// never stored in the caller's header; an explicit type and
// NoDefaultContentType are honored.
func TestClientDefaultContentTypeMatchesHTTP1(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString(string(ctx.Request.Header.ContentType()))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		name string
		make func(req *fasthttp.Request)
		want string // on the wire
		kept string // in the caller's header after Do
	}{
		{"post", func(req *fasthttp.Request) {
			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetBodyString("payload")
		}, "application/octet-stream", ""},
		{"post without default", func(req *fasthttp.Request) {
			req.Header.SetMethod(fasthttp.MethodPost)
			req.Header.SetNoDefaultContentType(true)
			req.SetBodyString("payload")
		}, "", ""},
		{"post with explicit type", func(req *fasthttp.Request) {
			req.Header.SetMethod(fasthttp.MethodPost)
			req.Header.SetContentType("text/plain")
			req.SetBodyString("payload")
		}, "text/plain", "text/plain"},
		{"sized stream", func(req *fasthttp.Request) {
			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetBodyStream(strings.NewReader("payload"), 7)
		}, "application/octet-stream", ""},
		{"unsized stream", func(req *fasthttp.Request) {
			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetBodyStream(strings.NewReader("payload"), -1)
		}, "", ""},
		{"get", func(*fasthttp.Request) {}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				req.SetRequestURI("http://" + addr + "/")
				tc.make(req)
				if err := hc.Do(req, resp); err != nil {
					t.Fatalf("protocol %d: Do() error: %v", i+1, err)
				}
				if got := string(resp.Body()); got != tc.want {
					t.Fatalf("protocol %d: server saw Content-Type %q, want %q", i+1, got, tc.want)
				}
				if got := string(req.Header.ContentType()); got != tc.kept {
					t.Fatalf("protocol %d: Content-Type in the request header after Do = %q, want %q", i+1, got, tc.kept)
				}
				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(resp)
			}
		})
	}
}

// A request reused after its body stream was consumed goes out as the
// bodyless request HTTP/1 sends: no stored default Content-Type follows it.
func TestClientReusedRequestAfterStreamMatchesHTTP1(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString(fmt.Sprintf("%s|%d|%s",
				ctx.Request.Header.ContentType(), ctx.Request.Header.ContentLength(), ctx.Request.Body()))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	var views [2][2]string
	for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetRequestURI("http://" + addr + "/")
		req.SetBodyStream(strings.NewReader("body"), 4)
		for attempt := range 2 {
			if err := hc.Do(req, resp); err != nil {
				t.Fatalf("protocol %d attempt %d: Do() error: %v", i+1, attempt+1, err)
			}
			views[i][attempt] = string(resp.Body())
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	if views[0] != views[1] {
		t.Fatalf("HTTP/1 server saw %q, HTTP/2 server saw %q", views[0], views[1])
	}
	if views[1][0] != "application/octet-stream|4|body" || views[1][1] != "|0|" {
		t.Fatalf("server saw %q", views[1])
	}
}

type parentContentTypePushHandler struct {
	parentContentType chan string
}

func (h *parentContentTypePushHandler) Accept(parent, _ *fasthttp.Request) bool {
	h.parentContentType <- string(parent.Header.ContentType())
	return true
}

func (*parentContentTypePushHandler) Handle(*fasthttp.Request, *fasthttp.Response) {}

// The parent request a PushHandler sees is the caller's: the wire-only
// default Content-Type isn't in it.
func TestClientPushParentKeepsCallerHeader(t *testing.T) {
	ln := listenPeer(t)
	pushes := &parentContentTypePushHandler{parentContentType: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	done := doAsync(hc, fasthttp.MethodPost, "/", []byte("payload"), nil)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	if got := parent.header("content-type"); got != "application/octet-stream" {
		t.Fatalf("content-type on the wire = %q, want the default", got)
	}
	promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
	select {
	case got := <-pushes.parentContentType:
		if got != "" {
			t.Fatalf("parent Content-Type seen by PushHandler.Accept = %q, want none", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the promise didn't reach PushHandler.Accept")
	}
	peer.writeHeaders(2, true, ":status", "204")
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
}

// A body read after the peer's END_STREAM returns connection credit only:
// no WINDOW_UPDATE goes out on a stream the peer ended (RFC 9113 §6.9).
func TestClientReadAfterEndStreamCreditsOnlyConnection(t *testing.T) {
	const window, frameSize = 65535, 16000
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{
		MaxResponseBufferPerConnection: window,
		MaxResponseBufferPerStream:     window,
	})
	hc.StreamResponseBody = true
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	for i := range 3 {
		peer.writeData(request.streamID, i == 2, make([]byte, frameSize))
	}
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	peer.sync() // END_STREAM is processed before anything is read
	if body, err := io.ReadAll(resp.BodyStream()); err != nil || len(body) != 3*frameSize {
		t.Fatalf("ReadAll() = %d bytes, %v", len(body), err)
	}
	isStreamUpdate := func(frame peerFrame) bool {
		return frame.typ == xhttp2.FrameWindowUpdate && frame.streamID == request.streamID
	}
	credit := 0
	for credit == 0 {
		frame := peer.next()
		if isStreamUpdate(frame) {
			t.Fatalf("WINDOW_UPDATE(%d) on stream %d after the peer ended it", frame.increment, frame.streamID)
		}
		if frame.typ == xhttp2.FrameWindowUpdate && frame.streamID == 0 {
			credit += int(frame.increment)
		}
	}
	for _, frame := range peer.sync() {
		if isStreamUpdate(frame) {
			t.Fatalf("WINDOW_UPDATE(%d) on stream %d after the peer ended it", frame.increment, frame.streamID)
		}
	}
}

// Credit the sender hasn't written yet isn't the peer's to spend: late DATA on
// a closed stream, while another producer holds the write slot, must still
// stay inside the window the peer was told about.
func TestClientUnsentCreditIsNotSpendable(t *testing.T) {
	const window = 65535
	c := installTestWriter(t, &clientConn{
		streams:       make(map[uint32]*clientStream),
		nextStreamID:  3,
		config:        clientConfig{connectionWindowSize: window, streamWindowSize: window},
		connFlowState: connFlowState{recv: recvWindow{window: window}},
	})
	c.writeSem <- struct{}{} // nothing goes out
	defer func() { <-c.writeSem }()
	frame := makeClientTestDataFrame(t, 1, make([]byte, 16384))
	for i := range 3 {
		if err := c.processResponseData(frame); err != nil {
			t.Fatalf("DATA %d inside the window: %v", i+1, err)
		}
	}
	err := c.processResponseData(frame)
	if code, ok := goAwayCode(err); !ok || code != xhttp2.ErrCodeFlowControl {
		t.Fatalf("DATA past the window = %v, want a FLOW_CONTROL_ERROR connection error", err)
	}
}

// Recorded credit grows the windows once the sender commits it, which it can
// only do when the write slot frees.
func TestClientCreditIsGrantedWhenCommitted(t *testing.T) {
	const window = 100
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	c := installTestWriter(t, &clientConn{
		conn:    local,
		streams: make(map[uint32]*clientStream),
		config:  clientConfig{connectionWindowSize: window, streamWindowSize: window},
	})
	stream := &clientStream{id: 3, conn: c, requestStarted: true, responseHeader: true}
	c.streams[3] = stream
	c.writeSem <- struct{}{}
	c.consumeResponseBytes(3, window) // both windows are spent: an increment each
	c.mu.Lock()
	connWindow, streamWindow := c.recv.window, stream.recv.window
	c.mu.Unlock()
	if connWindow != 0 || streamWindow != 0 {
		<-c.writeSem
		t.Fatalf("windows grew to %d and %d before the WINDOW_UPDATE was committed", connWindow, streamWindow)
	}
	<-c.writeSem
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	framer := xhttp2.NewFramer(nil, remote)
	credited := map[uint32]uint32{}
	for len(credited) < 2 {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("reading the client's frames: %v (credited %v)", err, credited)
		}
		if update, ok := frame.(*xhttp2.WindowUpdateFrame); ok {
			credited[update.StreamID] += update.Increment
		}
	}
	if credited[0] != window || credited[3] != window {
		t.Fatalf("credit written = %v, want %d on the connection and on stream 3", credited, window)
	}
	waitUntil(t, "the windows are granted", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.recv.window == window && stream.recv.window == window
	})
}

// pauseAfterFlush holds the producer in Flush after its bytes are enqueued,
// as a sender descheduled right after committing a frame.
type pauseAfterFlush struct {
	flushWriter

	once   sync.Once
	queued chan struct{}
	resume chan struct{}
}

// Flush pauses after the first flush only; a later one, such as the credit
// the test's own DATA queues, passes through.
func (w *pauseAfterFlush) Flush() error {
	err := w.flushWriter.Flush()
	w.once.Do(func() {
		close(w.queued)
		<-w.resume
	})
	return err
}

// Credit is granted as the WINDOW_UPDATE is committed: the peer may spend it
// as soon as the frame reaches it, before the sender runs again.
func TestClientPeerMaySpendCommittedCredit(t *testing.T) {
	const window = 65535
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	c := installTestWriter(t, &clientConn{
		conn:         local,
		streams:      make(map[uint32]*clientStream),
		nextStreamID: 3,
		config:       clientConfig{connectionWindowSize: window, streamWindowSize: window},
	})
	paused := &pauseAfterFlush{flushWriter: c.writer, queued: make(chan struct{}), resume: make(chan struct{})}
	t.Cleanup(func() { close(paused.resume) })
	c.bufferedWriter = paused
	c.framer = xhttp2.NewFramer(paused, local)

	// The peer spent the whole window on a stream that is closed by now.
	c.restoreConnectionWindow(window)
	<-paused.queued
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	frame, err := xhttp2.NewFramer(nil, remote).ReadFrame()
	if err != nil {
		t.Fatalf("reading the client's frame: %v", err)
	}
	if update, ok := frame.(*xhttp2.WindowUpdateFrame); !ok || update.StreamID != 0 || update.Increment != window {
		t.Fatalf("frame = %v, want WINDOW_UPDATE(0, %d)", frame, window)
	}
	// Received, so the peer may spend it, while the sender is still parked.
	if err := c.processResponseData(makeClientTestDataFrame(t, 1, []byte("x"))); err != nil {
		t.Fatalf("DATA inside the credit the peer received: %v", err)
	}
}

// A failed connection arms no idle timer: fail stops it before failing the
// streams, and finalizing the last stream must not bring it back, or the
// closed connection stays retained until MaxIdleConnDuration.
func TestClientFailedConnectionArmsNoIdleTimer(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxIdleConnDuration = time.Hour
	hc.RetryIfErr = func(*fasthttp.Request, int, error) (bool, bool) { return false, false }
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	peer.nextHeaders()
	conn := poolConns(transport, hc)[0]
	_ = peer.conn.Close()
	if err := waitResultOf(t, done); err == nil {
		t.Fatal("Do() succeeded on a closed connection")
	}
	waitUntil(t, "the connection failed", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return conn.closed
	})
	conn.mu.Lock()
	armed := conn.idleTimer != nil && conn.idleTimer.Stop()
	conn.mu.Unlock()
	if armed {
		t.Fatal("a failed connection armed its idle timer")
	}
}

// A response Date reaches the caller as a field, as HTTP/1's parser keeps it.
func TestClientResponseDateHeaderParity(t *testing.T) {
	const date = "Sun, 01 Nov 2026 10:00:00 GMT"
	var h1 fasthttp.Response
	if err := h1.Read(bufio.NewReader(strings.NewReader(
		"HTTP/1.1 200 OK\r\nDate: " + date + "\r\nContent-Length: 2\r\n\r\nok"))); err != nil {
		t.Fatal(err)
	}
	if got := string(h1.Header.Peek(fasthttp.HeaderDate)); got != date {
		t.Fatalf("HTTP/1 Date = %q", got)
	}

	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200", "date", date, "server", "peer")
	peer.writeData(request.streamID, true, []byte("ok"))
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if got := string(resp.Header.Peek(fasthttp.HeaderDate)); got != date {
		t.Fatalf("HTTP/2 Date = %q, want %q", got, date)
	}
	if got := string(resp.Header.Peek(fasthttp.HeaderServer)); got != "peer" {
		t.Fatalf("Server = %q, want peer", got)
	}
}

// The HTTP/1 and HTTP/2 clients see the same response header set from one
// server, including a Date.
func TestClientResponseHeadersMatchHTTP1(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			h := &ctx.Response.Header
			h.Set("X-One", "1")
			h.Add("X-Multi", "a")
			h.Add("X-Multi", "b")
			h.Set("X-Empty", "")
			h.SetServer("parity")
			h.SetContentEncoding("identity")
			var cookie fasthttp.Cookie
			cookie.SetKey("sid")
			cookie.SetValue("1")
			h.SetCookie(&cookie)
			h.Set(fasthttp.HeaderLastModified, "Sun, 01 Nov 2026 00:00:00 GMT")
			ctx.SetBodyString("hello")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	var views, dates [2]string
	for i, hc := range []*fasthttp.HostClient{{Addr: addr}, newPriorKnowledgeHostClient(t, addr)} {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.SetRequestURI("http://" + addr + "/")
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		var lines []string
		for key, value := range resp.Header.All() {
			switch name := strings.ToLower(string(key)); name {
			case "date":
				dates[i] = string(value)
			case "connection":
			default:
				lines = append(lines, name+": "+string(value))
			}
		}
		slices.Sort(lines)
		views[i] = strings.Join(lines, "\n") + "\nstatus: " + strconv.Itoa(resp.StatusCode()) + "\nbody: " + string(resp.Body())
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
	if views[0] != views[1] {
		t.Errorf("HTTP/1 client view:\n%s\nHTTP/2 client view:\n%s", views[0], views[1])
	}
	if dates[0] == "" || dates[1] == "" {
		t.Errorf("Date seen by the HTTP/1 client = %q, by the HTTP/2 client = %q", dates[0], dates[1])
	}
}

// A response's StatusMessage() is HTTP/1's reason phrase over HTTP/2 too,
// the fallback text for an unknown code included.
func TestClientStatusMessageMatchesHTTP1(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		code, _ := strconv.Atoi(strings.TrimPrefix(string(ctx.Path()), "/"))
		ctx.SetStatusCode(code)
		if code != fasthttp.StatusNoContent {
			ctx.SetBodyString("x")
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	h1 := &fasthttp.HostClient{Addr: addr}
	t.Cleanup(h1.CloseIdleConnections)
	h2 := newPriorKnowledgeHostClient(t, addr)
	for _, tc := range []struct {
		code int
		want string
	}{{200, "OK"}, {204, "No Content"}, {404, "Not Found"}, {599, "Unknown Status Code"}} {
		for i, hc := range []*fasthttp.HostClient{h1, h2} {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			req.SetRequestURI("http://" + addr + "/" + strconv.Itoa(tc.code))
			if err := hc.Do(req, resp); err != nil {
				t.Fatalf("%d protocol %d: Do() error: %v", tc.code, i+1, err)
			}
			if got := string(resp.Header.StatusMessage()); got != tc.want {
				t.Fatalf("%d protocol %d: StatusMessage() = %q, want %q", tc.code, i+1, got, tc.want)
			}
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
		}
	}
}

// Response Trailer declarations follow HTTP/1's parser: a forbidden name
// fails the response, an upfront field of a declared name stays in the header
// section, and repeated declarations accumulate.
func TestClientResponseTrailerDeclarationMatchesHTTP1(t *testing.T) {
	view := func(resp *fasthttp.Response) string {
		body := string(resp.Body()) // reads a streamed body to its trailers
		var lines []string
		for key, value := range resp.Header.All() {
			switch name := strings.ToLower(string(key)); name {
			case "date", "connection":
			default:
				lines = append(lines, name+": "+string(value))
			}
		}
		slices.Sort(lines)
		keys := make([]string, 0, len(resp.Header.PeekTrailerKeys()))
		for _, key := range resp.Header.PeekTrailerKeys() {
			keys = append(keys, string(key))
		}
		slices.Sort(keys)
		return strings.Join(lines, "\n") + "\ntrailer keys: " + strings.Join(keys, ",") +
			"\nx-check: " + string(resp.Header.Peek("X-Check")) + "\nbody: " + body
	}
	for _, tc := range []struct {
		name string
		h1   string
		h2   func(peer *clientPeer, streamID uint32)
		want func(t *testing.T, h2 bool, err error, view string)
	}{
		{
			name: "forbidden name",
			h1:   "HTTP/1.1 200 OK\r\nTrailer: Content-Length\r\nContent-Length: 2\r\n\r\nok",
			h2: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, false, ":status", "200", "trailer", "content-length", "content-length", "2")
				peer.writeData(streamID, true, []byte("ok"))
			},
			want: func(t *testing.T, h2 bool, err error, _ string) {
				if !errors.Is(err, fasthttp.ErrBadTrailer) {
					t.Fatalf("Do() error = %v, want ErrBadTrailer", err)
				}
				if h2 && !errors.Is(err, errInvalidResponseHeaders) {
					t.Fatalf("Do() error = %v, want a malformed response", err)
				}
			},
		},
		{
			name: "upfront field of a declared name",
			h1: "HTTP/1.1 200 OK\r\nX-Check: initial\r\nTrailer: X-Check\r\nTransfer-Encoding: chunked\r\n\r\n" +
				"2\r\nok\r\n0\r\nX-Check: tv\r\n\r\n",
			h2: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, false, ":status", "200", "x-check", "initial", "trailer", "x-check")
				peer.writeData(streamID, false, []byte("ok"))
				peer.writeHeaders(streamID, true, "x-check", "tv")
			},
			want: func(t *testing.T, _ bool, err error, view string) {
				if err != nil {
					t.Fatalf("Do() error: %v", err)
				}
				if !strings.Contains(view, "\nx-check: initial\n") || !strings.Contains(view, "\ntrailer keys: X-Check\n") {
					t.Fatalf("view:\n%s", view)
				}
			},
		},
		{
			name: "declarations accumulate",
			h1: "HTTP/1.1 200 OK\r\nTrailer: X-A\r\nTrailer: X-B\r\nTransfer-Encoding: chunked\r\n\r\n" +
				"2\r\nok\r\n0\r\nX-A: a\r\nX-B: b\r\n\r\n",
			h2: func(peer *clientPeer, streamID uint32) {
				peer.writeHeaders(streamID, false, ":status", "200", "trailer", "x-a", "trailer", "x-b")
				peer.writeData(streamID, false, []byte("ok"))
				peer.writeHeaders(streamID, true, "x-a", "a", "x-b", "b")
			},
			want: func(t *testing.T, _ bool, err error, view string) {
				if err != nil {
					t.Fatalf("Do() error: %v", err)
				}
				if !strings.Contains(view, "\ntrailer keys: X-A,X-B\n") {
					t.Fatalf("view:\n%s", view)
				}
			},
		},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/streaming=%v", tc.name, streaming), func(t *testing.T) {
				var views [2]string
				for i, h2 := range []bool{false, true} {
					ln := listenPeer(t)
					hc := &fasthttp.HostClient{Addr: ln.Addr().String(), StreamResponseBody: streaming, MaxIdemponentCallAttempts: 1}
					if h2 {
						if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
							t.Fatalf("ConfigureHostClient() error: %v", err)
						}
					}
					t.Cleanup(hc.CloseIdleConnections)
					resp := fasthttp.AcquireResponse()
					t.Cleanup(func() { fasthttp.ReleaseResponse(resp) })
					done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
					if h2 {
						peer := acceptPeer(t, ln)
						tc.h2(peer, peer.nextHeaders().streamID)
					} else {
						serveHTTP1Attempts(t, ln, tc.h1)
					}
					err := waitResultOf(t, done)
					if err == nil {
						views[i] = view(resp)
					}
					tc.want(t, h2, err, views[i])
				}
				if views[0] != views[1] {
					t.Fatalf("HTTP/1 client view:\n%s\nHTTP/2 client view:\n%s", views[0], views[1])
				}
			})
		}
	}
}

// A promised request that indicates content is reset with PROTOCOL_ERROR
// (RFC 9113 §8.4).
func TestClientRejectsPushWithRequestContent(t *testing.T) {
	for _, tc := range []struct {
		contentLength string
		accepted      bool
	}{
		{"1", false},
		{"01", false},
		{"0", true},
		{"00", true},
	} {
		t.Run("content-length="+tc.contentLength, func(t *testing.T) {
			ln := listenPeer(t)
			pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
			hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
			done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
			peer := acceptPeer(t, ln)
			parent := peer.nextHeaders()
			peer.wmu.Lock()
			err := peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
				StreamID: parent.streamID, PromiseID: 2, EndHeaders: true,
				BlockFragment: peer.encode(":method", "GET", ":scheme", "http", ":authority", parent.header(":authority"), ":path", "/pushed", "content-length", tc.contentLength),
			})
			peer.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			frames := peer.sync()
			peer.writeHeaders(parent.streamID, true, ":status", "200")
			if err := waitResultOf(t, done); err != nil {
				t.Fatal(err)
			}
			reset := false
			for _, frame := range frames {
				if frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 && frame.errCode == xhttp2.ErrCodeProtocol {
					reset = true
				}
			}
			if tc.accepted {
				if reset {
					t.Fatal("a promised request without content was reset with PROTOCOL_ERROR")
				}
				select {
				case <-pushes.acceptedPath:
				case <-time.After(2 * time.Second):
					t.Fatal("the promise didn't reach PushHandler.Accept")
				}
				return
			}
			if reset {
				return
			}
			select {
			case path := <-pushes.acceptedPath:
				t.Fatalf("the promise reached PushHandler.Accept for %q", path)
			default:
				t.Fatal("no PROTOCOL_ERROR reset for a promised request with content")
			}
		})
	}
}

// parkingPeer is a raw peer that reads only when asked, so a client producer
// can park in the writer queue against it.
type parkingPeer struct {
	conn    net.Conn
	framer  *xhttp2.Framer
	encoder *hpack.Encoder
	block   bytes.Buffer
}

func acceptParkingPeer(t *testing.T, ln net.Listener, settings ...xhttp2.Setting) *parkingPeer {
	t.Helper()
	setAcceptDeadline(ln, time.Now().Add(5*time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.ReadFull(conn, make([]byte, len(clientPreface))); err != nil {
		t.Fatalf("reading preface: %v", err)
	}
	peer := &parkingPeer{conn: conn, framer: xhttp2.NewFramer(conn, bufio.NewReader(conn))}
	peer.framer.ReadMetaHeaders = hpack.NewDecoder(defaultHeaderTableSize, nil)
	peer.encoder = hpack.NewEncoder(&peer.block)
	if err := peer.framer.WriteSettings(settings...); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	return peer
}

// nextHeaders reads up to the next HEADERS, acknowledging SETTINGS on the way.
func (p *parkingPeer) nextHeaders(t *testing.T) *xhttp2.MetaHeadersFrame {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = p.conn.SetReadDeadline(time.Time{}) }()
	for {
		frame, err := p.framer.ReadFrame()
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
		switch frame := frame.(type) {
		case *xhttp2.SettingsFrame:
			if !frame.IsAck() {
				_ = p.framer.WriteSettingsAck()
			}
		case *xhttp2.MetaHeadersFrame:
			return frame
		}
	}
}

func (p *parkingPeer) writeHeaders(t *testing.T, streamID uint32, endStream bool, fields ...string) {
	t.Helper()
	p.block.Reset()
	for i := 0; i+1 < len(fields); i += 2 {
		_ = p.encoder.WriteField(hpack.HeaderField{Name: fields[i], Value: fields[i+1]})
	}
	if err := p.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: bytes.Clone(p.block.Bytes()),
		EndStream:     endStream,
		EndHeaders:    true,
	}); err != nil {
		t.Fatalf("WriteHeaders() error: %v", err)
	}
}

// A producer parked in the writer queue holds the write slot. The read loop
// never waits for it: a PING from the peer must not stall every other
// stream's response until WriteByteTimeout.
func TestClientReadLoopNeverWaitsForTheWriteSlot(t *testing.T) {
	for _, ping := range []bool{false, true} {
		t.Run(fmt.Sprintf("ping=%v", ping), func(t *testing.T) {
			ln := listenPeer(t)
			hc, _ := newPeerClient(t, ln, &ClientConfig{WriteByteTimeout: 3 * time.Second})
			hc.RetryIfErr = func(*fasthttp.Request, int, error) (bool, bool) { return false, false }
			const window = 64 << 20
			warm := doAsync(hc, fasthttp.MethodGet, "/warm", nil, nil)
			peer := acceptParkingPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: window})
			if err := peer.framer.WriteWindowUpdate(0, window); err != nil {
				t.Fatal(err)
			}
			headers := peer.nextHeaders(t)
			peer.writeHeaders(t, headers.StreamID, true, ":status", "204")
			if err := waitResultOf(t, warm); err != nil {
				t.Fatalf("warm-up Do() error: %v", err)
			}
			get := doAsync(hc, fasthttp.MethodGet, "/get", nil, nil)
			getHeaders := peer.nextHeaders(t)
			// An upload the peer never reads: its producer parks in the queue.
			upload := doAsync(hc, fasthttp.MethodPost, "/up", make([]byte, 24<<20), nil)
			time.Sleep(500 * time.Millisecond)
			if ping {
				if err := peer.framer.WritePing(false, [8]byte{'p', 'a', 'r', 'k', 'e', 'd'}); err != nil {
					t.Fatal(err)
				}
			}
			peer.writeHeaders(t, getHeaders.StreamID, true, ":status", "200", "content-length", "0")
			select {
			case err := <-get:
				if err != nil {
					t.Fatalf("GET with its response on the wire: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("GET with its response on the wire didn't complete within 2s")
			}
			_ = peer.conn.Close()
			<-upload
		})
	}
}

// A dial error reaches the caller as HTTP/1 returns it, comparable with ==.
func TestClientDialErrorKeepsIdentity(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			hc := &fasthttp.HostClient{
				Addr: "127.0.0.1:1",
				Dial: func(string) (net.Conn, error) { return nil, fasthttp.ErrDialTimeout },
			}
			if h2 {
				if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
					t.Fatalf("ConfigureHostClient() error: %v", err)
				}
			}
			t.Cleanup(hc.CloseIdleConnections)
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI("http://127.0.0.1:1/")
			if err := hc.Do(req, resp); err != fasthttp.ErrDialTimeout { //nolint:errorlint // identity is the contract
				t.Fatalf("Do() error = %v, want ErrDialTimeout itself", err)
			}
		})
	}
}

// MaxResponseBodySize bounds buffered responses only: a streamed body reaches
// the caller whole, as over HTTP/1, while the buffering helpers keep the limit.
func TestClientMaxResponseBodySizeIgnoresStreamedResponse(t *testing.T) {
	const size = 8192
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) == "/unknown" {
			ctx.SetBodyStream(bytes.NewReader(bytes.Repeat([]byte("x"), size)), -1)
			return
		}
		ctx.SetBody(bytes.Repeat([]byte("x"), size))
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	for _, h2 := range []bool{false, true} {
		for _, path := range []string{"/declared", "/unknown"} {
			t.Run(fmt.Sprintf("h2=%v%s", h2, path), func(t *testing.T) {
				hc := &fasthttp.HostClient{Addr: addr, StreamResponseBody: true, MaxResponseBodySize: 1024}
				if h2 {
					if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
						t.Fatalf("ConfigureHostClient() error: %v", err)
					}
				}
				t.Cleanup(hc.CloseIdleConnections)
				req := fasthttp.AcquireRequest()
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(resp)
				req.SetRequestURI("http://" + addr + path)
				if err := hc.Do(req, resp); err != nil {
					t.Fatalf("streaming Do() error: %v", err)
				}
				body, err := io.ReadAll(resp.BodyStream())
				if err != nil || len(body) != size {
					t.Fatalf("streamed body = %d bytes, %v; want %d, nil", len(body), err, size)
				}
				_ = resp.CloseBodyStream()
				if _, _, err := hc.Get(nil, "http://"+addr+path); !errors.Is(err, fasthttp.ErrBodyTooLarge) {
					t.Fatalf("Get() error = %v, want ErrBodyTooLarge", err)
				}
			})
		}
	}
}

// A pushed response is buffered for its handler, so MaxResponseBodySize
// applies to it whatever StreamResponseBody says; the streamed parent is
// unbounded.
func TestClientPushedResponseKeepsMaxResponseBodySize(t *testing.T) {
	const size = 8192
	ln := listenPeer(t)
	pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
	hc.StreamResponseBody = true
	hc.MaxResponseBodySize = 1024
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	peer := acceptPeer(t, ln)
	parent := peer.nextHeaders()
	promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
	select {
	case <-pushes.acceptedPath:
	case <-time.After(5 * time.Second):
		t.Fatal("the promise didn't reach PushHandler.Accept")
	}
	peer.writeHeaders(2, false, ":status", "200", "content-length", strconv.Itoa(size))
	frames := peer.sync()
	if countResets(frames, 2) != 1 {
		t.Fatalf("pushed response above the limit: %d resets, want RST_STREAM", countResets(frames, 2))
	}
	peer.writeHeaders(parent.streamID, false, ":status", "200", "content-length", strconv.Itoa(size))
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	peer.writeData(parent.streamID, true, bytes.Repeat([]byte("x"), size))
	if body, err := io.ReadAll(resp.BodyStream()); err != nil || len(body) != size {
		t.Fatalf("streamed body = %d bytes, %v; want %d, nil", len(body), err, size)
	}
	select {
	case body := <-pushes.responseBody:
		t.Fatalf("the pushed response was delivered: %d bytes", len(body))
	default:
	}
}

// The response names its peer before the request goes out, as HTTP/1's does:
// RetryIfErrUpstream sees the address of a connection that failed before
// answering.
func TestClientRetryIfErrUpstreamSeesPeerBeforeResponse(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			ln := listenPeer(t)
			hc := &fasthttp.HostClient{Addr: ln.Addr().String()}
			if h2 {
				if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
					t.Fatalf("ConfigureHostClient() error: %v", err)
				}
			}
			t.Cleanup(hc.CloseIdleConnections)
			upstream := make(chan string, 1)
			hc.RetryIfErrUpstream = func(_ *fasthttp.Request, _ int, _ error, addr string) (bool, bool) {
				upstream <- addr
				return false, false
			}
			done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
			if h2 {
				peer := acceptPeer(t, ln)
				peer.nextHeaders()
				peer.sync()
				_ = peer.conn.Close()
			} else {
				conn, err := ln.Accept()
				if err != nil {
					t.Fatal(err)
				}
				var req fasthttp.Request
				if err := req.Read(bufio.NewReader(conn)); err != nil {
					t.Fatal(err)
				}
				_ = conn.Close()
			}
			if err := waitResultOf(t, done); err == nil {
				t.Fatal("Do() succeeded without a response")
			}
			select {
			case addr := <-upstream:
				if addr != hc.Addr {
					t.Fatalf("RetryIfErrUpstream upstream = %q, want %q", addr, hc.Addr)
				}
			default:
				t.Fatal("RetryIfErrUpstream wasn't consulted")
			}
		})
	}
}

type addrPushHandler struct{ addrs chan string }

func (h *addrPushHandler) Accept(_, _ *fasthttp.Request) bool { return true }

func (h *addrPushHandler) Handle(_ *fasthttp.Request, response *fasthttp.Response) {
	h.addrs <- response.RemoteAddr().String()
}

// Responses that don't pass through Do, OpenStream's and a pushed one, carry
// the connection's addresses as well.
func TestClientOpenStreamAndPushResponsesCarryAddrs(t *testing.T) {
	ln := listenPeer(t)
	pushes := &addrPushHandler{addrs: make(chan string, 1)}
	hc, _ := newPeerClient(t, ln, &ClientConfig{EnableExtendedConnect: true, PushHandler: pushes})
	var peer *clientPeer
	type opened struct {
		conn fasthttp.StreamConn
		resp *fasthttp.Response
	}
	tunnel := make(chan opened, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		req.Header.SetMethod(fasthttp.MethodConnect)
		req.Header.SetConnectProtocol("websocket")
		req.SetRequestURI("http://" + hc.Addr + "/ws")
		conn, err := hc.OpenStream(req, resp)
		if err != nil {
			t.Errorf("OpenStream() error: %v", err)
		}
		tunnel <- opened{conn: conn, resp: resp}
	}()
	peer = acceptPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingEnableConnectProtocol, Val: 1})
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, false, ":status", "200")
	got := <-tunnel
	if got.conn == nil {
		t.FailNow()
	}
	if got.resp.RemoteAddr() == nil || got.resp.RemoteAddr().String() != hc.Addr {
		t.Fatalf("OpenStream response RemoteAddr() = %v, want %q", got.resp.RemoteAddr(), hc.Addr)
	}
	_ = got.conn.Close()

	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	parent := peer.nextHeaders()
	promisePush(peer, parent.streamID, 2, fasthttp.MethodGet, parent.header(":authority"), "/pushed")
	peer.writeHeaders(2, true, ":status", "200")
	peer.writeHeaders(parent.streamID, true, ":status", "200")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	select {
	case addr := <-pushes.addrs:
		if addr != hc.Addr {
			t.Fatalf("pushed response RemoteAddr() = %q, want %q", addr, hc.Addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pushed response wasn't delivered")
	}
}

// serveHTTP1Attempts answers every HTTP/1 connection with response and
// closes it; an empty response keeps the connection silent. It counts accepts.
func serveHTTP1Attempts(t *testing.T, ln net.Listener, response string) *atomic.Int32 {
	t.Helper()
	var attempts atomic.Int32
	var mu sync.Mutex
	var silent []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range silent {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			go func() {
				var req fasthttp.Request
				if err := req.Read(bufio.NewReader(conn)); err != nil {
					_ = conn.Close()
					return
				}
				if response == "" {
					mu.Lock()
					silent = append(silent, conn)
					mu.Unlock()
					return
				}
				_, _ = io.WriteString(conn, response)
				_ = conn.Close()
			}()
		}
	}()
	return &attempts
}

// Post-header read failures are offered to HostClient's retry policy, as
// HTTP/1's are: a short body and a read timeout retry an idempotent request up
// to MaxIdemponentCallAttempts, a POST isn't retried by default, and
// ErrBodyTooLarge or a passed request deadline ends the attempts.
func TestClientPostHeaderFailuresFollowRetryPolicy(t *testing.T) {
	const maxAttempts = 3
	large := strings.Repeat("x", 1024)
	shortBody := func(peer *clientPeer, streamID uint32) {
		peer.writeHeaders(streamID, false, ":status", "200", "content-length", "10")
		peer.writeData(streamID, true, []byte("12345"))
	}
	largeBody := func(peer *clientPeer, streamID uint32) {
		peer.writeHeaders(streamID, false, ":status", "200", "content-length", "1024")
		peer.writeData(streamID, true, []byte(large))
	}
	cases := []struct {
		name     string
		method   string
		setup    func(hc *fasthttp.HostClient)
		timeout  time.Duration
		h1       string
		h2       func(peer *clientPeer, streamID uint32)
		attempts int32
		want     error
	}{
		{
			name:     "short body GET",
			method:   fasthttp.MethodGet,
			h1:       "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n12345",
			h2:       shortBody,
			attempts: maxAttempts,
			want:     io.ErrUnexpectedEOF,
		},
		{
			name:     "short body POST",
			method:   fasthttp.MethodPost,
			h1:       "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n12345",
			h2:       shortBody,
			attempts: 1,
			want:     io.ErrUnexpectedEOF,
		},
		{
			name:     "read timeout",
			method:   fasthttp.MethodGet,
			setup:    func(hc *fasthttp.HostClient) { hc.ReadTimeout = 50 * time.Millisecond },
			attempts: maxAttempts,
			want:     fasthttp.ErrTimeout,
		},
		{
			name:     "body too large",
			method:   fasthttp.MethodGet,
			setup:    func(hc *fasthttp.HostClient) { hc.MaxResponseBodySize = 16 },
			h1:       "HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\n" + large,
			h2:       largeBody,
			attempts: 1,
			want:     fasthttp.ErrBodyTooLarge,
		},
		{
			name:     "request deadline",
			method:   fasthttp.MethodGet,
			timeout:  60 * time.Millisecond,
			attempts: 1,
			want:     fasthttp.ErrTimeout,
		},
	}
	for _, tc := range cases {
		for _, h2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/h2=%v", tc.name, h2), func(t *testing.T) {
				ln := listenPeer(t)
				hc := &fasthttp.HostClient{Addr: ln.Addr().String(), MaxIdemponentCallAttempts: maxAttempts}
				if h2 {
					if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
						t.Fatalf("ConfigureHostClient() error: %v", err)
					}
				}
				t.Cleanup(hc.CloseIdleConnections)
				if tc.setup != nil {
					tc.setup(hc)
				}
				done := make(chan error, 1)
				go func() {
					req := fasthttp.AcquireRequest()
					resp := fasthttp.AcquireResponse()
					defer fasthttp.ReleaseRequest(req)
					defer fasthttp.ReleaseResponse(resp)
					req.Header.SetMethod(tc.method)
					req.SetRequestURI("http://" + hc.Addr + "/")
					if tc.timeout > 0 {
						done <- hc.DoTimeout(req, resp, tc.timeout)
						return
					}
					done <- hc.Do(req, resp)
				}()
				var attempts int32
				var err error
				if !h2 {
					counter := serveHTTP1Attempts(t, ln, tc.h1)
					err = waitResultOf(t, done)
					attempts = counter.Load()
				} else {
					peer := acceptPeer(t, ln)
				loop:
					for {
						select {
						case err = <-done:
							break loop
						case frame, ok := <-peer.frames:
							if !ok {
								t.Fatal("the client closed the connection")
							}
							if frame.typ == xhttp2.FrameHeaders {
								attempts++
								if tc.h2 != nil {
									tc.h2(peer, frame.streamID)
								}
							}
						case <-time.After(5 * time.Second):
							t.Fatal("request didn't finish")
						}
					}
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("Do() error = %v, want %v", err, tc.want)
				}
				if attempts != tc.attempts {
					t.Fatalf("attempts = %d, want %d", attempts, tc.attempts)
				}
			})
		}
	}
}

// A body cut short, by the peer's END_STREAM or by the connection, reports
// io.ErrUnexpectedEOF as HTTP/1 does, to Do and to a streamed body's reader.
func TestClientShortBodyReportsUnexpectedEOF(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, cut := range []string{"end-stream", "connection"} {
			t.Run(fmt.Sprintf("streaming=%v/%s", streaming, cut), func(t *testing.T) {
				ln := listenPeer(t)
				hc, _ := newPeerClient(t, ln, &ClientConfig{})
				hc.MaxIdemponentCallAttempts = 1
				hc.StreamResponseBody = streaming
				resp := fasthttp.AcquireResponse()
				defer fasthttp.ReleaseResponse(resp)
				done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
				peer := acceptPeer(t, ln)
				request := peer.nextHeaders()
				peer.writeHeaders(request.streamID, false, ":status", "200", "content-length", "5")
				if cut == "end-stream" {
					peer.writeData(request.streamID, true, []byte("abc"))
				} else {
					peer.writeData(request.streamID, false, []byte("abc"))
					peer.sync()
					_ = peer.conn.Close()
				}
				err := waitResultOf(t, done)
				if streaming {
					if err != nil {
						t.Fatalf("Do() error: %v", err)
					}
					var body []byte
					body, err = io.ReadAll(resp.BodyStream())
					if string(body) != "abc" {
						t.Fatalf("streamed body = %q before the cut, want %q", body, "abc")
					}
				}
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("short body error = %v, want io.ErrUnexpectedEOF", err)
				}
			})
		}
	}
}

// A response without a content-length reads as HTTP/1 reads a chunked one:
// ContentLength() -1 with a Transfer-Encoding entry, in every mode; a declared
// length is kept as is, with no Transfer-Encoding next to it, also on a
// Response reused after a 204 or 304.
func TestClientAbsentContentLengthReadsAsChunked(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	var peer *clientPeer
	exchange := func(t *testing.T, method string, answer func(streamID uint32)) {
		t.Helper()
		done := doAsync(hc, method, "/", nil, resp)
		if peer == nil {
			peer = acceptPeer(t, ln)
		}
		request := peer.nextHeaders()
		answer(request.streamID)
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
	}
	checkLength := func(t *testing.T, what string, length int) {
		t.Helper()
		if got := resp.Header.ContentLength(); got != length {
			t.Fatalf("%s: ContentLength() = %d, want %d", what, got, length)
		}
		encoding := string(resp.Header.Peek(fasthttp.HeaderTransferEncoding))
		if length == -1 && encoding != "chunked" || length >= 0 && encoding != "" {
			t.Fatalf("%s: Transfer-Encoding = %q with ContentLength() %d", what, encoding, length)
		}
	}
	check := func(t *testing.T, what string, length int, body string) {
		t.Helper()
		checkLength(t, what, length)
		if got := string(resp.Body()); got != body {
			t.Fatalf("%s: body = %q, want %q", what, got, body)
		}
	}
	noLength := func(body string) func(uint32) {
		return func(streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200")
			peer.writeData(streamID, true, []byte(body))
		}
	}
	declared := func(body string) func(uint32) {
		return func(streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200", "content-length", strconv.Itoa(len(body)))
			peer.writeData(streamID, true, []byte(body))
		}
	}
	bodyless := func(status string) func(uint32) {
		return func(streamID uint32) { peer.writeHeaders(streamID, true, ":status", status) }
	}

	exchange(t, fasthttp.MethodGet, noLength("hello"))
	check(t, "buffered without length", -1, "hello")
	exchange(t, fasthttp.MethodGet, bodyless("204"))
	if resp.StatusCode() != fasthttp.StatusNoContent || resp.Header.ContentLength() != -1 {
		t.Fatalf("204: status %d, ContentLength() = %d, want 204 and -1", resp.StatusCode(), resp.Header.ContentLength())
	}
	exchange(t, fasthttp.MethodGet, declared("abc"))
	check(t, "declared length after a 204", 3, "abc")
	exchange(t, fasthttp.MethodGet, bodyless("304"))
	if resp.StatusCode() != fasthttp.StatusNotModified || resp.Header.ContentLength() != -1 {
		t.Fatalf("304: status %d, ContentLength() = %d, want 304 and -1", resp.StatusCode(), resp.Header.ContentLength())
	}
	exchange(t, fasthttp.MethodGet, noLength("hi"))
	check(t, "no length after a 304", -1, "hi")
	exchange(t, fasthttp.MethodGet, bodyless("304"))
	exchange(t, fasthttp.MethodGet, declared("xy"))
	check(t, "declared length after a 304", 2, "xy")

	resp.SkipBody = true
	exchange(t, fasthttp.MethodGet, noLength("skipped"))
	check(t, "SkipBody", -1, "")
	resp.SkipBody = false
	exchange(t, fasthttp.MethodHead, bodyless("200"))
	check(t, "HEAD without length", -1, "")

	resp.StreamBody = true
	exchange(t, fasthttp.MethodGet, func(streamID uint32) {
		peer.writeHeaders(streamID, false, ":status", "200")
		peer.writeData(streamID, true, []byte("streamed"))
	})
	checkLength(t, "streamed without length", -1)
	if body, err := io.ReadAll(resp.BodyStream()); err != nil || string(body) != "streamed" {
		t.Fatalf("streamed body = %q, %v", body, err)
	}
	exchange(t, fasthttp.MethodGet, declared("sized"))
	checkLength(t, "streamed with length", 5)
	if body, err := io.ReadAll(resp.BodyStream()); err != nil || string(body) != "sized" {
		t.Fatalf("streamed sized body = %q, %v", body, err)
	}
}

// HTTP/1's chunked response is the model: -1 with the Transfer-Encoding entry,
// buffered and streamed.
func TestHTTP1ChunkedResponseContentLength(t *testing.T) {
	ln := listenPeer(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			var req fasthttp.Request
			if err := req.Read(reader); err != nil {
				return
			}
			if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"); err != nil {
				return
			}
		}
	}()
	hc := &fasthttp.HostClient{Addr: ln.Addr().String()}
	t.Cleanup(hc.CloseIdleConnections)
	for _, streaming := range []bool{false, true} {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		resp.StreamBody = streaming
		req.SetRequestURI("http://" + hc.Addr + "/")
		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		if got := resp.Header.ContentLength(); got != -1 {
			t.Fatalf("streaming=%v: ContentLength() = %d, want -1", streaming, got)
		}
		if got := string(resp.Header.Peek(fasthttp.HeaderTransferEncoding)); got != "chunked" {
			t.Fatalf("streaming=%v: Transfer-Encoding = %q, want chunked", streaming, got)
		}
		var body []byte
		if stream := resp.BodyStream(); stream != nil {
			body, _ = io.ReadAll(stream)
		} else {
			body = resp.Body()
		}
		if string(body) != "hello" {
			t.Fatalf("streaming=%v: body = %q", streaming, body)
		}
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
}

// A streamed response body is a ReadCloserWithError, as Response.BodyStream
// documents; closing it with an error resets its stream only, directly and
// through Response.CloseBodyStream, and the connection stays usable.
func TestClientStreamedBodyImplementsReadCloserWithError(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.StreamResponseBody = true
	var peer *clientPeer
	start := func(t *testing.T, resp *fasthttp.Response) uint32 {
		t.Helper()
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
		if peer == nil {
			peer = acceptPeer(t, ln)
		}
		request := peer.nextHeaders()
		peer.writeHeaders(request.streamID, false, ":status", "200")
		peer.writeData(request.streamID, false, []byte("part"))
		if err := waitResultOf(t, done); err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		return request.streamID
	}
	expectReset := func(t *testing.T, streamID uint32) {
		t.Helper()
		for _, frame := range peer.sync() {
			if frame.typ == xhttp2.FrameRSTStream && frame.streamID == streamID {
				if frame.errCode != xhttp2.ErrCodeCancel {
					t.Fatalf("RST_STREAM(%d) code = %v, want CANCEL", streamID, frame.errCode)
				}
				return
			}
		}
		t.Fatalf("no RST_STREAM for stream %d", streamID)
	}

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	streamID := start(t, resp)
	body, ok := resp.BodyStream().(fasthttp.ReadCloserWithError)
	if !ok {
		t.Fatalf("BodyStream() is a %T, want a ReadCloserWithError", resp.BodyStream())
	}
	if err := body.CloseWithError(errors.New("done early")); err != nil {
		t.Fatalf("CloseWithError() error: %v", err)
	}
	expectReset(t, streamID)

	streamID = start(t, resp)
	if err := resp.CloseBodyStream(); err != nil {
		t.Fatalf("CloseBodyStream() error: %v", err)
	}
	expectReset(t, streamID)

	done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
	request := peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "204")
	if err := waitResultOf(t, done); err != nil || resp.StatusCode() != fasthttp.StatusNoContent {
		t.Fatalf("Do() after the closes = %v, status %d", err, resp.StatusCode())
	}
}

// connIDServer answers with the connection's ID; /slow waits for release.
func connIDServer(t *testing.T, release <-chan struct{}) *testServer {
	t.Helper()
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) == "/slow" {
			<-release
		}
		ctx.SetBodyString(strconv.FormatUint(ctx.ConnID(), 10))
	}}
	return newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
}

// doPath runs one request and returns its body.
func doPath(t *testing.T, hc *fasthttp.HostClient, path string, connectionClose bool) (string, error) {
	t.Helper()
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("http://" + hc.Addr + path)
	if connectionClose {
		req.SetConnectionClose()
	}
	err := hc.Do(req, resp)
	return string(resp.Body()), err
}

// A request with SetConnectionClose retires its connection, as HTTP/1 closes
// it after the response: the next request lands on a new connection while a
// stream in flight on the old one completes; a request the connection never
// took, for lack of capacity, retires nothing.
func TestClientRequestConnectionCloseRetiresConnection(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			release := make(chan struct{})
			testServer := connIDServer(t, release)
			hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String(), MaxConns: 2}
			if h2 {
				if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, MaxConcurrentStreams: 1}); err != nil {
					t.Fatalf("ConfigureHostClient() error: %v", err)
				}
			}
			t.Cleanup(hc.CloseIdleConnections)
			slow := make(chan string, 1)
			go func() {
				body, err := doPath(t, hc, "/slow", false)
				if err != nil {
					t.Errorf("slow request error: %v", err)
				}
				slow <- body
			}()
			waitUntil(t, "the slow request holds a connection", func() bool { return hc.ConnsCount() == 1 })

			first, err := doPath(t, hc, "/", true)
			if err != nil {
				t.Fatalf("request with Connection: close: %v", err)
			}
			second, err := doPath(t, hc, "/", false)
			if err != nil {
				t.Fatalf("request after Connection: close: %v", err)
			}
			if first == second {
				t.Fatalf("the connection closed by request %s served request %s too", first, second)
			}
			close(release)
			select {
			case body := <-slow:
				if body == "" {
					t.Fatal("the slow request got no body")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the stream in flight on the retired connection didn't complete")
			}
		})
	}
}

// A refused reservation leaves the connection as it is: Connection: close on
// a request that fails with ErrNoFreeConns doesn't retire anything.
func TestClientRefusedConnectionCloseRequestRetiresNothing(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			release := make(chan struct{})
			testServer := connIDServer(t, release)
			hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String(), MaxConns: 1}
			if h2 {
				if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, MaxConcurrentStreams: 1}); err != nil {
					t.Fatalf("ConfigureHostClient() error: %v", err)
				}
			}
			t.Cleanup(hc.CloseIdleConnections)
			slow := make(chan string, 1)
			go func() {
				body, err := doPath(t, hc, "/slow", false)
				if err != nil {
					t.Errorf("slow request error: %v", err)
				}
				slow <- body
			}()
			waitUntil(t, "the slow request holds the connection", func() bool { return hc.ConnsCount() == 1 })
			if _, err := doPath(t, hc, "/", true); !errors.Is(err, fasthttp.ErrNoFreeConns) {
				t.Fatalf("request at MaxConns error = %v, want ErrNoFreeConns", err)
			}
			close(release)
			slowID := <-slow
			next, err := doPath(t, hc, "/", false)
			if err != nil {
				t.Fatalf("request after the refusal: %v", err)
			}
			if next != slowID {
				t.Fatalf("a refused Connection: close request retired the connection: %s then %s", slowID, next)
			}
		})
	}
}

// An expired connection closes once its last stream ends, without a further
// request or the idle timer: ConnsCount drops, and a request waiting at
// MaxConns gets the freed slot.
func TestClientExpiredConnectionClosesWhenLastStreamEnds(t *testing.T) {
	newExpiringClient := func(t *testing.T, release chan struct{}) (*fasthttp.HostClient, <-chan string) {
		t.Helper()
		testServer := connIDServer(t, release)
		hc := newPriorKnowledgeHostClient(t, testServer.listener.Addr().String())
		hc.MaxConns = 1
		hc.MaxConnDuration = 50 * time.Millisecond
		hc.MaxIdleConnDuration = time.Hour
		hc.MaxConnWaitTimeout = 5 * time.Second
		slow := make(chan string, 1)
		go func() {
			body, err := doPath(t, hc, "/slow", false)
			if err != nil {
				t.Errorf("slow request error: %v", err)
			}
			slow <- body
		}()
		waitUntil(t, "the slow request holds the connection", func() bool { return hc.ConnsCount() == 1 })
		time.Sleep(80 * time.Millisecond) // past MaxConnDuration
		return hc, slow
	}
	t.Run("closes without a further request", func(t *testing.T) {
		release := make(chan struct{})
		hc, slow := newExpiringClient(t, release)
		close(release)
		<-slow
		waitUntil(t, "the expired connection closes", func() bool { return hc.ConnsCount() == 0 })
	})
	t.Run("frees the slot for a waiter", func(t *testing.T) {
		release := make(chan struct{})
		hc, slow := newExpiringClient(t, release)
		waiting := make(chan string, 1)
		go func() {
			body, err := doPath(t, hc, "/", false)
			if err != nil {
				t.Errorf("waiting request error: %v", err)
			}
			waiting <- body
		}()
		waitUntil(t, "the request waits at MaxConns", func() bool { return hc.PendingRequests() == 2 })
		time.Sleep(50 * time.Millisecond)
		close(release)
		slowID := <-slow
		select {
		case served := <-waiting:
			if served == slowID {
				t.Fatalf("the waiting request was served by the expired connection %s", served)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the waiting request wasn't served by the freed slot")
		}
	})
}

// releaseServer answers with the connection's ID; /hold/<name> waits for its
// release channel.
func releaseServer(t *testing.T, config ServerConfig, releases map[string]chan struct{}) *testServer {
	t.Helper()
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if name, ok := strings.CutPrefix(string(ctx.Path()), "/hold/"); ok {
			<-releases[name]
		}
		ctx.SetBodyString(strconv.FormatUint(ctx.ConnID(), 10))
	}}
	return newTestServer(t, server, config)
}

// The pool follows ConnPoolStrategy as HTTP/1's idle list does: FIFO reuses
// the connection released earliest, LIFO the one released latest; when no
// connection is idle, the first created with capacity is used.
func TestClientPoolFollowsConnPoolStrategy(t *testing.T) {
	type result struct {
		body string
		err  error
	}
	start := func(hc *fasthttp.HostClient, path string) <-chan result {
		done := make(chan result, 1)
		go func() {
			body, err := doPath(t, hc, path, false)
			done <- result{body, err}
		}()
		return done
	}
	finish := func(t *testing.T, name string, release chan struct{}, done <-chan result) string {
		t.Helper()
		close(release)
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("%s: %v", name, r.err)
			}
			return r.body
		case <-time.After(5 * time.Second):
			t.Fatalf("%s didn't finish", name)
		}
		return ""
	}
	for _, tc := range []struct {
		name       string
		strategy   fasthttp.ConnPoolStrategyType
		firstDone  string // which held request finishes first
		wantSecond bool   // the next request lands on the connection released second
	}{
		{"FIFO after a then b", fasthttp.FIFO, "a", false},
		{"LIFO after a then b", fasthttp.LIFO, "a", true},
		{"FIFO after b then a", fasthttp.FIFO, "b", false},
		{"LIFO after b then a", fasthttp.LIFO, "b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
			testServer := releaseServer(t, ServerConfig{MaxConcurrentStreams: 1}, releases)
			hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String(), MaxConns: 2, ConnPoolStrategy: tc.strategy}
			if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, MaxConcurrentStreams: 1}); err != nil {
				t.Fatalf("ConfigureHostClient() error: %v", err)
			}
			t.Cleanup(hc.CloseIdleConnections)
			a := start(hc, "/hold/a")
			waitUntil(t, "the first connection is held", func() bool { return hc.ConnsCount() == 1 })
			b := start(hc, "/hold/b")
			waitUntil(t, "the second connection is held", func() bool { return hc.ConnsCount() == 2 })
			first, second := "a", "b"
			if tc.firstDone == "b" {
				first, second = "b", "a"
			}
			held := map[string]<-chan result{"a": a, "b": b}
			releasedFirst := finish(t, first, releases[first], held[first])
			releasedSecond := finish(t, second, releases[second], held[second])
			if releasedFirst == releasedSecond {
				t.Fatalf("both held requests ran on connection %s", releasedFirst)
			}
			next, err := doPath(t, hc, "/", false)
			if err != nil {
				t.Fatal(err)
			}
			want := releasedFirst
			if tc.wantSecond {
				want = releasedSecond
			}
			if next != want {
				t.Fatalf("next request used connection %s, want %s (released first %s, second %s)", next, want, releasedFirst, releasedSecond)
			}
		})
	}
	t.Run("active connections in creation order", func(t *testing.T) {
		releases := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
		testServer := releaseServer(t, ServerConfig{MaxConcurrentStreams: 2}, releases)
		hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String(), MaxConns: 2, ConnPoolStrategy: fasthttp.LIFO}
		if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge, MaxConcurrentStreams: 2}); err != nil {
			t.Fatalf("ConfigureHostClient() error: %v", err)
		}
		t.Cleanup(hc.CloseIdleConnections)
		a := start(hc, "/hold/a")
		b := start(hc, "/hold/b")
		waitUntil(t, "the first connection holds two streams", func() bool {
			return hc.ConnsCount() == 1 && hc.PendingRequests() == 2
		})
		time.Sleep(20 * time.Millisecond)
		c := start(hc, "/hold/c")
		waitUntil(t, "the second connection is held", func() bool { return hc.ConnsCount() == 2 })
		firstConn := finish(t, "a", releases["a"], a)
		// Both connections are active with capacity left: creation order wins.
		next, err := doPath(t, hc, "/", false)
		if err != nil {
			t.Fatal(err)
		}
		if next != firstConn {
			t.Fatalf("next request used connection %s, want the first created %s", next, firstConn)
		}
		if got := finish(t, "b", releases["b"], b); got != firstConn {
			t.Fatalf("request b ran on connection %s, want %s", got, firstConn)
		}
		if got := finish(t, "c", releases["c"], c); got == firstConn {
			t.Fatal("request c ran on the first connection")
		}
	})
}

// IdleConnsCount counts HTTP/2 connections as it counts HTTP/1 ones: open
// with no request in flight, until CloseIdleConnections.
func TestClientIdleConnsCountIncludesHTTP2(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			release := make(chan struct{})
			testServer := connIDServer(t, release)
			hc := &fasthttp.HostClient{Addr: testServer.listener.Addr().String()}
			if h2 {
				if err := ConfigureHostClient(hc, ClientConfig{Mode: PriorKnowledge}); err != nil {
					t.Fatalf("ConfigureHostClient() error: %v", err)
				}
			}
			t.Cleanup(hc.CloseIdleConnections)
			if n := hc.IdleConnsCount(); n != 0 {
				t.Fatalf("IdleConnsCount() before any request = %d", n)
			}
			slow := make(chan struct{})
			go func() {
				defer close(slow)
				if _, err := doPath(t, hc, "/slow", false); err != nil {
					t.Errorf("held request error: %v", err)
				}
			}()
			waitUntil(t, "the held request has a connection", func() bool { return hc.ConnsCount() == 1 })
			if n := hc.IdleConnsCount(); n != 0 {
				t.Fatalf("IdleConnsCount() with a request in flight = %d, want 0", n)
			}
			close(release)
			<-slow
			if conns, idle := hc.ConnsCount(), hc.IdleConnsCount(); conns != 1 || idle != 1 {
				t.Fatalf("after the request: ConnsCount() = %d, IdleConnsCount() = %d, want 1 and 1", conns, idle)
			}
			hc.CloseIdleConnections()
			waitUntil(t, "the idle connection closes", func() bool { return hc.ConnsCount() == 0 })
			if n := hc.IdleConnsCount(); n != 0 {
				t.Fatalf("IdleConnsCount() after CloseIdleConnections() = %d, want 0", n)
			}
		})
	}
}

// A promised request is held to the request rules (RFC 9113 §8.3.1, §8.4): an
// invalid te, :path or :scheme is reset with PROTOCOL_ERROR before the push
// handler sees it, the decoder stays in sync, and the promised stream is
// accounted: later frames on it are a closed stream's, not an idle one's.
func TestClientRejectsMalformedPushPromise(t *testing.T) {
	cases := []struct {
		name   string
		scheme string
		path   string
		te     string
	}{
		{name: "te", scheme: "http", path: "/pushed", te: "gzip"},
		{name: "path", scheme: "http", path: "not-an-absolute-path", te: "trailers"},
		{name: "scheme", scheme: "ftp", path: "/pushed", te: "trailers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ln := listenPeer(t)
			pushes := &testPushHandler{t: t, acceptedPath: make(chan string, 1), responseBody: make(chan string, 1)}
			hc, _ := newPeerClient(t, ln, &ClientConfig{PushHandler: pushes})
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(resp)
			done := doAsync(hc, fasthttp.MethodGet, "/", nil, resp)
			peer := acceptPeer(t, ln)
			parent := peer.nextHeaders()
			authority := parent.header(":authority")
			peer.wmu.Lock()
			err := peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
				StreamID: parent.streamID, PromiseID: 2, EndHeaders: true,
				BlockFragment: peer.encode(
					":method", fasthttp.MethodGet, ":scheme", tc.scheme, ":authority", authority,
					":path", tc.path, "te", tc.te, "x-indexed", "kept in the table",
				),
			})
			peer.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			reset := false
			for _, frame := range peer.sync() {
				if frame.typ == xhttp2.FrameRSTStream && frame.streamID == 2 {
					if frame.errCode != xhttp2.ErrCodeProtocol {
						t.Fatalf("RST_STREAM(2) code = %v, want PROTOCOL_ERROR", frame.errCode)
					}
					reset = true
				}
			}
			if !reset {
				t.Fatal("the malformed promise wasn't reset")
			}
			select {
			case path := <-pushes.acceptedPath:
				t.Fatalf("the malformed promise reached PushHandler.Accept for %q", path)
			default:
			}
			// Frames on the rejected promised stream are a closed stream's.
			peer.writeHeaders(2, true, ":status", "200")
			if frames := peer.sync(); len(frames) != 0 {
				t.Fatalf("a late response on the rejected promise drew %d frames", len(frames))
			}
			// The field the promise added to the dynamic table decodes.
			peer.writeHeaders(parent.streamID, true, ":status", "200", "x-indexed", "kept in the table")
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("Do() error: %v", err)
			}
			if got := string(resp.Header.Peek("x-indexed")); got != "kept in the table" {
				t.Fatalf("x-indexed = %q after the rejected promise", got)
			}

			done = doAsync(hc, fasthttp.MethodGet, "/next", nil, nil)
			parent = peer.nextHeaders()
			peer.wmu.Lock()
			err = peer.framer.WritePushPromise(xhttp2.PushPromiseParam{
				StreamID: parent.streamID, PromiseID: 4, EndHeaders: true,
				BlockFragment: peer.encode(
					":method", fasthttp.MethodGet, ":scheme", "http", ":authority", authority,
					":path", "/pushed", "te", "trailers",
				),
			})
			peer.wmu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			select {
			case path := <-pushes.acceptedPath:
				if path != "/pushed" {
					t.Fatalf("accepted path = %q", path)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a valid promise didn't reach PushHandler.Accept")
			}
			peer.writeHeaders(4, true, ":status", "204")
			peer.writeHeaders(parent.streamID, true, ":status", "200")
			if err := waitResultOf(t, done); err != nil {
				t.Fatalf("Do() error: %v", err)
			}
		})
	}
}

// parkUpload starts an upload the parking peer never reads and waits until its
// producer holds the write slot with the writer queue full.
func parkUpload(t *testing.T, hc *fasthttp.HostClient, c *clientConn) <-chan error {
	t.Helper()
	transport := c.conn.(*stallConn) //nolint:forcetypeassert
	transport.stall()
	upload := doAsync(hc, fasthttp.MethodPost, "/upload", make([]byte, 24<<20), nil)
	waitUntil(t, "the upload parks in the writer queue", func() bool {
		select {
		case c.writeSem <- struct{}{}:
			<-c.writeSem
			return false
		default:
			return transport.held.Load() != 0 && len(c.writer.queue) == cap(c.writer.queue)
		}
	})
	return upload
}

// warmParkingPeer serves one request so that the connection exists and the
// peer's window is wide enough for a long upload.
func warmParkingPeer(t *testing.T, ln net.Listener, hc *fasthttp.HostClient, transport *Transport) (*parkingPeer, *clientConn) {
	t.Helper()
	const window = 64 << 20
	hc.Dial = func(addr string) (net.Conn, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return newStallConn(conn), nil
	}
	warm := doAsync(hc, fasthttp.MethodGet, "/warm", nil, nil)
	peer := acceptParkingPeer(t, ln, xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: window})
	if err := peer.framer.WriteWindowUpdate(0, window); err != nil {
		t.Fatal(err)
	}
	headers := peer.nextHeaders(t)
	peer.writeHeaders(t, headers.StreamID, true, ":status", "204")
	if err := waitResultOf(t, warm); err != nil {
		t.Fatalf("warm-up Do() error: %v", err)
	}
	return peer, poolConns(transport, hc)[0]
}

// A peer that keeps sending frames that need answers while the transport is
// stalled can't grow the control queue without bound: past
// maxQueuedControlFrames the connection fails with ENHANCE_YOUR_CALM.
func TestClientBoundsQueuedControlFrames(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{WriteByteTimeout: 10 * time.Second})
	hc.RetryIfErr = func(*fasthttp.Request, int, error) (bool, bool) { return false, false }
	peer, c := warmParkingPeer(t, ln, hc, transport)
	upload := parkUpload(t, hc, c)
	defer func() { _ = peer.conn.Close() }()

	stop := make(chan struct{})
	sampled := make(chan int, 1)
	go func() {
		most := 0
		for {
			c.mu.Lock()
			queued := len(c.controls) + c.controlsInFlight
			c.mu.Unlock()
			most = max(most, queued)
			select {
			case <-stop:
				sampled <- most
				return
			default:
			}
		}
	}()
	for i := range 20000 {
		if err := peer.framer.WritePing(false, [8]byte{byte(i), byte(i >> 8)}); err != nil {
			break // the client closed the connection
		}
	}
	waitUntil(t, "the connection fails", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closed
	})
	close(stop)
	if most := <-sampled; most > maxQueuedControlFrames {
		t.Fatalf("%d control frames were queued, bound %d", most, maxQueuedControlFrames)
	}
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if !errors.Is(err, errTooManyControlFrames) {
		t.Fatalf("connection error = %v, want errTooManyControlFrames", err)
	}
	if err := waitResultOf(t, upload); !errors.Is(err, errTooManyControlFrames) {
		t.Fatalf("upload error = %v, want errTooManyControlFrames", err)
	}
}

// Resets queued for locally finalized streams count toward the bound too.
func TestClientQueuedResetsCountTowardControlBound(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{WriteByteTimeout: 10 * time.Second})
	hc.RetryIfErr = func(*fasthttp.Request, int, error) (bool, bool) { return false, false }
	peer, c := warmParkingPeer(t, ln, hc, transport)
	c.mu.Lock()
	c.maxControls = 8
	c.mu.Unlock()
	defer func() { _ = peer.conn.Close() }()
	// Requests whose HEADERS go out now and whose deadlines reset them later.
	gets := make([]<-chan error, 0, 10)
	for range 10 {
		done := make(chan error, 1)
		go func() {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI("http://" + hc.Addr + "/slow")
			done <- hc.DoTimeout(req, resp, 1500*time.Millisecond)
		}()
		gets = append(gets, done)
		peer.nextHeaders(t)
	}
	upload := parkUpload(t, hc, c)
	waitUntil(t, "the connection fails", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closed
	})
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if !errors.Is(err, errTooManyControlFrames) {
		t.Fatalf("connection error = %v, want errTooManyControlFrames", err)
	}
	for _, done := range gets {
		if err := waitResultOf(t, done); err == nil {
			t.Fatal("a request on the failed connection succeeded")
		}
	}
	if err := waitResultOf(t, upload); err == nil {
		t.Fatal("the upload on the failed connection succeeded")
	}
}

// Every PING on a healthy connection is answered.
func TestClientAnswersEveryPing(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	for i := range 100 {
		data := [8]byte{'p', byte(i)}
		peer.write(func(framer *xhttp2.Framer) error { return framer.WritePing(false, data) })
	}
	acks := 0
	peer.until(func(frame peerFrame) bool {
		if frame.typ == xhttp2.FramePing && frame.ack && frame.data[0] == 'p' {
			acks++
		}
		return acks == 100
	})
	peer.writeHeaders(request.streamID, true, ":status", "204")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
}

// doClose runs one request with Connection: close on hc and reports its error.
func doClose(hc *fasthttp.HostClient, path string) <-chan error {
	done := make(chan error, 1)
	go func() {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://" + hc.Addr + path)
		req.SetConnectionClose()
		done <- hc.Do(req, resp)
	}()
	return done
}

// drainUntilClosed reads the client's frames until the connection closes and
// reports whether a GOAWAY(NO_ERROR) came first.
func drainUntilClosed(t *testing.T, peer *clientPeer) bool {
	t.Helper()
	goAway := false
	timeout := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-peer.frames:
			if !ok {
				return goAway
			}
			if frame.typ == xhttp2.FrameGoAway {
				if frame.errCode != xhttp2.ErrCodeNo {
					t.Fatalf("GOAWAY code = %v, want NO_ERROR", frame.errCode)
				}
				goAway = true
			}
		case <-timeout:
			t.Fatal("the connection didn't close")
		}
	}
}

// Connection: close intent drains the connection: the other stream on it
// completes, then GOAWAY(NO_ERROR) precedes the close.
func TestClientConnectionCloseDrainsWithGoAway(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	slow := doAsync(hc, fasthttp.MethodGet, "/slow", nil, nil)
	peer := acceptPeer(t, ln)
	first := peer.nextHeaders()
	closing := doClose(hc, "/close")
	second := peer.nextHeaders()
	peer.writeHeaders(second.streamID, true, ":status", "200", "content-length", "0")
	if err := waitResultOf(t, closing); err != nil {
		t.Fatalf("request with Connection: close: %v", err)
	}
	if frames := peer.sync(); len(frames) != 0 {
		t.Fatalf("the connection sent %d frames while a stream was still open", len(frames))
	}
	peer.writeHeaders(first.streamID, true, ":status", "200", "content-length", "0")
	if err := waitResultOf(t, slow); err != nil {
		t.Fatalf("the stream in flight on the retiring connection: %v", err)
	}
	if !drainUntilClosed(t, peer) {
		t.Fatal("the connection closed without GOAWAY after Connection: close")
	}
	waitUntil(t, "the slot is freed", func() bool { return hc.ConnsCount() == 0 })
}

// A connection past MaxConnDuration drains the same way when its last stream
// ends; a connection retired by the idle timer keeps closing silently.
func TestClientMaxConnDurationRetirementSendsGoAway(t *testing.T) {
	ln := listenPeer(t)
	hc, _ := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConnDuration = 50 * time.Millisecond
	done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer := acceptPeer(t, ln)
	request := peer.nextHeaders()
	time.Sleep(80 * time.Millisecond)
	peer.writeHeaders(request.streamID, true, ":status", "204")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if !drainUntilClosed(t, peer) {
		t.Fatal("the over-age connection closed without GOAWAY")
	}

	ln = listenPeer(t)
	hc, _ = newPeerClient(t, ln, &ClientConfig{})
	hc.MaxIdleConnDuration = 50 * time.Millisecond
	done = doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
	peer = acceptPeer(t, ln)
	request = peer.nextHeaders()
	peer.writeHeaders(request.streamID, true, ":status", "204")
	if err := waitResultOf(t, done); err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	if drainUntilClosed(t, peer) {
		t.Fatal("the idle timer's close sent GOAWAY")
	}
}

// A retirement whose GOAWAY can't be written, the write slot being held by a
// stalled producer, still closes the connection within the drain bound.
func TestClientRetirementDrainIsBounded(t *testing.T) {
	ln := listenPeer(t)
	hc, transport := newPeerClient(t, ln, &ClientConfig{})
	hc.MaxConns = 1
	slow := doAsync(hc, fasthttp.MethodGet, "/slow", nil, nil)
	peer := acceptPeer(t, ln)
	first := peer.nextHeaders()
	closing := doClose(hc, "/close")
	second := peer.nextHeaders()
	peer.writeHeaders(second.streamID, true, ":status", "200", "content-length", "0")
	if err := waitResultOf(t, closing); err != nil {
		t.Fatalf("request with Connection: close: %v", err)
	}
	c := poolConns(transport, hc)[0]
	c.writeSem <- struct{}{} // the stalled producer
	defer func() { <-c.writeSem }()
	start := time.Now()
	peer.writeHeaders(first.streamID, true, ":status", "200", "content-length", "0")
	if err := waitResultOf(t, slow); err != nil {
		t.Fatalf("the stream in flight on the retiring connection: %v", err)
	}
	drainUntilClosed(t, peer)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the retirement took %v with the write slot held", elapsed)
	}
	waitUntil(t, "the slot is freed", func() bool { return hc.ConnsCount() == 0 })
}

// Each post-header failure kind of a buffered GET is offered to the retry
// policy: a read timeout after the response headers, a peer reset after them,
// and a connection that fails after them each count attempts as HTTP/1's read
// failures do.
func TestClientPostHeaderFailureKindsAreRetried(t *testing.T) {
	const maxAttempts = 3
	newClient := func(t *testing.T) (net.Listener, *fasthttp.HostClient) {
		t.Helper()
		ln := listenPeer(t)
		hc, _ := newPeerClient(t, ln, &ClientConfig{})
		hc.MaxIdemponentCallAttempts = maxAttempts
		return ln, hc
	}
	// answerUntilDone answers every HEADERS on one connection and reports the
	// attempts seen and Do's error.
	answerUntilDone := func(t *testing.T, peer *clientPeer, done <-chan error, answer func(streamID uint32)) (int, error) {
		t.Helper()
		attempts := 0
		for {
			select {
			case err := <-done:
				return attempts, err
			case frame, ok := <-peer.frames:
				if !ok {
					t.Fatal("the client closed the connection")
				}
				if frame.typ == xhttp2.FrameHeaders {
					attempts++
					answer(frame.streamID)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request didn't finish")
			}
		}
	}
	t.Run("read timeout after headers", func(t *testing.T) {
		ln, hc := newClient(t)
		hc.ReadTimeout = 80 * time.Millisecond
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		attempts, err := answerUntilDone(t, peer, done, func(streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200", "content-length", "10")
		})
		if !errors.Is(err, fasthttp.ErrTimeout) || attempts != maxAttempts {
			t.Fatalf("Do() = %v after %d attempts, want ErrTimeout after %d", err, attempts, maxAttempts)
		}
	})
	t.Run("peer reset after headers", func(t *testing.T) {
		ln, hc := newClient(t)
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		peer := acceptPeer(t, ln)
		attempts, err := answerUntilDone(t, peer, done, func(streamID uint32) {
			peer.writeHeaders(streamID, false, ":status", "200", "content-length", "10")
			peer.write(func(framer *xhttp2.Framer) error {
				return framer.WriteRSTStream(streamID, xhttp2.ErrCodeInternal)
			})
		})
		if _, ok := errors.AsType[*StreamError](err); !ok || attempts != maxAttempts {
			t.Fatalf("Do() = %v after %d attempts, want the peer's reset after %d", err, attempts, maxAttempts)
		}
	})
	t.Run("connection failure after headers", func(t *testing.T) {
		ln, hc := newClient(t)
		done := doAsync(hc, fasthttp.MethodGet, "/", nil, nil)
		attempts := 0
		for attempts < maxAttempts {
			peer := acceptPeer(t, ln)
			request := peer.nextHeaders()
			attempts++
			peer.writeHeaders(request.streamID, false, ":status", "200", "content-length", "10")
			peer.writeData(request.streamID, false, []byte("ab"))
			peer.sync()
			_ = peer.conn.Close()
		}
		if err := waitResultOf(t, done); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Do() = %v after %d attempts, want io.ErrUnexpectedEOF", err, attempts)
		}
		setAcceptDeadline(ln, time.Now().Add(100*time.Millisecond))
		if conn, err := ln.Accept(); err == nil {
			_ = conn.Close()
			t.Fatalf("a %dth attempt was made", maxAttempts+1)
		}
	})
}

// A batch the sender detached counts toward the control bound until it is
// written; the count is released when the write completes and when it fails.
func TestClientDetachedControlBatchIsCounted(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("writeFails=%v", writeFails), func(t *testing.T) {
			conn, open, _ := newGatedClientConn(t)
			queued := func() (controls, inFlight int) {
				conn.mu.Lock()
				defer conn.mu.Unlock()
				return len(conn.controls), conn.controlsInFlight
			}
			// The writer queue is full, so the sender parks in its flush with
			// the batch detached.
			conn.queueControl(controlFrame{kind: controlPingAck, ping: [8]byte{'a'}})
			waitUntil(t, "the sender detached the batch", func() bool {
				controls, inFlight := queued()
				return controls == 0 && inFlight == 1
			})
			conn.queueControl(controlFrame{kind: controlPingAck, ping: [8]byte{'b'}})
			if controls, inFlight := queued(); controls != 1 || inFlight != 1 {
				t.Fatalf("queued = %d, in flight = %d; want 1 and 1", controls, inFlight)
			}
			if writeFails {
				// Marked closed so that the sender's fail finds nothing to do;
				// the transport is gone when the gate opens.
				conn.mu.Lock()
				conn.closed = true
				conn.mu.Unlock()
				_ = conn.conn.Close()
			}
			open()
			waitUntil(t, "the batch count is released", func() bool {
				_, inFlight := queued()
				return inFlight == 0
			})
			if !writeFails {
				drainSender(t, conn)
				if controls, inFlight := queued(); controls != 0 || inFlight != 0 {
					t.Fatalf("after the drain: queued = %d, in flight = %d", controls, inFlight)
				}
			}
		})
	}
}
