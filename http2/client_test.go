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
	t.Cleanup(func() { c.writer.abort(net.ErrClosed) })
	return c
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
	transport.mu.Lock()
	_, retained := transport.pools[hostClient]
	transport.mu.Unlock()
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

func TestResponseContentLengthSemantics(t *testing.T) {
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	// The request method is snapshotted when the stream is reserved, because the
	// read loop must never dereference a request the caller may already own
	// again. Mirror that here instead of mutating the request between calls.
	stream := &clientStream{req: request, isHead: true}

	request.Header.SetMethod(fasthttp.MethodHead)
	if err := validateResponseContentLength(stream, fasthttp.StatusOK, 123); err != nil {
		t.Fatalf("HEAD content-length rejected: %v", err)
	}
	stream = &clientStream{req: request}
	request.Header.SetMethod(fasthttp.MethodGet)
	if err := validateResponseContentLength(stream, fasthttp.StatusNotModified, 123); err != nil {
		t.Fatalf("304 content-length rejected: %v", err)
	}
	if err := validateResponseContentLength(stream, fasthttp.StatusNoContent, 0); err == nil {
		t.Fatal("204 content-length was accepted")
	}
	stream = &clientStream{req: request, isConnect: true}
	request.Header.SetMethod(fasthttp.MethodConnect)
	if err := validateResponseContentLength(stream, fasthttp.StatusOK, 0); err == nil {
		t.Fatal("successful CONNECT content-length was accepted")
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
		encoder:        hpack.NewEncoder(io.Discard),
		streams:        make(map[uint32]*clientStream),
	}
	installTestWriter(t, conn)
	if err := conn.processSettings(frame.(*xhttp2.SettingsFrame)); err != nil { //nolint:forcetypeassert
		t.Fatalf("processSettings() error: %v", err)
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
		framer:                  xhttp2.NewFramer(writer, nil),
		bufferedWriter:          writer,
		streams:                 make(map[uint32]*clientStream),
		nextStreamID:            3,
		receiveConnectionWindow: windowSize,
	}
	installTestWriter(t, conn)
	frame := makeClientTestDataFrame(t, 1, bytes.Repeat([]byte{'x'}, flowLength))

	if err := conn.processResponseData(frame); err != nil {
		t.Fatalf("processResponseData() error: %v", err)
	}
	if conn.receiveConnectionWindow != windowSize {
		t.Fatalf("connection receive window = %d, want %d", conn.receiveConnectionWindow, windowSize)
	}
	requireConnectionWindowUpdate(t, wire.Bytes(), flowLength)
}

func TestClosedResponseBodyCallbacksCannotAffectLaterStream(t *testing.T) {
	conn := installTestWriter(t, &clientConn{streams: make(map[uint32]*clientStream)})
	later := &clientStream{id: 3, conn: conn}
	conn.streams[later.id] = later
	body := conn.newClientResponseBody(1)
	if err := body.Close(); err != nil {
		t.Fatalf("old response body Close() error: %v", err)
	}
	if conn.streams[later.id] != later || later.err != nil {
		t.Fatalf("old response body affected later stream: stream=%p err=%v", conn.streams[later.id], later.err)
	}
}

func TestClientRefusedStreamReturnsTypedError(t *testing.T) {
	pool := &clientPool{
		hc:        &fasthttp.HostClient{},
		available: make(chan struct{}, 1),
	}
	conn := &clientConn{
		writeSem:         make(chan struct{}, 1),
		pool:             pool,
		hc:               pool.hc,
		streams:          make(map[uint32]*clientStream),
		nextStreamID:     5,
		activeStreams:    2,
		receivedSettings: true,
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
		hc:        &fasthttp.HostClient{},
		available: make(chan struct{}, 1),
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
			hc:        &fasthttp.HostClient{},
			available: make(chan struct{}, 1),
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
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
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
			InsecureSkipVerify: true, //nolint:gosec // Test-only certificate.
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
		hc:                       &fasthttp.HostClient{},
		config:                   clientConfig{maxConcurrentStreams: 1},
		peerMaxConcurrentStreams: 100,
		nextStreamID:             1,
		streams:                  map[uint32]*clientStream{},
		activeStreams:            1,
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
		available: make(chan struct{}, 1),
		notify:    make(chan struct{}),
	}
	pool.remove(nil)
	select {
	case <-pool.available:
	default:
		t.Fatal("remove() didn't signal waiters blocked on MaxConns")
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

	transport.mu.Lock()
	pool := transport.pools[hc]
	transport.mu.Unlock()
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
	pool := &clientPool{available: make(chan struct{}, 1)}
	conn := &clientConn{
		pool:                    pool,
		hc:                      &fasthttp.HostClient{},
		writeSem:                make(chan struct{}, 1),
		bufferedWriter:          bufferedWriter,
		streams:                 make(map[uint32]*clientStream),
		nextStreamID:            5,
		activeStreams:           2,
		peerMaxFrameSize:        defaultMaxFrameSize,
		peerMaxHeaderListSize:   1<<32 - 1,
		peerInitialStreamWindow: 65535,
		peerConnectionWindow:    65535,
		receiveConnectionWindow: 65535,
	}
	installTestWriter(t, conn)
	conn.framer = xhttp2.NewFramer(bufferedWriter, nil)
	conn.encoder = hpack.NewEncoder(&conn.headerBuffer)

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
		canceledDone <- conn.writeRequestHeaders(canceled, true, canceledRequest, time.Now().Add(100*time.Millisecond))
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
	if err := conn.writeRequestHeaders(survivor, true, survivorRequest, time.Now().Add(time.Second)); err != nil {
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
	pool := &clientPool{available: make(chan struct{}, 1)}
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
