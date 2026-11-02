package http2

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand"
	"mime/multipart"
	"net"
	stdhttp "net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"github.com/valyala/fasthttp/fasthttputil"
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

// ownedServerHandler serves connections through newServerConn and hands the
// test each serverConn, with its command queue in place.
type ownedServerHandler struct {
	config serverConfig
	conns  chan *serverConn
}

func (h *ownedServerHandler) ServeConn(ctx *fasthttp.ProtocolServerContext, c net.Conn) error {
	conn := newServerConn(ctx, c, &h.config)
	conn.initQueues()
	h.conns <- conn
	return conn.serve(nil)
}

// newOwnedTestServer is newTestServer with the test holding each connection's
// serverConn, for owner-synchronized inspection through inspectOwner.
func newOwnedTestServer(t testing.TB, server *fasthttp.Server, config ServerConfig) (*testServer, <-chan *serverConn) {
	t.Helper()
	normalized, err := normalizeServerConfig(server, &config)
	if err != nil {
		t.Fatalf("normalizeServerConfig() error: %v", err)
	}
	handler := &ownedServerHandler{config: normalized, conns: make(chan *serverConn, 8)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _ = server.ServeProtocolConn(c, handler) }()
		}
	}()
	dials := new(atomic.Int64)
	transport := &xhttp2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			dials.Add(1)
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		_ = listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.ShutdownWithContext(ctx); err != nil {
			t.Errorf("ShutdownWithContext() error: %v", err)
		}
	})
	return &testServer{
		server:    server,
		listener:  listener,
		scheme:    "http",
		transport: transport,
		client:    &stdhttp.Client{Transport: transport},
		dials:     dials,
	}, handler.conns
}

// testProtocolContext is a Server's ProtocolServerContext, for tests that
// call the request parser directly.
func testProtocolContext(tb testing.TB) *fasthttp.ProtocolServerContext {
	tb.Helper()
	contexts := make(chan *fasthttp.ProtocolServerContext, 1)
	handler := protocolContextCapture{contexts: contexts}
	pipe := fasthttputil.NewPipeConns()
	defer pipe.Conn1().Close()
	if err := (&fasthttp.Server{}).ServeProtocolConn(pipe.Conn2(), handler); err != nil {
		tb.Fatalf("ServeProtocolConn() error: %v", err)
	}
	return <-contexts
}

type protocolContextCapture struct {
	contexts chan *fasthttp.ProtocolServerContext
}

func (h protocolContextCapture) ServeConn(ctx *fasthttp.ProtocolServerContext, _ net.Conn) error {
	h.contexts <- ctx
	return nil
}

// inspectOwner runs fn on conn's event loop and waits for it.
func inspectOwner(t testing.TB, conn *serverConn, fn func(*serverConn)) {
	t.Helper()
	result := make(chan error, 1)
	select {
	case conn.commands <- serverCommand{kind: serverCommandInspect, inspect: fn, result: result}:
	case <-conn.ctx.Done():
		t.Fatal("connection ended before the inspection")
	}
	select {
	case <-result:
	case <-conn.ctx.Done():
		t.Fatal("connection ended during the inspection")
	}
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

// Each value goes out in the section it is in, as over HTTP/1.
func TestServerKeepsResponseValuesInTheirSection(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			switch string(ctx.Path()) {
			case "/mixed":
				err := ctx.Response.Read(bufio.NewReader(strings.NewReader("HTTP/1.1 200 OK\r\nX-Mixed: upfront\r\n" +
					"Trailer: X-Mixed\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\nX-Mixed: late\r\n\r\n")))
				if err != nil {
					t.Error(err)
				}
			case "/server":
				// The default line stays upfront.
				_ = ctx.Response.Header.AddTrailer(fasthttp.HeaderServer)
				ctx.Response.Header.Set(fasthttp.HeaderServer, "late")
			case "/deleted":
				ctx.Response.Header.Del(fasthttp.HeaderServer)
				_ = ctx.Response.Header.AddTrailer(fasthttp.HeaderServer)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	for _, tc := range []struct {
		path, header, trailer string
	}{
		{"/mixed", `X-Mixed=["upfront"] Server=["fasthttp"]`, `X-Mixed=["late"] Server=[]`},
		{"/server", `X-Mixed=[] Server=["fasthttp"]`, `X-Mixed=[] Server=["late"]`},
		{"/deleted", `X-Mixed=[] Server=["fasthttp"]`, `X-Mixed=[] Server=[]`},
	} {
		resp, err := testServer.client.Get(testServer.URL(tc.path))
		if err != nil {
			t.Fatalf("%s: Get() error: %v", tc.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		sections := func(h stdhttp.Header) string {
			return fmt.Sprintf("X-Mixed=%q Server=%q", h.Values("X-Mixed"), h.Values("Server"))
		}
		if got := sections(resp.Header); got != tc.header {
			t.Errorf("%s: header block %s, want %s", tc.path, got, tc.header)
		}
		if got := sections(resp.Trailer); got != tc.trailer {
			t.Errorf("%s: trailers %s, want %s", tc.path, got, tc.trailer)
		}
	}
}

// The Trailer field announces the trailers in the header block, as HTTP/1
// writes it, so a client knows the keys before the body; the values still
// travel in the trailing section alone, which a HEAD doesn't get.
func TestResponseDeclaresTrailersInTheHeaderBlock(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if err := ctx.Response.Header.AddTrailer("X-Trail"); err != nil {
			t.Errorf("AddTrailer() error: %v", err)
		}
		ctx.Response.Header.Set("X-Trail", "tv")
		ctx.SetBodyString("body")
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	// blocks is the header block fields of each HEADERS on streamID, in order.
	blocks := func(peer *rawPeer, streamID uint32) []map[string]string {
		var blocks []map[string]string
		peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok && headers.StreamID == streamID {
				fields := map[string]string{}
				for _, field := range headers.Fields {
					fields[field.Name] = field.Value
				}
				blocks = append(blocks, fields)
			}
			return isStreamEnd(streamID)(frame)
		})
		return blocks
	}
	for _, tc := range []struct {
		method string
		blocks int
	}{{stdhttp.MethodGet, 2}, {stdhttp.MethodHead, 1}} {
		t.Run(tc.method, func(t *testing.T) {
			wire := http1Exchange(t, addr, tc.method+" / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
			if !strings.Contains(wire, "\r\nTrailer: X-Trail\r\n") {
				t.Fatalf("HTTP/1 control header block lacks the declaration: %q", wire)
			}
			peer := dialRawPeer(t, addr)
			peer.writeHeaders(1, true, [2]string{":method", tc.method}, [2]string{":scheme", "http"},
				[2]string{":authority", "example.com"}, [2]string{":path", "/"})
			got := blocks(peer, 1)
			if len(got) != tc.blocks {
				t.Fatalf("header blocks = %v, want %d", got, tc.blocks)
			}
			if got[0]["trailer"] != "X-Trail" {
				t.Fatalf("header block = %v, want trailer: X-Trail", got[0])
			}
			if _, leaked := got[0]["x-trail"]; leaked {
				t.Fatalf("header block = %v carries the trailer value", got[0])
			}
			if tc.blocks == 2 && got[1]["x-trail"] != "tv" {
				t.Fatalf("trailing section = %v, want x-trail: tv", got[1])
			}
		})
	}
	// net/http's HTTP/2 transport exposes the declared keys before the body.
	resp, err := testServer.client.Get(testServer.URL("/"))
	if err != nil {
		t.Fatalf("Get() error: %v", err)
	}
	defer resp.Body.Close()
	if keys := slices.Sorted(maps.Keys(resp.Trailer)); !slices.Equal(keys, []string{"X-Trail"}) {
		t.Fatalf("trailer keys before the body = %q, want [X-Trail]", keys)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if values := resp.Trailer.Values("X-Trail"); !slices.Equal(values, []string{"tv"}) {
		t.Fatalf("trailer values after the body = %q, want [tv]", values)
	}
}

// writeOversizedRequest sends a request whose header block exceeds an 8 KiB
// MaxHeaderListSize over HEADERS and CONTINUATION frames.
func writeOversizedRequest(t *testing.T, peer *rawPeer, streamID uint32, endStream bool) {
	t.Helper()
	fields := requestFields("/big")
	for i := range 40 {
		fields = append(fields, [2]string{fmt.Sprintf("x-h%d", i), strings.Repeat("v", 400)})
	}
	block := peer.headerBlock(fields...)
	const fragment = 8000
	first := min(len(block), fragment)
	if err := peer.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:first],
		EndStream:     endStream,
		EndHeaders:    first == len(block),
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeContinuationFrames(peer.framer, streamID, block[first:], fragment); err != nil {
		t.Fatal(err)
	}
}

// A request header block over MaxHeaderListSize is answered as HTTP/1
// answers a header it won't read in full: through Server.ErrorHandler with
// *fasthttp.ErrSmallBuffer, 431 by default, on a connection that stays
// usable; nothing counts as a rapid reset. An upload left open is reset once
// the response is out, and the stream bound admits the request first.
func TestOversizedRequestHeaderAnswers431(t *testing.T) {
	type seen struct {
		smallBuffer  bool
		method, path string
	}
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-handler", true: "custom-handler"}[custom], func(t *testing.T) {
			errs := make(chan seen, 2)
			server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
			if custom {
				server.ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
					_, smallBuffer := errors.AsType[*fasthttp.ErrSmallBuffer](err)
					errs <- seen{smallBuffer: smallBuffer, method: string(ctx.Method()), path: string(ctx.Path())}
					ctx.Error("custom", fasthttp.StatusTeapot)
				}
			}
			want := "431"
			if custom {
				want = "418"
			}
			// The owned server speaks HTTP/2 alone; HTTP/1 is a server of its own.
			controlServer := &fasthttp.Server{Handler: server.Handler, ErrorHandler: server.ErrorHandler}
			control := newTestServer(t, controlServer, ServerConfig{})
			bigHeader := "GET /big HTTP/1.1\r\nHost: example.com\r\nX-Big: " + strings.Repeat("v", 16<<10) + "\r\nConnection: close\r\n\r\n"
			if wire := http1Exchange(t, control.listener.Addr().String(), bigHeader); !strings.HasPrefix(wire, "HTTP/1.1 "+want+" ") {
				t.Fatalf("HTTP/1 control = %q, want %s", wire, want)
			}
			testServer, conns := newOwnedTestServer(t, server, ServerConfig{MaxHeaderListSize: 8 << 10})
			addr := testServer.listener.Addr().String()
			if custom {
				// HTTP/1 keeps no request target from a header it couldn't read.
				if got := <-errs; !got.smallBuffer {
					t.Fatalf("HTTP/1 ErrorHandler saw %+v, want *ErrSmallBuffer", got)
				}
			}
			peer := dialRawPeer(t, addr)
			conn := <-conns
			writeOversizedRequest(t, peer, 1, true)
			status, reset := streamOutcome(peer, 1)
			if status != want || reset != nil {
				t.Fatalf("status = %q, reset = %v, want %s", status, reset, want)
			}
			if custom {
				if got := <-errs; !got.smallBuffer || got.method != fasthttp.MethodGet || got.path != "/big" {
					t.Fatalf("ErrorHandler saw %+v, want *ErrSmallBuffer for GET /big", got)
				}
			}
			peer.request(3, "/ok", true)
			if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("next request statuses = %q, want [200]", statuses)
			}
			var rapidResets uint32
			inspectOwner(t, conn, func(c *serverConn) { rapidResets = c.rapidResetCount })
			if rapidResets != 0 {
				t.Fatalf("rapid resets = %d, want none", rapidResets)
			}
		})
	}
	t.Run("upload-open", func(t *testing.T) {
		testServer := newTestServer(t, &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}, ServerConfig{MaxHeaderListSize: 8 << 10})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		writeOversizedRequest(t, peer, 1, false)
		if status, reset := streamOutcome(peer, 1); status != "431" || reset != nil {
			t.Fatalf("status = %q, reset = %v, want 431", status, reset)
		}
		if counts, kind := peer.waitForAny(2*time.Second, "rst_NO_ERROR", "closed"); kind != "rst_NO_ERROR" {
			t.Fatalf("open upload wasn't reset after the response: %v", counts)
		}
	})
	t.Run("stream-bound", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		var errorHandlerCalls atomic.Int32
		server := &fasthttp.Server{
			Handler:      func(*fasthttp.RequestCtx) { <-release },
			ErrorHandler: func(ctx *fasthttp.RequestCtx, _ error) { errorHandlerCalls.Add(1); ctx.Error("x", 431) },
		}
		testServer := newTestServer(t, server, ServerConfig{MaxHeaderListSize: 8 << 10, MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/held", true)
		writeOversizedRequest(t, peer, 3, true)
		if counts, kind := peer.waitForAny(2*time.Second, "rst_REFUSED_STREAM", "headers", "closed"); kind != "rst_REFUSED_STREAM" {
			t.Fatalf("oversized request over the stream bound: %v", counts)
		}
		if errorHandlerCalls.Load() != 0 {
			t.Fatal("ErrorHandler ran for a request refused by the stream bound")
		}
	})
}

// Closing a streamed request body returns its retained flow credit: the
// connection's always, the stream's while the upload is still open. The rest
// of the upload is dropped, a second upload on the connection proceeds and
// END_STREAM afterwards finishes the stream normally; a body the peer reset
// first has nothing left to return.
func TestClosingRequestBodyReturnsFlowCredit(t *testing.T) {
	const window = 65535
	for _, tc := range []struct {
		name       string
		endStream  bool
		resetFirst bool
	}{
		{name: "upload-open"},
		{name: "upload-complete", endStream: true},
		{name: "reset-first", resetFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeNow, closed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(release)
			server := &fasthttp.Server{StreamRequestBody: true, Handler: func(ctx *fasthttp.RequestCtx) {
				if string(ctx.Path()) == "/second" {
					body, err := io.ReadAll(ctx.RequestBodyStream())
					if err != nil {
						t.Errorf("reading the second upload: %v", err)
					}
					ctx.SetBodyString(strconv.Itoa(len(body)))
					return
				}
				<-closeNow
				_ = ctx.Request.CloseBodyStream()
				close(closed)
				<-release
				ctx.SetBodyString("done")
			}}
			testServer, conns := newOwnedTestServer(t, server, ServerConfig{
				MaxUploadBufferPerStream:     window,
				MaxUploadBufferPerConnection: window,
			})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			conn := <-conns
			upload := func(streamID uint32, endStream bool) {
				t.Helper()
				peer.writeHeaders(streamID, false, [2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
					[2]string{":authority", "example.com"}, [2]string{":path", map[uint32]string{1: "/first", 3: "/second"}[streamID]})
				for left := window; left > 0; {
					n := min(left, defaultMaxFrameSize)
					left -= n
					if err := peer.framer.WriteData(streamID, endStream && left == 0, make([]byte, n)); err != nil {
						t.Fatal(err)
					}
				}
			}
			// barrier reads the server's frames up to a PING ACK and sums the
			// WINDOW_UPDATE increments by stream.
			barrier := func(data byte) map[uint32]uint32 {
				t.Helper()
				if err := peer.framer.WritePing(false, [8]byte{data}); err != nil {
					t.Fatal(err)
				}
				updates := map[uint32]uint32{}
				peer.readHeaders(1, func(frame xhttp2.Frame) bool {
					if update, ok := frame.(*xhttp2.WindowUpdateFrame); ok {
						updates[update.StreamID] += update.Increment
					}
					return isPingAck(frame)
				})
				return updates
			}
			upload(1, tc.endStream)
			if updates := barrier(1); len(updates) != 0 {
				t.Fatalf("WINDOW_UPDATEs before the body was read or closed: %v", updates)
			}
			if tc.resetFirst {
				if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
					t.Fatal(err)
				}
				barrier(2)
			}
			close(closeNow)
			<-closed
			var unconsumed, streamWindow, connWindow int64
			var isReset bool
			inspectOwner(t, conn, func(c *serverConn) {
				connWindow = c.recv.window
				stream := c.streams[1]
				unconsumed, streamWindow, isReset = stream.unconsumedFlow, stream.recv.window, stream.isReset
			})
			updates := barrier(3)
			switch {
			case tc.resetFirst:
				// The reset returned the credit; the close has nothing left.
				if !isReset || unconsumed != 0 || len(updates) != 0 {
					t.Fatalf("after the close of a reset stream: reset %v, retained flow %d, WINDOW_UPDATEs %v", isReset, unconsumed, updates)
				}
			case unconsumed != 0 || connWindow != window:
				t.Fatalf("after the close: retained flow %d, connection window %d; want 0 and %d", unconsumed, connWindow, window)
			case updates[0] != window:
				t.Fatalf("connection WINDOW_UPDATEs = %v, want %d on stream 0", updates, window)
			case tc.endStream && updates[1] != 0:
				t.Fatalf("WINDOW_UPDATE %d on a stream the peer has closed", updates[1])
			case !tc.endStream && (updates[1] != window || streamWindow != window):
				t.Fatalf("stream WINDOW_UPDATEs = %v, stream window %d; want %d restored", updates, streamWindow, window)
			}
			// The connection's credit is back: a second upload goes through.
			upload(3, true)
			if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("second upload statuses = %q, want [200]", statuses)
			}
			if !tc.endStream && !tc.resetFirst {
				// The dropped upload's end still finishes the stream.
				if err := peer.framer.WriteData(1, true, nil); err != nil {
					t.Fatal(err)
				}
			}
			release <- struct{}{}
			if tc.resetFirst {
				barrier(4)
				return
			}
			if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("first stream statuses = %q, want [200]", statuses)
			}
		})
	}
}

// Priority signals are read as RFC 8941 Dictionaries on the wire too: a
// request's repeated priority lines combine into one, PRIORITY_UPDATE keeps
// the valid members of a value with an invalid one, and such a value for an
// idle stream counts toward the prioritized-stream bound.
func TestPrioritySignalsKeepValidMembers(t *testing.T) {
	hold := func() (*fasthttp.Server, chan struct{}) {
		release := make(chan struct{})
		return &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) { <-release }}, release
	}
	streamPriority := func(t *testing.T, conn *serverConn, streamID uint32) priority {
		t.Helper()
		var got priority
		inspectOwner(t, conn, func(c *serverConn) { got = c.streams[streamID].priority })
		return got
	}
	t.Run("request-lines-combined", func(t *testing.T) {
		server, release := hold()
		defer close(release)
		testServer, conns := newOwnedTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		conn := <-conns
		peer.writeHeaders(1, true, append(requestFields("/"), [2]string{"priority", "u=1, i=42"}, [2]string{"priority", "i, x=\"u=7,\""})...)
		if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(1, isPingAck)
		if got := streamPriority(t, conn, 1); got != (priority{urgency: 1, incremental: true}) {
			t.Fatalf("priority = %+v, want urgency 1, incremental", got)
		}
	})
	t.Run("priority-update", func(t *testing.T) {
		server, release := hold()
		defer close(release)
		testServer, conns := newOwnedTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		conn := <-conns
		peer.writeHeaders(1, true, append(requestFields("/"), [2]string{"priority", "u=2"})...)
		if err := peer.framer.WritePriorityUpdate(1, "u=8, i"); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WritePing(false, [8]byte{2}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(1, isPingAck)
		if got := streamPriority(t, conn, 1); got != (priority{urgency: 3, incremental: true}) {
			t.Fatalf("priority = %+v, want the default urgency, incremental", got)
		}
	})
	t.Run("idle-limit", func(t *testing.T) {
		server, release := hold()
		defer close(release)
		testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/", true)
		if err := peer.framer.WritePing(false, [8]byte{3}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(1, isPingAck)
		// A valid signal for an idle stream, over the one open stream.
		if err := peer.framer.WritePriorityUpdate(3, "u=8, i"); err != nil {
			t.Fatal(err)
		}
		if counts, kind := peer.waitForAny(2*time.Second, "goaway_PROTOCOL_ERROR", "closed"); kind != "goaway_PROTOCOL_ERROR" {
			t.Fatalf("prioritized idle stream over the bound: %v", counts)
		}
	})
}

// A rejected hijack that meant to send no response, the adaptor's answer to
// http.ErrAbortHandler, ends the stream with RST_STREAM and nothing sent, as
// HTTP/1 closes the connection; the connection stays usable and the stream
// is released. A hijack expecting a response keeps the 501.
func TestHijackWithoutResponseAbortsTheStream(t *testing.T) {
	abort := fasthttpadaptor.NewFastHTTPHandler(stdhttp.HandlerFunc(func(stdhttp.ResponseWriter, *stdhttp.Request) {
		panic(stdhttp.ErrAbortHandler)
	}))
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/abort":
			abort(ctx)
		case "/hijack-no-response":
			ctx.HijackSetNoResponse(true)
			ctx.Hijack(func(net.Conn) { t.Error("HTTP/2 request invoked a connection hijack handler") })
			ctx.SetBodyString("handler body")
		case "/hijack":
			ctx.Hijack(func(net.Conn) { t.Error("HTTP/2 request invoked a connection hijack handler") })
			ctx.SetBodyString("handler body")
		default:
			ctx.SetBodyString("ok")
		}
	}}
	testServer, conns := newOwnedTestServer(t, server, ServerConfig{})
	control := newTestServer(t, &fasthttp.Server{Handler: server.Handler}, ServerConfig{})
	http1 := http1Client(t)
	if resp, err := http1.Get(control.URL("/abort")); err == nil {
		resp.Body.Close()
		t.Fatalf("HTTP/1 control answered an aborted request with %d", resp.StatusCode)
	}
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	conn := <-conns
	streamID := uint32(1)
	for _, tc := range []struct {
		path, status string
		reset        bool
	}{
		{"/abort", "", true},
		{"/hijack-no-response", "", true},
		{"/hijack", "501", false},
		{"/", "200", false},
	} {
		peer.request(streamID, tc.path, true)
		status, reset := streamOutcome(peer, streamID)
		switch {
		case tc.reset && (status != "" || reset == nil || *reset != xhttp2.ErrCodeInternal):
			t.Fatalf("%s: status = %q, reset = %v, want RST_STREAM(INTERNAL_ERROR) alone", tc.path, status, reset)
		case !tc.reset && (status != tc.status || reset != nil):
			t.Fatalf("%s: status = %q, reset = %v, want %s", tc.path, status, reset, tc.status)
		}
		var open int
		inspectOwner(t, conn, func(c *serverConn) { open = len(c.streams) })
		if open != 0 {
			t.Fatalf("%s: %d streams still registered", tc.path, open)
		}
		streamID += 2
	}
}

// A malformed priority signal is ignored wherever it arrives: a request
// header keeps the default, also when an empty line makes the combined list
// malformed, a PRIORITY_UPDATE on an open stream leaves its priority alone,
// and one for an idle stream doesn't count toward the prioritized-stream
// bound.
func TestMalformedPrioritySignalsAreIgnored(t *testing.T) {
	hold := func() (*fasthttp.Server, chan struct{}) {
		release := make(chan struct{})
		return &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) { <-release }}, release
	}
	for _, tc := range []struct {
		name   string
		fields [][2]string
		update string
		want   priority
	}{
		{name: "undecodable-binary-header", fields: [][2]string{{"priority", "x=:A:,u=0"}}, want: priority{urgency: 3}},
		{name: "undecodable-binary-update", fields: [][2]string{{"priority", "u=2"}}, update: "x=:A:,u=0", want: priority{urgency: 2}},
		{name: "empty-first-line", fields: [][2]string{{"priority", ""}, {"priority", "u=0"}}, want: priority{urgency: 3}},
		{name: "empty-second-line", fields: [][2]string{{"priority", "u=0"}, {"priority", ""}}, want: priority{urgency: 3}},
		{name: "empty-line", fields: [][2]string{{"priority", ""}}, want: priority{urgency: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, release := hold()
			defer close(release)
			testServer, conns := newOwnedTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			conn := <-conns
			peer.writeHeaders(1, true, append(requestFields("/"), tc.fields...)...)
			if tc.update != "" {
				if err := peer.framer.WritePriorityUpdate(1, tc.update); err != nil {
					t.Fatal(err)
				}
			}
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			peer.readHeaders(1, isPingAck)
			var got priority
			inspectOwner(t, conn, func(c *serverConn) { got = c.streams[1].priority })
			if got != tc.want {
				t.Fatalf("priority = %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("idle-slot", func(t *testing.T) {
		server, release := hold()
		defer close(release)
		testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/", true)
		if err := peer.framer.WritePriorityUpdate(3, "x=:A:,u=0"); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WritePing(false, [8]byte{2}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
				t.Fatalf("GOAWAY(%v) for a malformed priority signal", goAway.ErrCode)
			}
			return isPingAck(frame)
		})
	})
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

// A peer that connects and closes before sending the preface ends ServeConn
// with nil, as HTTP/1's ServeConn reports it; a partial preface is an error.
func TestServeConnPeerClosesBeforePreface(t *testing.T) {
	for _, tc := range []struct {
		name    string
		send    string
		wantErr bool
	}{
		{name: "nothing"},
		{name: "partial-preface", send: clientPreface[:10], wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				done <- ServeConn(&fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}, conn, ServerConfig{})
			}()
			peerConn, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if tc.send != "" {
				if _, err := io.WriteString(peerConn, tc.send); err != nil {
					t.Fatal(err)
				}
			}
			_ = peerConn.Close()
			select {
			case err := <-done:
				if (err != nil) != tc.wantErr {
					t.Fatalf("ServeConn() error = %v, want error %v", err, tc.wantErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ServeConn() didn't return after the peer closed")
			}
		})
	}
}

// A peer that closes cleanly with a request in flight ends ServeConn with
// io.ErrUnexpectedEOF, as HTTP/1's partial request then FIN does, which the
// worker pool doesn't log; an idle peer's close stays nil and a protocol
// error stays its own error.
func TestPeerCloseWithRequestInFlightIsUnexpectedEOF(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	// serve runs one accepted connection through ServeConn.
	serve := func(t *testing.T, server *fasthttp.Server, http2 bool) (net.Conn, <-chan error) {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			if http2 {
				done <- ServeConn(server, conn, ServerConfig{})
			} else {
				done <- server.ServeConn(conn)
			}
		}()
		peerConn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peerConn.Close() })
		return peerConn, done
	}
	wait := func(t *testing.T, done <-chan error) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("ServeConn() didn't return after the peer closed")
			return nil
		}
	}
	// settle sends a PING and reads the server's frames up to its ACK, so
	// the close finds nothing unread and every frame before it processed.
	settle := func(t *testing.T, peerConn net.Conn, framer *xhttp2.Framer) {
		t.Helper()
		if err := framer.WritePing(false, [8]byte{7}); err != nil {
			t.Fatal(err)
		}
		_ = peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				t.Fatalf("reading the server's frames: %v", err)
			}
			if isPingAck(frame) {
				return
			}
		}
	}
	for _, tc := range []struct {
		name    string
		drive   func(t *testing.T, peerConn net.Conn, framer *xhttp2.Framer)
		wantErr error
	}{
		{
			name: "upload-pending",
			drive: func(t *testing.T, peerConn net.Conn, framer *xhttp2.Framer) {
				if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
					StreamID:      1,
					BlockFragment: peerRequestBlock(t, fasthttp.MethodPost),
					EndHeaders:    true,
				}); err != nil {
					t.Fatal(err)
				}
				settle(t, peerConn, framer)
			},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name: "handler-running",
			drive: func(t *testing.T, peerConn net.Conn, framer *xhttp2.Framer) {
				if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
					StreamID:      1,
					BlockFragment: peerRequestBlock(t, fasthttp.MethodGet),
					EndStream:     true,
					EndHeaders:    true,
				}); err != nil {
					t.Fatal(err)
				}
				settle(t, peerConn, framer)
			},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "idle",
			drive:   settle,
			wantErr: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
				<-release
				ctx.SetBodyString("ok")
			}}
			peerConn, done := serve(t, server, true)
			if _, err := io.WriteString(peerConn, clientPreface); err != nil {
				t.Fatal(err)
			}
			framer := xhttp2.NewFramer(peerConn, peerConn)
			if err := framer.WriteSettings(); err != nil {
				t.Fatal(err)
			}
			tc.drive(t, peerConn, framer)
			_ = peerConn.Close()
			if tc.name == "handler-running" {
				// ServeConn waits for the handler.
				select {
				case err := <-done:
					t.Fatalf("ServeConn() returned %v while the handler ran", err)
				case <-time.After(50 * time.Millisecond):
				}
				release <- struct{}{}
			}
			if err := wait(t, done); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ServeConn() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("protocol-error", func(t *testing.T) {
		peerConn, done := serve(t, &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}, true)
		if _, err := io.WriteString(peerConn, clientPreface); err != nil {
			t.Fatal(err)
		}
		framer := xhttp2.NewFramer(peerConn, peerConn)
		framer.AllowIllegalWrites = true
		if err := framer.WriteSettings(); err != nil {
			t.Fatal(err)
		}
		// DATA on stream 0 is a connection error, not a transport one.
		if err := framer.WriteData(0, false, []byte("x")); err != nil {
			t.Fatal(err)
		}
		err := wait(t, done)
		if err == nil || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("ServeConn() error = %v, want the protocol error", err)
		}
	})
	t.Run("http1-control", func(t *testing.T) {
		peerConn, done := serve(t, &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}, false)
		if _, err := io.WriteString(peerConn, "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 10\r\n\r\nabc"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		_ = peerConn.Close()
		if err := wait(t, done); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("HTTP/1 ServeConn() error = %v, want %v", err, io.ErrUnexpectedEOF)
		}
	})
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

// A client that closes its connection in the middle of a streamed upload
// must not look like a complete upload: the reader fails with
// io.ErrUnexpectedEOF instead of ending with io.EOF.
func TestTruncatedStreamedUploadIsNotCleanEOF(t *testing.T) {
	for _, withLength := range []bool{false, true} {
		name := "no-content-length"
		if withLength {
			name = "content-length"
		}
		t.Run(name, func(t *testing.T) {
			type readResult struct {
				n   int
				err error
			}
			results := make(chan readResult, 1)
			server := &fasthttp.Server{
				StreamRequestBody: true,
				Handler: func(ctx *fasthttp.RequestCtx) {
					body, err := io.ReadAll(ctx.RequestBodyStream())
					results <- readResult{len(body), err}
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			fields := [][2]string{
				{":method", fasthttp.MethodPost},
				{":scheme", "http"},
				{":authority", "example.com"},
				{":path", "/upload"},
			}
			if withLength {
				fields = append(fields, [2]string{"content-length", "100"})
			}
			peer.writeHeaders(1, false, fields...)
			if err := peer.framer.WriteData(1, false, make([]byte, 10)); err != nil {
				t.Fatal(err)
			}
			// The DATA reaches the handler before the connection drops.
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			peer.readHeaders(1, isPingAck)
			// A FIN without a reset, as from a client that read everything the
			// server sent before closing.
			tcpConn, ok := peer.conn.(*net.TCPConn)
			if !ok {
				t.Fatalf("peer connection is %T, want *net.TCPConn", peer.conn)
			}
			if err := tcpConn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-results:
				if got.n != 10 || !errors.Is(got.err, io.ErrUnexpectedEOF) {
					t.Fatalf("ReadAll() = %d bytes, %v; want 10 bytes and %v", got.n, got.err, io.ErrUnexpectedEOF)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("handler didn't finish")
			}
		})
	}
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

// A handler that stops reading its streamed upload and answers early gets
// its response out: later DATA is credited and dropped, as HTTP/1 drains
// the rest, and the complete response is followed by RST_STREAM(NO_ERROR),
// which counts toward no limit.
func TestClosedRequestBodyKeepsEarlyResponse(t *testing.T) {
	closed := make(chan struct{}, 2)
	release := make(chan struct{})
	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			if _, err := ctx.RequestBodyStream().Read(make([]byte, 4)); err != nil {
				t.Errorf("first Read() error: %v", err)
			}
			if err := ctx.Request.CloseBodyStream(); err != nil {
				t.Errorf("CloseBodyStream() error: %v", err)
			}
			closed <- struct{}{}
			<-release
			ctx.Error("too large", fasthttp.StatusRequestEntityTooLarge)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{MaxRapidResetsPerSecond: 1})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	for _, streamID := range []uint32{1, 3} {
		peer.writeHeaders(streamID, false,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
		)
		if err := peer.framer.WriteData(streamID, false, []byte("head")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("handler didn't run")
		}
		// The rest of the upload, in flight when the handler gave up.
		if err := peer.framer.WriteData(streamID, false, []byte("more")); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WritePing(false, [8]byte{byte(streamID)}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
				t.Fatalf("RST_STREAM(%v) on stream %d before its response", rst.ErrCode, rst.StreamID)
			}
			return isPingAck(frame)
		})
		release <- struct{}{}
		var reset *xhttp2.RSTStreamFrame
		statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			rst, ok := frame.(*xhttp2.RSTStreamFrame)
			if ok && rst.StreamID == streamID {
				reset = rst
			}
			return ok
		})
		if !slices.Equal(statuses, []string{"413"}) || reset.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("stream %d: statuses = %q, reset = %v", streamID, statuses, reset.ErrCode)
		}
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

// A stream the peer cancels is closed on the wire, so a client that opens a
// new one in its place stays within SETTINGS_MAX_CONCURRENT_STREAMS while the
// old handler still runs.
func TestCancelledStreamsDontCountTowardConcurrentStreams(t *testing.T) {
	const limit = 2
	started := make(chan struct{}, 2*limit)
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		started <- struct{}{}
		<-release
		ctx.SetBodyString("ok")
	}}
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: limit})
	defer releaseOnce.Do(func() { close(release) })
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/", true)
	peer.request(3, "/", true)
	for range limit {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("handlers didn't start")
		}
	}
	for _, streamID := range []uint32{1, 3} {
		if err := peer.framer.WriteRSTStream(streamID, xhttp2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}
	peer.request(5, "/", true)
	peer.request(7, "/", true)
	if err := peer.framer.WritePing(false, [8]byte{5}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(7, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			t.Fatalf("RST_STREAM(%v) on stream %d while %d streams are open", rst.ErrCode, rst.StreamID, limit)
		}
		return isPingAck(frame)
	})
	releaseOnce.Do(func() { close(release) })
	answered := map[uint32]bool{}
	peer.readHeaders(7, func(frame xhttp2.Frame) bool {
		if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok {
			answered[headers.StreamID] = true
		}
		return answered[5] && answered[7]
	})
}

// Handlers beyond the stream limit wait in order instead of being refused,
// and a queued stream the peer resets never runs.
func TestHandlersBeyondConcurrentStreamsQueue(t *testing.T) {
	var running, maxRunning atomic.Int32
	paths := make(chan string, 4)
	release := make(chan struct{})
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		paths <- string(ctx.Path())
		<-release
		running.Add(-1)
		ctx.SetBodyString("ok")
	}}
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
	defer close(release)
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	nextPath := func() string {
		t.Helper()
		select {
		case path := <-paths:
			return path
		case <-time.After(2 * time.Second):
			t.Fatal("no handler started")
			return ""
		}
	}
	peer.request(1, "/first", true)
	if path := nextPath(); path != "/first" {
		t.Fatalf("first handler path = %q", path)
	}
	if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	// Stream 3 takes the freed slot on the wire; its handler waits for the
	// first one.
	peer.request(3, "/queued", true)
	if err := peer.framer.WritePing(false, [8]byte{3}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(3, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			t.Fatalf("RST_STREAM(%v) on stream %d", rst.ErrCode, rst.StreamID)
		}
		return isPingAck(frame)
	})
	if len(paths) != 0 {
		t.Fatalf("handler for %q started beyond the limit", <-paths)
	}
	if err := peer.framer.WriteRSTStream(3, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	peer.request(5, "/third", true)
	if err := peer.framer.WritePing(false, [8]byte{5}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(5, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			t.Fatalf("RST_STREAM(%v) on stream %d", rst.ErrCode, rst.StreamID)
		}
		return isPingAck(frame)
	})
	release <- struct{}{}
	if path := nextPath(); path != "/third" {
		t.Fatalf("handler after the first returned = %q, want /third", path)
	}
	release <- struct{}{}
	if statuses := peer.readHeaders(5, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		return ok && headers.StreamID == 5
	}); !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("stream 5 statuses = %q", statuses)
	}
	if got := maxRunning.Load(); got != 1 {
		t.Fatalf("handlers ran %d at a time, want 1", got)
	}
}

// A refusal for capacity is not the peer's doing and must not count toward
// MaxRapidResetsPerSecond.
func TestCapacityRefusalsDontCountAsRapidResets(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		started <- struct{}{}
		<-release
	}}
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1, MaxRapidResetsPerSecond: 2})
	defer close(release)
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/", true)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler didn't start")
	}
	for streamID := uint32(3); streamID <= 7; streamID += 2 {
		peer.request(streamID, "/", true)
	}
	if err := peer.framer.WritePing(false, [8]byte{7}); err != nil {
		t.Fatal(err)
	}
	refused := 0
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.RSTStreamFrame:
			if frame.ErrCode != xhttp2.ErrCodeRefusedStream {
				t.Fatalf("RST_STREAM(%v) on stream %d", frame.ErrCode, frame.StreamID)
			}
			refused++
		case *xhttp2.GoAwayFrame:
			t.Fatalf("GOAWAY(%v) after %d capacity refusals", frame.ErrCode, refused)
		}
		return isPingAck(frame)
	})
	if refused != 3 {
		t.Fatalf("refused %d streams, want 3", refused)
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
		ctx:      ctx,
		cancel:   cancel,
		framer:   xhttp2.NewFramer(nil, wire),
		events:   make(chan incomingFrame, 1),
		readDone: make(chan struct{}),
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

func TestMalformedHeadersPaddingIsProtocolError(t *testing.T) {
	wire := bufio.NewReader(bytes.NewReader([]byte{
		0, 0, 2, byte(xhttp2.FrameHeaders),
		byte(xhttp2.FlagHeadersPadded | xhttp2.FlagHeadersEndHeaders),
		0, 0, 0, 1,
		0xff, 0,
	}))
	framer := xhttp2.NewFramer(nil, wire)
	reader := newFrameReader(framer, wire)
	_, err := reader.readFrame()
	if _, ok := errors.AsType[xhttp2.ConnectionError](err); !ok || errorCode(err) != xhttp2.ErrCodeProtocol {
		t.Fatalf("malformed padding error = %v (%v), want connection PROTOCOL_ERROR", err, errorCode(err))
	}
}

func TestMalformedHeaderStreamIDCannotBeReused(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 10},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		connFlowState:       connFlowState{receivedSettings: true},
	}
	event := incomingFrame{
		kind:     incomingFrameStreamError,
		streamID: 1,
		errCode:  xhttp2.ErrCodeProtocol,
		err:      errInvalidRequestHeaders,
	}
	if err := conn.processFrame(&event); err != nil {
		t.Fatalf("first malformed stream error: %v", err)
	}
	if conn.lastClientStreamID != 1 {
		t.Fatalf("last client stream ID = %d, want 1", conn.lastClientStreamID)
	}
	if !conn.closedClientStreams[1] {
		t.Fatal("malformed stream wasn't remembered as reset")
	}
	// RFC 9113 §5.1: frames on a stream this server reset are ignored.
	written := wire.Len()
	if err := conn.processFrame(&event); err != nil {
		t.Fatalf("repeated malformed stream error = %v, want it ignored", err)
	}
	if err := conn.processFrame(&incomingFrame{kind: incomingFrameHeaders, streamID: 1, endStream: true}); err != nil {
		t.Fatalf("HEADERS on the reset stream = %v, want it ignored", err)
	}
	if len(conn.streams) != 0 || wire.Len() != written {
		t.Fatalf("reset stream reopened (%d streams) or answered (%d bytes)", len(conn.streams), wire.Len()-written)
	}
}

func TestRejectedHeaderStreamIDCannotBeReused(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config:              serverConfig{maxConcurrentStreams: 1},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		connFlowState:       connFlowState{receivedSettings: true},
	}
	event := incomingFrame{kind: incomingFrameHeaders, streamID: 1, hasPriority: true, dependency: 1}
	if err := conn.processFrame(&event); err != nil {
		t.Fatalf("first rejected HEADERS error: %v", err)
	}
	if conn.lastClientStreamID != 1 {
		t.Fatalf("last client stream ID = %d, want 1", conn.lastClientStreamID)
	}
	written := wire.Len()
	if err := conn.processFrame(&event); err != nil {
		t.Fatalf("HEADERS on the rejected stream = %v, want it ignored", err)
	}
	if len(conn.streams) != 0 || wire.Len() != written {
		t.Fatalf("rejected stream reopened (%d streams) or answered (%d bytes)", len(conn.streams), wire.Len()-written)
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

// parsePriority reads an RFC 8941 Dictionary: a member of the wrong type or
// out of range is ignored on its own (RFC 9218 §4), a repeated key takes its
// last value, and a syntax error fails the whole field.
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
		{"", priority{urgency: 3}, false},
		{"  u=5", priority{urgency: 5}, false},
		{"u=8", priority{urgency: 3}, false},
		{"u=8, i", priority{urgency: 3, incremental: true}, false},
		{"u=-1", priority{urgency: 3}, false},
		{"u=0, i=42", priority{urgency: 0}, false},
		{"u=1.5, i", priority{urgency: 3, incremental: true}, false},
		{"u=abc, i=?1", priority{urgency: 3, incremental: true}, false},
		{"u=(1 2), i", priority{urgency: 3, incremental: true}, false},
		{"u=(), i", priority{urgency: 3, incremental: true}, false},
		{`x="hello,u=7",u=0`, priority{urgency: 0}, false},
		{`x="a\"b\\c";p=1, u=2`, priority{urgency: 2}, false},
		{"x=:YWJj:, u=4", priority{urgency: 4}, false},
		{"x=:QUI=:, u=4", priority{urgency: 4}, false},
		{"x=:QUI:, u=4", priority{urgency: 4}, false},
		{"x=:QUJD:, u=4", priority{urgency: 4}, false},
		{"x=::, u=4", priority{urgency: 4}, false},
		{"x=?0;q;r=token, u=6", priority{urgency: 6}, false},
		{"u=1, u=2", priority{urgency: 2}, false},
		{"u=0, u=8", priority{urgency: 3}, false},
		{"i, i=?0", priority{urgency: 3}, false},
		{"i=?2", priority{}, true},
		{"u=", priority{}, true},
		{",u=1", priority{}, true},
		{"u=1,", priority{}, true},
		{"u=3,,i", priority{}, true},
		{"u = 1", priority{}, true},
		{"U=1", priority{}, true},
		{"u=1;", priority{}, true},
		{"u=1 i", priority{}, true},
		{"u=1234567890123456", priority{}, true},
		{"u=1.", priority{}, true},
		{"u=1.2345", priority{}, true},
		{`x="unterminated`, priority{}, true},
		{`x="bad\n"`, priority{}, true},
		{"x=:a=b:", priority{}, true},
		{"x=:abc", priority{}, true},
		{"x=:A:,u=0", priority{}, true},
		{"x=:YWJj=:", priority{}, true},
		{"x=:ABCDE:", priority{}, true},
		{"x=:ABCD=:", priority{}, true},
		{"x=:A===:", priority{}, true},
		{"x=:QUJD===:", priority{}, true},
		{"u=(1", priority{}, true},
		{"u=(1,2)", priority{}, true},
		{"x=?", priority{}, true},
		{"x=@", priority{}, true},
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
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = parsePriority(`u=1; extension=token, x="a,b", i`)
	}); allocs != 0 {
		t.Fatalf("parsePriority() allocates %v times per call", allocs)
	}
}

func TestRequestBodyOwnsAndBoundsChunks(t *testing.T) {
	const chunks = maxRequestBodyChunks + 2
	released := 0
	conn := &serverConn{}
	conn.initQueues()
	body := newRequestBody(conn, 1)
	for i := range chunks {
		data := []byte{byte(i)}
		if err := body.writeOwned(data, func([]byte) { released++ }); err != nil {
			t.Fatalf("writeOwned() error: %v", err)
		}
	}
	if len(body.chunks) > maxRequestBodyChunks {
		t.Fatalf("queued chunks = %d, want at most %d", len(body.chunks), maxRequestBodyChunks)
	}
	body.complete()
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
	if consumed, _ := body.takeCredit(); consumed != chunks {
		t.Fatalf("consumed bytes = %d, want %d", consumed, chunks)
	}
	if released != chunks {
		t.Fatalf("released chunks = %d, want %d", released, chunks)
	}
}

func TestRequestBodyCloseReleasesOwnedChunks(t *testing.T) {
	released := 0
	body := newRequestBody(nil, 0)
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
		body:           newRequestBody(nil, 0),
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
	conn.initQueues()
	stream.body = newRequestBody(conn, stream.id)
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
	if err := conn.applyQueuedCredit(); err != nil {
		t.Fatalf("applyQueuedCredit() error: %v", err)
	}
	if conn.recv.pending != 4 || stream.unconsumedFlow != 0 {
		t.Fatalf("after body read: pending connection update = %d, unconsumed flow = %d; want 4 and 0", conn.recv.pending, stream.unconsumedFlow)
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

// Consumed credit is held back only while the peer has more window left than
// is held; otherwise the update is due however small it is.
// RFC 9113 §5.1: a body read after the peer's END_STREAM returns connection
// credit only; a WINDOW_UPDATE on the half-closed stream is an error there.
func TestBodyReadAfterEndStreamCreditsConnectionOnly(t *testing.T) {
	const window = 65535
	release := make(chan struct{})
	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			<-release
			if _, err := io.Copy(io.Discard, ctx.RequestBodyStream()); err != nil {
				t.Errorf("reading the body: %v", err)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{
		MaxUploadBufferPerConnection: window,
		MaxUploadBufferPerStream:     window,
	})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
		[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
	)
	body := make([]byte, 40000)
	for sent := 0; sent < len(body); sent += defaultMaxFrameSize {
		chunk := body[sent:min(sent+defaultMaxFrameSize, len(body))]
		if err := peer.framer.WriteData(1, sent+len(chunk) == len(body), chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isPingAck)
	close(release)
	connectionCredit := uint32(0)
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		if update, ok := frame.(*xhttp2.WindowUpdateFrame); ok {
			if update.StreamID == 1 {
				t.Fatalf("WINDOW_UPDATE(+%d) on half-closed stream 1", update.Increment)
			}
			connectionCredit += update.Increment
		}
		return isStreamEnd(1)(frame)
	})
	if connectionCredit == 0 {
		t.Fatal("consumed body returned no connection credit")
	}
}

func TestRecvWindowZeroDebitPassesNegativeWindow(t *testing.T) {
	window := recvWindow{window: -1}
	if !window.debit(0) {
		t.Fatal("empty frame rejected by a negative window")
	}
	if window.debit(1) {
		t.Fatal("a padded empty frame passed a negative window")
	}
}

// A SETTINGS reduction may leave a stream's receive window negative while
// the peer's last DATA is in flight; its empty END_STREAM frame spends no
// credit and is accepted, while padding still counts.
func TestEmptyEndStreamPassesWindowReducedBySettings(t *testing.T) {
	release := make(chan struct{})
	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			<-release
			// Stream 3's handler reads its reset as an error.
			_, _ = io.Copy(io.Discard, ctx.RequestBodyStream())
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{MaxUploadBufferPerStream: 1})
	defer close(release)
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// Sent under the RFC default window, before the server's SETTINGS is
	// acknowledged.
	post := func(streamID uint32) {
		peer.writeHeaders(streamID, false,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/"},
		)
	}
	post(1)
	if err := peer.framer.WriteData(1, false, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		settings, ok := frame.(*xhttp2.SettingsFrame)
		return ok && !settings.IsAck()
	})
	// The acknowledgement takes stream 1's window from 65533 to -1.
	if err := peer.framer.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteData(1, true, nil); err != nil {
		t.Fatal(err)
	}
	post(3)
	if err := peer.framer.WriteDataPadded(3, true, nil, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{2}); err != nil {
		t.Fatal(err)
	}
	resets := map[uint32]xhttp2.ErrCode{}
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			resets[rst.StreamID] = rst.ErrCode
		}
		return isPingAck(frame)
	})
	if _, reset := resets[1]; reset {
		t.Fatalf("empty END_STREAM under the reduced window was reset: %v", resets)
	}
	if resets[3] != xhttp2.ErrCodeFlowControl {
		t.Fatalf("padded empty DATA beyond the window: resets = %v, want FLOW_CONTROL_ERROR on stream 3", resets)
	}
}

func TestRecvWindowReturnsCreditThePeerRunsShortOf(t *testing.T) {
	const windowSize = 1000
	w := recvWindow{window: windowSize}
	w.debit(100)
	if increment := w.consume(100, windowSize); increment != 0 {
		t.Fatalf("increment with 900 left = %d, want 0", increment)
	}
	w.debit(800)
	increment := w.consume(200, windowSize)
	if increment != 300 {
		t.Fatalf("increment with 100 left = %d, want 300", increment)
	}
	w.grant(increment)
	if w.window != 400 || w.pending != 0 {
		t.Fatalf("window = %d pending = %d, want 400 and 0", w.window, w.pending)
	}
}

// Request bodies nobody reads can pin most of the connection window. A stream
// whose data was consumed must still return its connection credit, or the
// whole connection stalls.
func TestConsumedBodyReturnsConnectionCreditUnderPinnedWindow(t *testing.T) {
	const (
		connectionWindow = 192 << 10
		streamWindow     = 64 << 10
	)
	release := make(chan struct{})
	defer close(release)
	read := make(chan int64, 1)
	testServer := newTestServer(t, &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/hold" {
				<-release
				return
			}
			n, _ := io.Copy(io.Discard, ctx.RequestBodyStream())
			read <- n
		},
	}, ServerConfig{MaxUploadBufferPerConnection: connectionWindow, MaxUploadBufferPerStream: streamWindow})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	// The connection-level update that opens the configured window.
	peer.readHeaders(0, func(frame xhttp2.Frame) bool {
		update, ok := frame.(*xhttp2.WindowUpdateFrame)
		return ok && update.StreamID == 0
	})
	chunk := make([]byte, defaultMaxFrameSize)
	send := func(id uint32, path string, end bool) {
		peer.writeHeaders(id, false,
			[2]string{":method", "POST"}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", path},
		)
		for sent := 0; sent < streamWindow; sent += len(chunk) {
			if err := peer.framer.WriteData(id, end && sent+len(chunk) == streamWindow, chunk); err != nil {
				t.Fatal(err)
			}
		}
	}
	send(1, "/hold", false)
	send(3, "/hold", false)
	// The last third of the connection window, consumed in full.
	send(5, "/read", true)
	if n := <-read; n != streamWindow {
		t.Fatalf("handler read %d bytes, want %d", n, streamWindow)
	}
	peer.readHeaders(0, func(frame xhttp2.Frame) bool {
		update, ok := frame.(*xhttp2.WindowUpdateFrame)
		return ok && update.StreamID == 0
	})
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
	stream.body = newRequestBody(nil, 0)
	stream.body.discardWithError(errStreamClosed)
	conn.streams[stream.id] = stream
	event := incomingFrame{
		streamID:   stream.id,
		flowLength: 4,
		data:       []byte("body"),
	}

	// The dropped frame is credited exactly once on both windows.
	for range 2 {
		if err := conn.processData(&event); err != nil {
			t.Fatalf("processData() error: %v", err)
		}
	}
	if conn.recv.window+conn.recv.pending != windowSize {
		t.Fatalf("connection credit = %d+%d, want %d", conn.recv.window, conn.recv.pending, windowSize)
	}
	if stream.recv.window+stream.recv.pending != windowSize {
		t.Fatalf("stream credit = %d+%d, want %d", stream.recv.window, stream.recv.pending, windowSize)
	}
	if stream.isReset || !stream.discardRequestBody || stream.unconsumedFlow != 0 {
		t.Fatalf("DATA on a closed body left reset=%v discard=%v unconsumed=%d; want the upload dropped",
			stream.isReset, stream.discardRequestBody, stream.unconsumedFlow)
	}
}

// priorityTestConn is a bare connection for the RFC 9218 §7.1 accounting.
func priorityTestConn(maxConcurrentStreams uint32) *serverConn {
	return &serverConn{
		config:              serverConfig{maxConcurrentStreams: maxConcurrentStreams, maxRapidResetsPerSecond: 1000},
		streams:             make(map[uint32]*serverStream),
		priorityUpdates:     make(map[uint32]priority),
		closedClientStreams: make(map[uint32]bool),
		framer:              xhttp2.NewFramer(io.Discard, nil),
	}
}

func isProtocolConnectionError(err error) bool {
	protocolErr, ok := errors.AsType[*serverError](err)
	return ok && protocolErr.code == xhttp2.ErrCodeProtocol
}

// RFC 9218 §7.1: the hints held for idle streams plus the open streams may
// not exceed SETTINGS_MAX_CONCURRENT_STREAMS; a signal that would is a
// connection error, hints for streams that are no longer idle don't count,
// and a re-signal for a prioritized idle stream fits (E3).
func TestPrioritizedIdleStreamsAreBoundedByConcurrentStreams(t *testing.T) {
	signal := func(conn *serverConn, streamID uint32) error {
		return conn.processPriorityUpdate(&incomingFrame{streamID: streamID, priority: "u=1"})
	}
	t.Run("second-idle-hint", func(t *testing.T) {
		conn := priorityTestConn(1)
		if err := signal(conn, 1); err != nil {
			t.Fatalf("first hint: %v", err)
		}
		if err := signal(conn, 1); err != nil {
			t.Fatalf("re-signal at the limit: %v", err)
		}
		if err := signal(conn, 3); !isProtocolConnectionError(err) {
			t.Fatalf("second idle hint error = %v, want connection PROTOCOL_ERROR", err)
		}
	})
	t.Run("hint-next-to-an-open-stream", func(t *testing.T) {
		conn := priorityTestConn(1)
		conn.streams[1] = &serverStream{id: 1}
		conn.openStreams, conn.lastClientStreamID = 1, 1
		if err := signal(conn, 3); !isProtocolConnectionError(err) {
			t.Fatalf("hint next to an open stream error = %v, want connection PROTOCOL_ERROR", err)
		}
	})
	t.Run("closed-streams-free-the-budget", func(t *testing.T) {
		conn := priorityTestConn(1)
		if err := signal(conn, 3); err != nil {
			t.Fatalf("hint for 3: %v", err)
		}
		// Streams 1 and 3 came and went: the hint for 3 is dead.
		conn.lastClientStreamID = 3
		if err := signal(conn, 5); err != nil {
			t.Fatalf("hint after the prioritized stream closed: %v", err)
		}
		if _, live := conn.priorityUpdates[3]; live || len(conn.priorityUpdates) != 1 {
			t.Fatalf("hints = %v, want only 5", conn.priorityUpdates)
		}
	})
	t.Run("open-stream-over-the-hints", func(t *testing.T) {
		// A hint for 3 then stream 1 opens: 1 hint + 1 stream > 1.
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
		testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if err := peer.framer.WritePriorityUpdate(3, "u=1"); err != nil {
			t.Fatal(err)
		}
		peer.request(1, "/", true)
		if counts, kind := peer.waitForAny(2*time.Second, "goaway_PROTOCOL_ERROR", "headers", "closed"); kind != "goaway_PROTOCOL_ERROR" {
			t.Fatalf("stream opened over the prioritized idle streams: %s %v", kind, counts)
		}
	})
	t.Run("wire", func(t *testing.T) {
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
		testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		for _, streamID := range []uint32{1, 3} {
			if err := peer.framer.WritePriorityUpdate(streamID, "u=1"); err != nil {
				t.Fatal(err)
			}
		}
		if counts, kind := peer.waitForAny(2*time.Second, "goaway_PROTOCOL_ERROR", "closed"); kind != "goaway_PROTOCOL_ERROR" {
			t.Fatalf("two prioritized idle streams under a limit of 1: %s %v", kind, counts)
		}
	})
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
	stream.body = newRequestBody(nil, 0)
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

// A body writer cut short by a reset may still use its RequestCtx after the
// pump exits (DESIGN.md lets it set trailers), so that ctx is abandoned
// rather than pooled; a response the peer lets finish still pools its ctx.
func TestResetDuringBodyStreamWriterAbandonsRequestCtx(t *testing.T) {
	ctxs := make(chan *fasthttp.RequestCtx, 4)
	writerFailed := make(chan struct{})
	touch := make(chan struct{})
	touched := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctxs <- ctx
			if string(ctx.Path()) != "/chat" {
				return
			}
			if err := ctx.Response.Header.AddTrailer("Grpc-Status"); err != nil {
				t.Errorf("AddTrailer() error: %v", err)
			}
			ctx.Response.Header.Set("Grpc-Status", "0")
			ctx.Response.SetBodyStreamWriter(func(w *bufio.Writer) {
				chunk := make([]byte, 1024)
				for {
					if _, err := w.Write(chunk); err != nil {
						break
					}
					if err := w.Flush(); err != nil {
						break
					}
				}
				close(writerFailed)
				<-touch
				ctx.Response.Header.Set("Grpc-Status", "13")
				close(touched)
			})
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// A closed send window keeps the writer blocked on the pump.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	nextCtx := func() *fasthttp.RequestCtx {
		t.Helper()
		select {
		case ctx := <-ctxs:
			return ctx
		case <-time.After(2 * time.Second):
			t.Fatal("handler didn't run")
			return nil
		}
	}
	peer.request(1, "/chat", true)
	first := nextCtx()
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		return ok && headers.StreamID == 1
	})
	if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("writer never saw the reset")
	}
	peer.request(3, "/next", true)
	peer.readHeaders(3, isStreamEnd(3))
	second := nextCtx()
	close(touch)
	<-touched
	if second == first {
		t.Fatalf("stream 3 got the RequestCtx (%p) stream 1's body writer still uses", first)
	}
	peer.request(5, "/next", true)
	peer.readHeaders(5, isStreamEnd(5))
	if third := nextCtx(); third != second {
		t.Fatalf("completed response's RequestCtx wasn't pooled: %p, then %p", second, third)
	}
}

// A reset that lands before the response pump starts leaves the
// SetBodyStreamWriter goroutine, started with the call, running with the
// RequestCtx: finalization abandons that ctx instead of pooling it.
// A stream reset before its pump starts never runs its body writer: the
// writer starts on the first Read, which nothing issues.
func TestResetBeforePumpStartsLeavesTheWriterUnstarted(t *testing.T) {
	ctxs := make(chan *fasthttp.RequestCtx, 2)
	releaseHandler := make(chan struct{})
	writerStarted := make(chan struct{})
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctxs <- ctx
		if string(ctx.Path()) != "/first" {
			return
		}
		ctx.Response.SetBodyStreamWriter(func(w *bufio.Writer) {
			close(writerStarted)
			ctx.Response.Header.Set("X-Writer", "ran")
		})
		<-releaseHandler
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	nextCtx := func() *fasthttp.RequestCtx {
		t.Helper()
		select {
		case ctx := <-ctxs:
			return ctx
		case <-time.After(2 * time.Second):
			t.Fatal("handler didn't run")
			return nil
		}
	}
	peer.request(1, "/first", true)
	_ = nextCtx()
	// The reset lands while the handler still runs: no pump has started.
	if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isPingAck)
	close(releaseHandler)
	peer.request(3, "/second", true)
	peer.readHeaders(3, isStreamEnd(3))
	_ = nextCtx()
	select {
	case <-writerStarted:
		t.Fatal("the body writer ran for a stream reset before its pump started")
	case <-time.After(50 * time.Millisecond):
	}
}

// closeObservedConn reports its Close; it stays a net.Conn.
type closeObservedConn struct {
	net.Conn

	closed chan struct{}
	once   sync.Once
}

func (c *closeObservedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// A response body reader blocked in Read is closed when the stream is reset
// or the connection ends, so the pump exits and shutdown completes.
func TestResetClosesBlockedResponseReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(peer *rawPeer) error
	}{
		{name: "reset", end: func(peer *rawPeer) error { return peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel) }},
		{name: "connection-close", end: func(peer *rawPeer) error { return peer.conn.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := net.Pipe()
			t.Cleanup(func() { _ = writer.Close() })
			body := &closeObservedConn{Conn: reader, closed: make(chan struct{})}
			started := make(chan struct{})
			server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
				ctx.SetBodyStream(body, -1)
				close(started)
			}}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			peer.request(1, "/", true)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("handler didn't run")
			}
			peer.readHeaders(1, func(frame xhttp2.Frame) bool {
				headers, ok := frame.(*xhttp2.MetaHeadersFrame)
				return ok && headers.StreamID == 1
			})
			if err := tc.end(peer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-body.closed:
			case <-time.After(2 * time.Second):
				t.Fatalf("body reader still open after the %s", tc.name)
			}
		})
	}
}

func TestPeerResetDiscardsAlreadyCreditedRequestBody(t *testing.T) {
	conn := &serverConn{
		config:  serverConfig{connectionWindowSize: 1024, streamWindowSize: 1024},
		framer:  xhttp2.NewFramer(io.Discard, nil),
		streams: make(map[uint32]*serverStream),
	}
	conn.initQueues()
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.unconsumedFlow = 4
	stream.body = newRequestBody(conn, stream.id)
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
	if more, err := conn.flushStream(stream, false); err != nil || !more {
		t.Fatalf("partial flush = %v, %v; want more to send", more, err)
	}
	if err := conn.processCommand(&serverCommand{
		kind: serverCommandCancelStream, streamID: 1, err: errStreamTimeout,
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

// A response not queued whole by its deadline is reset with INTERNAL_ERROR
// and its pending data released (E7).
func TestResponseWriteTimeoutResetsStream(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
		},
		ctx:      ctx,
		commands: make(chan serverCommand, 1),
		framer:   xhttp2.NewFramer(&wire, nil),
		streams:  make(map[uint32]*serverStream),
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.writeTimeout = 20 * time.Millisecond
	stream.pendingData = []byte("blocked")
	conn.streams[stream.id] = stream
	conn.armResponseWriteTimeout(stream)
	select {
	case command := <-conn.commands:
		if err := conn.processCommand(&command); err != nil {
			t.Fatalf("processing the response deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response deadline didn't fire")
	}
	if !stream.isReset || len(stream.pendingData) != 0 {
		t.Fatalf("stream = {reset:%v pending:%d}, want reset and released", stream.isReset, len(stream.pendingData))
	}
	frame, err := xhttp2.NewFramer(nil, &wire).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if rst, ok := frame.(*xhttp2.RSTStreamFrame); !ok || rst.ErrCode != xhttp2.ErrCodeInternal {
		t.Fatalf("frame = %v, want RST_STREAM(INTERNAL_ERROR)", frame)
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

// headerBlock encodes fields with the peer's HPACK encoder.
func (p *rawPeer) headerBlock(fields ...[2]string) []byte {
	p.t.Helper()
	p.block.Reset()
	for _, field := range fields {
		if err := p.encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			p.t.Fatalf("WriteField() error: %v", err)
		}
	}
	return append([]byte(nil), p.block.Bytes()...)
}

func (p *rawPeer) writeHeaders(streamID uint32, endStream bool, fields ...[2]string) {
	p.t.Helper()
	if err := p.framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: p.headerBlock(fields...),
		EndStream:     endStream,
		EndHeaders:    true,
	}); err != nil {
		p.t.Fatalf("WriteHeaders() error: %v", err)
	}
}

// requestFields is a GET's header block for path.
func requestFields(path string) [][2]string {
	return [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", path}}
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
func TestStreamConnClearedWriteDeadlineReachesBlockedWrite(t *testing.T) {
	conns := make(chan fasthttp.StreamConn, 1)
	finished := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				conns <- conn
				<-finished
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// One byte of send window: the write blocks after its first byte.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":protocol", "websocket"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/ws"},
	)
	conn := <-conns
	defer close(finished)
	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	payload := []byte("payload")
	written := make(chan error, 1)
	go func() {
		n, err := conn.Write(payload)
		if err == nil && n != len(payload) {
			err = fmt.Errorf("wrote %d of %d bytes", n, len(payload))
		}
		written <- err
	}()
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		data, ok := frame.(*xhttp2.DataFrame)
		return ok && data.StreamID == 1
	})
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("SetWriteDeadline() error: %v", err)
	}
	select {
	case err := <-written:
		t.Fatalf("Write() ended after its deadline was cleared: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := peer.framer.WriteWindowUpdate(1, 1<<16); err != nil {
		t.Fatalf("WriteWindowUpdate() error: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("Write() error: %v", err)
	}
}

func TestStreamConnCloseOutlivingHandler(t *testing.T) {
	// A Close from another goroutine can get in first, which makes the
	// handler's own Close return at once; the stream then finalizes while that
	// goroutine is still closing. Its remaining calls must fail cleanly rather
	// than reach a recycled stream.
	conns := make(chan *streamConn, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
				conn := stream.(*streamConn) //nolint:forcetypeassert
				// What that Close does first.
				conn.mu.Lock()
				conn.isClosed = true
				conn.writeClosed = true
				conn.mu.Unlock()
				conns <- conn
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
	conn := <-conns
	peer.waitForAny(2*time.Second, "rst_NO_ERROR")
	// The event loop answers a PING only after it finalized the stream.
	if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
		t.Fatalf("WritePing() error: %v", err)
	}
	for {
		_ = peer.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for the PING ack: %v", err)
		}
		if ping, ok := frame.(*xhttp2.PingFrame); ok && ping.IsAck() {
			break
		}
	}

	// That Close already closed the write side, so CloseWrite has nothing to
	// do; CloseRead still asks the owner, which no longer has the stream.
	for name, tc := range map[string]struct {
		call func() error
		want error
	}{
		"CloseRead":  {conn.CloseRead, errStreamClosed},
		"CloseWrite": {conn.CloseWrite, nil},
	} {
		result := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					result <- fmt.Errorf("panic: %v", r)
				}
			}()
			result <- tc.call()
		}()
		select {
		case err := <-result:
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s() on the finalized stream = %v, want %v", name, err, tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s() on the finalized stream never returned", name)
		}
	}
	// A deadline timer firing this late finds nothing to interrupt; it must
	// not reach the stream either.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("late deadline: panic: %v", r)
			}
		}()
		conn.wakeRead()
		conn.interruptWrite()
	}()
}

// RequestCtx is a server-scoped context, as for HTTP/1: Done and Err may be
// called after the handler returned while the context serves other streams,
// and Shutdown closes Done.
func TestRequestCtxContextIsServerScoped(t *testing.T) {
	const requests = 50
	release := make(chan struct{})
	lateErrs := make(chan error, requests)
	entered := make(chan struct{})
	shutdownErr := make(chan error, 1)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) == "/wait" {
			close(entered)
			<-ctx.Done()
			shutdownErr <- ctx.Err()
			return
		}
		done := ctx.Done()
		go func() {
			<-release
			select {
			case <-done:
				lateErrs <- errors.New("Done closed before shutdown")
			default:
				lateErrs <- ctx.Err()
			}
		}()
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	for range requests {
		resp, err := testServer.client.Get(testServer.URL("/"))
		if err != nil {
			t.Fatalf("Get() error: %v", err)
		}
		resp.Body.Close()
	}
	close(release)
	for range requests {
		if err := <-lateErrs; err != nil {
			t.Fatalf("late Err() = %v, want nil before shutdown", err)
		}
	}

	go func() {
		if resp, err := testServer.client.Get(testServer.URL("/wait")); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	go func() { _ = server.Shutdown() }()
	select {
	case err := <-shutdownErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Err() after Shutdown = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown didn't close Done for an HTTP/2 handler")
	}
}

func TestTimedOutHandlerCannotReachItsStream(t *testing.T) {
	// TimeoutHandler answers without waiting for the handler, which may still
	// use its RequestCtx after the stream ended, including when the connection
	// closed before the event loop saw the handler return.
	for _, closeConn := range []bool{false, true} {
		t.Run(fmt.Sprintf("connection-closed=%v", closeConn), func(t *testing.T) {
			started := make(chan struct{})
			late := make(chan struct{})
			result := make(chan error, 1)
			handler := func(ctx *fasthttp.RequestCtx) {
				close(started)
				<-late
				defer func() {
					if r := recover(); r != nil {
						result <- fmt.Errorf("panic: %v", r)
					}
				}()
				result <- ctx.AcceptStream(func(fasthttp.StreamConn) {})
			}
			closed := make(chan struct{})
			server := &fasthttp.Server{
				Handler: fasthttp.TimeoutHandler(handler, 10*time.Millisecond, "timeout"),
				ConnState: func(_ net.Conn, state fasthttp.ConnState) {
					if state == fasthttp.StateClosed {
						close(closed)
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
			<-started
			if closeConn {
				_ = peer.conn.Close()
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("the server didn't finish the connection")
				}
			} else {
				// The timeout response ends the stream, which the server then
				// finalizes.
				peer.waitForAny(2*time.Second, "rst_NO_ERROR")
			}
			close(late)
			if err := <-result; !errors.Is(err, errStreamClosed) {
				t.Fatalf("AcceptStream() from the abandoned handler = %v, want %v", err, errStreamClosed)
			}
		})
	}
}

// readHeaders reads frames until stop reports true and returns the :status of
// every HEADERS block on streamID.
func (p *rawPeer) readHeaders(streamID uint32, stop func(xhttp2.Frame) bool) []string {
	p.t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var statuses []string
	for {
		frame, err := p.framer.ReadFrame()
		if err != nil {
			p.t.Fatalf("ReadFrame() error: %v", err)
		}
		if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok && headers.StreamID == streamID {
			statuses = append(statuses, headers.PseudoValue("status"))
		}
		if stop(frame) {
			return statuses
		}
	}
}

func isPingAck(frame xhttp2.Frame) bool {
	ping, ok := frame.(*xhttp2.PingFrame)
	return ok && ping.IsAck()
}

func TestTimedOutHandlerHijackAndInterimAreIgnored(t *testing.T) {
	// As in HTTP/1, the timeout response stands over a hijack by the handler
	// that timed out, and no 1xx may follow it.
	late := make(chan struct{})
	hints := make(chan error, 1)
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.Hijack(func(net.Conn) {})
		<-late
		ctx.Hijack(func(net.Conn) {})
		ctx.Response.Header.Add("Link", "</style.css>; rel=preload")
		hints <- ctx.EarlyHints()
	}
	server := &fasthttp.Server{Handler: fasthttp.TimeoutHandler(handler, 10*time.Millisecond, "timeout")}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// A closed send window holds the timeout body, keeping the stream open
	// after its final HEADERS.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	peer.request(1, "/", true)
	statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		return ok && headers.StreamID == 1
	})
	if want := strconv.Itoa(fasthttp.StatusRequestTimeout); len(statuses) != 1 || statuses[0] != want {
		t.Fatalf("response statuses = %q, want [%s]", statuses, want)
	}

	close(late)
	if err := <-hints; !errors.Is(err, errStreamClosed) {
		t.Fatalf("EarlyHints() after the final response = %v, want %v", err, errStreamClosed)
	}
	if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
		t.Fatalf("WritePing() error: %v", err)
	}
	if statuses := peer.readHeaders(1, isPingAck); len(statuses) != 0 {
		t.Fatalf("HEADERS after the final response: %q", statuses)
	}
}

func TestTimedOutHandlerRequestChangesDontShapeTimeoutResponse(t *testing.T) {
	late := make(chan struct{})
	handler := func(ctx *fasthttp.RequestCtx) {
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		<-late
		ctx.Request.Header.SetMethod(fasthttp.MethodPost)
	}
	server := &fasthttp.Server{Handler: fasthttp.TimeoutHandler(handler, 10*time.Millisecond, "timeout")}
	testServer := newTestServer(t, server, ServerConfig{})
	defer close(late)
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.writeHeaders(1, true,
		[2]string{":method", fasthttp.MethodHead},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/"},
	)
	_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame() error: %v", err)
		}
		switch frame := frame.(type) {
		case *xhttp2.DataFrame:
			if frame.StreamID == 1 && len(frame.Data()) != 0 {
				t.Fatalf("HEAD timeout response has a body: %q", frame.Data())
			}
			if frame.StreamID == 1 && frame.StreamEnded() {
				return
			}
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID == 1 && frame.StreamEnded() {
				return
			}
		}
	}
}

func TestTimedOutHandlerAcceptedStreamDoesNotStart(t *testing.T) {
	var started atomic.Bool
	handler := func(ctx *fasthttp.RequestCtx) {
		if err := ctx.AcceptStream(func(fasthttp.StreamConn) { started.Store(true) }); err != nil {
			t.Errorf("AcceptStream() error: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A 2xx without a body is what an accepted stream answers with.
	server := &fasthttp.Server{Handler: fasthttp.TimeoutWithCodeHandler(handler, 10*time.Millisecond, "", fasthttp.StatusOK)}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodConnect},
		[2]string{":protocol", "websocket"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/ws"},
	)
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.RSTStreamFrame:
			return frame.StreamID == 1
		case *xhttp2.MetaHeadersFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		case *xhttp2.DataFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		}
		return false
	})
	if started.Load() {
		t.Fatal("the timed-out handler's stream handler ran")
	}
}

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
		// processing, so follow-ups may still see REFUSED_STREAM for as long
		// as that goroutine waits to run. A real leak never frees the slot, so
		// retrying for a bounded time tells the two apart.
		accepted := false
		for deadline := time.Now().Add(2 * time.Second); !accepted && time.Now().Before(deadline); {
			peer.request(streamID, "/plain", true)
			streamID += 2
			_, kind := peer.waitForAny(2*time.Second, "headers", "rst_REFUSED_STREAM")
			if accepted = kind == "headers"; !accepted {
				time.Sleep(time.Millisecond)
			}
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

// deadlineRecordingConn records the read deadlines set on it in completion
// order and can hold one arming call open until released.
type deadlineRecordingConn struct {
	net.Conn

	mu        sync.Mutex
	deadlines []time.Time
	holdArm   bool
	held      chan struct{}
	release   chan struct{}
}

func (c *deadlineRecordingConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	hold := c.holdArm && deadline.After(time.Now())
	if hold {
		c.holdArm = false
	}
	c.mu.Unlock()
	if hold {
		close(c.held)
		<-c.release
	}
	c.mu.Lock()
	c.deadlines = append(c.deadlines, deadline)
	c.mu.Unlock()
	return nil
}

func (c *deadlineRecordingConn) recorded() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.deadlines)
}

// The reader's header-block deadline and drainBeforeClose's interruption of
// the reader are ordered: the interrupt waits for an arming in progress, and
// the clear that ends the block never overwrites it (E6).
func TestHeaderBlockDeadlineYieldsToTheReaderInterrupt(t *testing.T) {
	fixture := &deadlineRecordingConn{holdArm: true, held: make(chan struct{}), release: make(chan struct{})}
	conn := &serverConn{server: &fasthttp.Server{ReadTimeout: time.Hour}, conn: fixture}
	armed := make(chan struct{})
	go func() {
		conn.armHeaderBlockDeadline()
		close(armed)
	}()
	<-fixture.held
	interrupted := make(chan struct{})
	go func() {
		conn.interruptReader()
		close(interrupted)
	}()
	select {
	case <-interrupted:
		t.Fatal("the interrupt overtook the reader's arming in progress")
	case <-time.After(100 * time.Millisecond):
	}
	close(fixture.release)
	<-armed
	<-interrupted
	// The block ends after the interrupt; the reader must stay interrupted.
	conn.clearHeaderBlockDeadline()
	deadlines := fixture.recorded()
	if len(deadlines) != 2 || !deadlines[0].After(time.Now()) || !deadlines[1].Equal(time.Unix(1, 0)) {
		t.Fatalf("read deadlines = %v, want the arming, then the interrupt, nothing after", deadlines)
	}
}

// Server.ReadTimeout bounds a request's header block, HEADERS payload and
// CONTINUATIONs together, as it bounds HTTP/1's request header read; the
// stream's body timer starts afterwards. Without it a block may take as long
// as it likes (E6).
func TestReadTimeoutBoundsTheHeaderBlock(t *testing.T) {
	const readTimeout = 40 * time.Millisecond
	newServer := func(t *testing.T, readTimeout time.Duration) *testServer {
		t.Helper()
		// A ReadTimeout is also the idle timeout's fallback; the idle timer
		// must not end these connections.
		server := &fasthttp.Server{
			ReadTimeout: readTimeout,
			IdleTimeout: 5 * time.Second,
			Handler:     func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
		}
		return newTestServer(t, server, ServerConfig{})
	}
	// outcome is "200", or "closed" when the connection ended first.
	outcome := func(peer *rawPeer) string {
		peer.t.Helper()
		counts, kind := peer.waitForAny(3*time.Second, "headers", "goaway_PROTOCOL_ERROR", "closed")
		if kind == "headers" {
			return "200"
		}
		if counts["headers"] != 0 {
			peer.t.Fatalf("response next to %s: %v", kind, counts)
		}
		return "closed"
	}
	// continuations sends a block as HEADERS plus n CONTINUATIONs, pausing
	// before each; a write the closed connection refuses counts as closed.
	continuations := func(peer *rawPeer, n int, pause time.Duration) {
		peer.t.Helper()
		if err := peer.framer.WriteHeaders(xhttp2.HeadersFrameParam{
			StreamID: 1, BlockFragment: peer.headerBlock(requestFields("/")...), EndStream: true,
		}); err != nil {
			peer.t.Fatal(err)
		}
		for i := range n {
			time.Sleep(pause)
			if err := peer.framer.WriteContinuation(1, i == n-1, nil); err != nil {
				return
			}
		}
	}
	t.Run("late-continuation", func(t *testing.T) {
		peer := dialRawPeer(t, newServer(t, readTimeout).listener.Addr().String())
		continuations(peer, 1, 150*time.Millisecond)
		if got := outcome(peer); got != "closed" {
			t.Fatalf("a header block completed after ReadTimeout got %s", got)
		}
	})
	t.Run("prompt-continuation", func(t *testing.T) {
		peer := dialRawPeer(t, newServer(t, readTimeout).listener.Addr().String())
		continuations(peer, 1, 10*time.Millisecond)
		if got := outcome(peer); got != "200" {
			t.Fatalf("a header block completed within ReadTimeout got %s", got)
		}
	})
	t.Run("continuations-exceeding-the-budget-together", func(t *testing.T) {
		peer := dialRawPeer(t, newServer(t, readTimeout).listener.Addr().String())
		continuations(peer, 3, 20*time.Millisecond)
		if got := outcome(peer); got != "closed" {
			t.Fatalf("a header block of prompt CONTINUATIONs exceeding ReadTimeout together got %s", got)
		}
	})
	t.Run("no-read-timeout", func(t *testing.T) {
		peer := dialRawPeer(t, newServer(t, 0).listener.Addr().String())
		continuations(peer, 1, 150*time.Millisecond)
		if got := outcome(peer); got != "200" {
			t.Fatalf("without a ReadTimeout a slow header block got %s", got)
		}
	})
	t.Run("slow-headers-payload", func(t *testing.T) {
		peer := dialRawPeer(t, newServer(t, readTimeout).listener.Addr().String())
		var wire bytes.Buffer
		if err := xhttp2.NewFramer(&wire, nil).WriteHeaders(xhttp2.HeadersFrameParam{
			StreamID: 1, BlockFragment: peer.headerBlock(requestFields("/")...), EndStream: true, EndHeaders: true,
		}); err != nil {
			t.Fatal(err)
		}
		frame := wire.Bytes()
		// The frame header at once, then the payload a byte at a time over
		// 150ms; a write the closed connection refuses counts as closed.
		if _, err := peer.conn.Write(frame[:9]); err != nil {
			t.Fatal(err)
		}
		pause := 150 * time.Millisecond / time.Duration(len(frame)-9)
		for _, b := range frame[9:] {
			time.Sleep(pause)
			if _, err := peer.conn.Write([]byte{b}); err != nil {
				break
			}
		}
		if got := outcome(peer); got != "closed" {
			t.Fatalf("a HEADERS payload spread over more than ReadTimeout got %s", got)
		}
	})
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

// RFC 9113 §5.1: frames a client had in flight when the server reset its
// stream, trailers included, are ignored rather than failing the connection or
// drawing another RST_STREAM.
func TestFramesAfterServerResetAreIgnored(t *testing.T) {
	post := [][2]string{
		{":method", "POST"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/"},
	}
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "early-response", true: "refused"}[refuse], func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			server := &fasthttp.Server{
				StreamRequestBody: true,
				Handler: func(ctx *fasthttp.RequestCtx) {
					if string(ctx.Path()) == "/block" {
						<-release
					}
					ctx.SetBodyString("early")
				},
			}
			config := ServerConfig{}
			if refuse {
				config.MaxConcurrentStreams = 1
			}
			testServer := newTestServer(t, server, config)
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			if refuse {
				peer.request(1, "/block", true)
			}
			peer.writeHeaders(3, false, post...)
			want := xhttp2.ErrCodeNo
			if refuse {
				want = xhttp2.ErrCodeRefusedStream
			}
			var resets []xhttp2.ErrCode
			stopAtReset := func(frame xhttp2.Frame) bool {
				switch frame := frame.(type) {
				case *xhttp2.RSTStreamFrame:
					if frame.StreamID == 3 {
						resets = append(resets, frame.ErrCode)
						return true
					}
				case *xhttp2.GoAwayFrame:
					t.Fatalf("GOAWAY(%v) before the reset", frame.ErrCode)
				}
				return false
			}
			peer.readHeaders(3, stopAtReset)
			if len(resets) != 1 || resets[0] != want {
				t.Fatalf("resets on stream 3 = %v, want [%v]", resets, want)
			}

			if err := peer.framer.WriteData(3, false, []byte("in flight")); err != nil {
				t.Fatal(err)
			}
			peer.writeHeaders(3, true, [2]string{"x-checksum", "abc"})
			if err := peer.framer.WriteWindowUpdate(3, 100); err != nil {
				t.Fatal(err)
			}
			if err := peer.framer.WriteRSTStream(3, xhttp2.ErrCodeCancel); err != nil {
				t.Fatal(err)
			}
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			peer.readHeaders(3, func(frame xhttp2.Frame) bool {
				switch frame := frame.(type) {
				case *xhttp2.RSTStreamFrame:
					if frame.StreamID == 3 {
						t.Fatalf("second RST_STREAM(%v) on the reset stream", frame.ErrCode)
					}
				case *xhttp2.GoAwayFrame:
					t.Fatalf("GOAWAY(%v) for frames on the reset stream", frame.ErrCode)
				}
				return isPingAck(frame)
			})
		})
	}
}

// The server's own resets, and frames the client had in flight on the streams
// they ended, are not peer resets and don't count toward the rapid reset limit.
func TestServerOwnResetsDontCountAsRapidResets(t *testing.T) {
	var wire bytes.Buffer
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    10,
			maxRapidResetsPerSecond: 1,
			connectionWindowSize:    1 << 20,
			streamWindowSize:        1 << 20,
		},
		framer:              xhttp2.NewFramer(&wire, nil),
		streams:             make(map[uint32]*serverStream),
		closedClientStreams: make(map[uint32]bool),
		lastClientStreamID:  5,
		connFlowState: connFlowState{
			receivedSettings: true,
			recv:             recvWindow{window: 1 << 20},
		},
	}
	// Complete responses sent before the request ended.
	for _, id := range []uint32{1, 3} {
		stream := newServerStream(conn, id)
		stream.localClosed = true
		conn.streams[id] = stream
		if err := conn.finishResponse(stream); err != nil {
			t.Fatalf("early response on stream %d: %v", id, err)
		}
	}
	// A stream its accepted StreamConn closed; its handler still runs.
	stream := newServerStream(conn, 5)
	stream.handlerStarted = true
	conn.streams[5] = stream
	if err := conn.processCommand(&serverCommand{
		kind: serverCommandCancelStream, streamID: 5, err: net.ErrClosed,
	}); err != nil {
		t.Fatalf("stream cancel: %v", err)
	}
	for _, id := range []uint32{1, 3, 5} {
		for _, event := range []incomingFrame{
			{kind: incomingFrameData, streamID: id, flowLength: 4, data: []byte("body")},
			{kind: incomingFrameRST, streamID: id, errCode: xhttp2.ErrCodeCancel},
		} {
			if err := conn.processFrame(&event); err != nil {
				t.Fatalf("frame %d on reset stream %d: %v", event.kind, id, err)
			}
		}
	}
	if conn.rapidResetCount != 0 {
		t.Fatalf("rapid reset count = %d, want 0", conn.rapidResetCount)
	}
	if want := int64(1<<20 - 12); conn.recv.window != want || conn.recv.pending != 12 {
		t.Fatalf("connection window = %d pending %d, want %d pending 12", conn.recv.window, conn.recv.pending, want)
	}
	resets := map[uint32]int{}
	framer := xhttp2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	for {
		frame, err := framer.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			resets[rst.StreamID]++
		}
	}
	if resets[1] != 1 || resets[3] != 1 || resets[5] != 1 {
		t.Fatalf("RST_STREAM frames per stream = %v, want one each", resets)
	}
}

// An upload the handler answers early costs the client nothing toward the
// rapid reset limit, however fast it repeats.
func TestEarlyResponsesToUploadsDontTripRapidResetLimit(t *testing.T) {
	testServer := newTestServer(t, &fasthttp.Server{
		StreamRequestBody: true,
		Handler:           func(ctx *fasthttp.RequestCtx) { ctx.SetStatusCode(fasthttp.StatusUnauthorized) },
	}, ServerConfig{MaxRapidResetsPerSecond: 2})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	for id := uint32(1); id <= 9; id += 2 {
		peer.writeHeaders(id, false,
			[2]string{":method", "POST"}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
		)
		peer.readHeaders(id, func(frame xhttp2.Frame) bool {
			if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
				t.Fatalf("GOAWAY(%v) at stream %d", goAway.ErrCode, id)
			}
			rst, ok := frame.(*xhttp2.RSTStreamFrame)
			return ok && rst.StreamID == id
		})
		// The upload already in flight, then the client's own cancel.
		if err := peer.framer.WriteData(id, false, []byte("rest")); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WriteRSTStream(id, xhttp2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.framer.WritePing(false, [8]byte{2}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(0, func(frame xhttp2.Frame) bool {
		if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
			t.Fatalf("GOAWAY(%v) after well-behaved uploads", goAway.ErrCode)
		}
		return isPingAck(frame)
	})
}

// Expect: 100-continue is answered as HTTP/1 answers it: ExpectHandler, then
// ContinueHandler, decide, and an accepted request gets 100 before its body.
func TestServerExpectContinue(t *testing.T) {
	expectPost := func(peer *rawPeer, endStream bool) {
		peer.writeHeaders(1, endStream,
			[2]string{":method", "POST"}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/"},
			[2]string{"expect", "100-Continue"},
		)
	}
	untilEnd := func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.DataFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		case *xhttp2.MetaHeadersFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		case *xhttp2.GoAwayFrame:
			return true
		}
		return false
	}
	accept := func(*fasthttp.RequestHeader) bool { return true }
	for _, tc := range []struct {
		name            string
		continueHandler func(*fasthttp.RequestHeader) bool
		expectHandler   func(*fasthttp.RequestCtx) int
		statuses        []string
	}{
		{name: "default", statuses: []string{"100", "200"}},
		{name: "continue-handler-accepts", continueHandler: accept, statuses: []string{"100", "200"}},
		{
			name:            "continue-handler-rejects",
			continueHandler: func(*fasthttp.RequestHeader) bool { return false },
			statuses:        []string{"417"},
		},
		{
			name:            "expect-handler-takes-precedence",
			continueHandler: accept,
			expectHandler:   func(*fasthttp.RequestCtx) int { return fasthttp.StatusUnauthorized },
			statuses:        []string{"401"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handled atomic.Bool
			server := &fasthttp.Server{
				ContinueHandler: tc.continueHandler,
				ExpectHandler:   tc.expectHandler,
				Handler: func(ctx *fasthttp.RequestCtx) {
					handled.Store(true)
					ctx.SetBody(ctx.Request.Body())
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			expectPost(peer, false)
			// The interim or final answer arrives before any body is sent.
			statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
				headers, ok := frame.(*xhttp2.MetaHeadersFrame)
				return ok && headers.StreamID == 1
			})
			if len(statuses) != 1 || statuses[0] != tc.statuses[0] {
				t.Fatalf("answer before the body = %q, want [%s]", statuses, tc.statuses[0])
			}
			if err := peer.framer.WriteData(1, true, []byte("body")); err != nil {
				t.Fatal(err)
			}
			if tc.statuses[0] == "100" {
				statuses = append(statuses, peer.readHeaders(1, untilEnd)...)
			} else {
				// A body the client sent anyway is ignored; the complete
				// response may be followed by RST_STREAM(NO_ERROR).
				if err := peer.framer.WritePing(false, [8]byte{4}); err != nil {
					t.Fatal(err)
				}
				peer.readHeaders(1, func(frame xhttp2.Frame) bool {
					switch frame := frame.(type) {
					case *xhttp2.GoAwayFrame:
						t.Fatalf("GOAWAY(%v) after a rejected expectation", frame.ErrCode)
					case *xhttp2.RSTStreamFrame:
						if frame.ErrCode != xhttp2.ErrCodeNo {
							t.Fatalf("RST_STREAM(%v) for the ignored body", frame.ErrCode)
						}
					}
					return isPingAck(frame)
				})
			}
			if !slices.Equal(statuses, tc.statuses) {
				t.Fatalf("statuses = %q, want %q", statuses, tc.statuses)
			}
			if handled.Load() != (tc.statuses[0] == "100") {
				t.Fatalf("handler ran = %v for statuses %q", handled.Load(), statuses)
			}
		})
	}
	t.Run("complete-request", func(t *testing.T) {
		testServer := newTestServer(t, &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {},
		}, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		expectPost(peer, true)
		if statuses := peer.readHeaders(1, untilEnd); !slices.Equal(statuses, []string{"200"}) {
			t.Fatalf("statuses = %q, want [200]", statuses)
		}
	})
}

// ExpectHandler and ContinueHandler decide for every request carrying the
// expectation, as HTTP/1 consults them, also for one complete at its header
// block; an accepted complete request gets no 100. A positive declared
// length with END_STREAM still fails length validation.
func TestExpectCallbacksRunForCompleteRequests(t *testing.T) {
	type policy struct {
		name            string
		expectHandler   func(*fasthttp.RequestCtx) int
		continueHandler func(*fasthttp.RequestHeader) bool
		status          string
	}
	policies := []policy{
		{name: "expect-handler", expectHandler: func(*fasthttp.RequestCtx) int { return fasthttp.StatusUnauthorized }, status: "401"},
		{name: "continue-handler", continueHandler: func(*fasthttp.RequestHeader) bool { return false }, status: "417"},
		{name: "accepted", status: "200"},
	}
	for _, streaming := range []bool{false, true} {
		for _, method := range []string{fasthttp.MethodGet, fasthttp.MethodPost} {
			for _, tc := range policies {
				name := map[bool]string{false: "buffered", true: "streaming"}[streaming] + "/" + method + "/" + tc.name
				t.Run(name, func(t *testing.T) {
					lengths := make(chan int, 2)
					server := &fasthttp.Server{
						StreamRequestBody: streaming,
						ExpectHandler:     tc.expectHandler,
						ContinueHandler:   tc.continueHandler,
						Handler: func(ctx *fasthttp.RequestCtx) {
							lengths <- ctx.Request.Header.ContentLength()
						},
					}
					testServer := newTestServer(t, server, ServerConfig{})
					addr := testServer.listener.Addr().String()
					// HTTP/1 writes the 100 of an accepted request without
					// looking for a body; HTTP/2 knows the request is complete.
					wire := http1Exchange(t, addr, method+" / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\n"+
						"Expect: 100-continue\r\nConnection: close\r\n\r\n")
					if !strings.Contains(wire, "HTTP/1.1 "+tc.status+" ") ||
						tc.status != "200" && strings.Contains(wire, "100 Continue") {
						t.Fatalf("HTTP/1 control = %q, want %s", wire, tc.status)
					}
					peer := dialRawPeer(t, addr)
					peer.writeHeaders(1, true,
						[2]string{":method", method}, [2]string{":scheme", "http"},
						[2]string{":authority", "example.com"}, [2]string{":path", "/"},
						[2]string{"content-length", "0"}, [2]string{"expect", "100-continue"},
					)
					if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{tc.status}) {
						t.Fatalf("statuses = %q, want [%s]", statuses, tc.status)
					}
					if tc.status != "200" {
						if len(lengths) != 0 {
							t.Fatal("the handler ran for a rejected expectation")
						}
						return
					}
					if http1, http2 := <-lengths, <-lengths; http1 != http2 {
						t.Fatalf("handler saw ContentLength() %d over HTTP/1 and %d over HTTP/2", http1, http2)
					}
				})
			}
		}
	}
	t.Run("declared-length-mismatch", func(t *testing.T) {
		var handled atomic.Bool
		server := &fasthttp.Server{
			ExpectHandler: func(*fasthttp.RequestCtx) int { return fasthttp.StatusContinue },
			Handler:       func(*fasthttp.RequestCtx) { handled.Store(true) },
		}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.writeHeaders(1, true,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/"},
			[2]string{"content-length", "5"}, [2]string{"expect", "100-continue"},
		)
		status, reset := streamOutcome(peer, 1)
		if status != "" || reset == nil || *reset != xhttp2.ErrCodeProtocol {
			t.Fatalf("status = %q, reset = %v, want RST_STREAM(PROTOCOL_ERROR) alone", status, reset)
		}
		if err := peer.framer.WritePing(false, [8]byte{5}); err != nil {
			t.Fatal(err)
		}
		peer.readHeaders(1, isPingAck)
		if handled.Load() {
			t.Fatal("the handler ran for a request whose body didn't match its length")
		}
	})
}

func writeExpectPost(peer *rawPeer, streamID uint32, path string) {
	peer.t.Helper()
	peer.writeHeaders(streamID, false,
		[2]string{":method", "POST"}, [2]string{":scheme", "http"},
		[2]string{":authority", "example.com"}, [2]string{":path", path},
		[2]string{"expect", "100-continue"},
	)
}

func isStreamEnd(streamID uint32) func(xhttp2.Frame) bool {
	return func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.DataFrame:
			return frame.StreamID == streamID && frame.StreamEnded()
		case *xhttp2.MetaHeadersFrame:
			return frame.StreamID == streamID && frame.StreamEnded()
		}
		return false
	}
}

// A rejected expectation never starts the handler, also when the client's
// END_STREAM arrives while the rejection is still being flushed.
func TestRejectedExpectNeverStartsHandler(t *testing.T) {
	var calls atomic.Int32
	server := &fasthttp.Server{
		ExpectHandler: func(ctx *fasthttp.RequestCtx) int {
			ctx.SetBodyString("denied")
			return fasthttp.StatusExpectationFailed
		},
		Handler: func(ctx *fasthttp.RequestCtx) {
			calls.Add(1)
			ctx.SetBodyString("handler ran")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// A closed send window keeps the rejection body pending while the
	// request body arrives.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	writeExpectPost(peer, 1, "/rejected")
	if err := peer.framer.WriteData(1, true, []byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteWindowUpdate(1, 1<<20); err != nil {
		t.Fatal(err)
	}
	if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"417"}) {
		t.Fatalf("statuses = %q, want [417]", statuses)
	}
	peer.request(3, "/next", true)
	if err := peer.framer.WriteWindowUpdate(3, 1<<20); err != nil {
		t.Fatal(err)
	}
	if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("stream 3 statuses = %q, want [200]", statuses)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want once for stream 3", got)
	}
}

// GetOnly answers a POST through ErrorHandler with HTTP/1's status instead of
// a retryable REFUSED_STREAM; the handler runs off the event loop.
func TestGetOnlyAnswersThroughErrorHandler(t *testing.T) {
	post := func(peer *rawPeer) []string {
		peer.t.Helper()
		peer.writeHeaders(1, false,
			[2]string{":method", "POST"}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
		)
		if err := peer.framer.WriteData(1, true, []byte("body")); err != nil {
			peer.t.Fatal(err)
		}
		return peer.readHeaders(1, isStreamEnd(1))
	}
	t.Run("error-handler", func(t *testing.T) {
		errs := make(chan error, 1)
		server := &fasthttp.Server{
			GetOnly: true,
			Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("get") },
			ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
				errs <- err
				ctx.Response.Header.Add("Link", "</a.css>; rel=preload")
				if err := ctx.EarlyHints(); err != nil {
					t.Errorf("EarlyHints() error: %v", err)
				}
				ctx.Error("get only", fasthttp.StatusMethodNotAllowed)
			},
		}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if statuses := post(peer); !slices.Equal(statuses, []string{"103", "405"}) {
			t.Fatalf("statuses = %q, want [103 405]", statuses)
		}
		if err := <-errs; !errors.Is(err, fasthttp.ErrGetOnly) {
			t.Fatalf("ErrorHandler error = %v, want %v", err, fasthttp.ErrGetOnly)
		}
		peer.request(3, "/", true)
		if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
			t.Fatalf("GET after the refusal: statuses = %q", statuses)
		}
	})
	t.Run("default", func(t *testing.T) {
		testServer := newTestServer(t, &fasthttp.Server{
			GetOnly: true,
			Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("get") },
		}, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if statuses := post(peer); !slices.Equal(statuses, []string{"400"}) {
			t.Fatalf("statuses = %q, want [400]", statuses)
		}
	})
}

// A request body over MaxRequestBodySize, declared or discovered in flight
// before the handler starts, is answered as HTTP/1 answers it: through
// ErrorHandler with ErrBodyTooLarge (400 by default), the upload dropped. A
// handler already reading a streamed body gets the error from its read.
func TestTooLargeRequestBodyAnswersLikeHTTP1(t *testing.T) {
	const limit = 16
	for _, tc := range []struct {
		name     string
		declared bool
	}{
		{name: "declared", declared: true},
		{name: "in-flight"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := make(chan error, 1)
			server := &fasthttp.Server{
				MaxRequestBodySize: limit,
				Handler:            func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("handled") },
				ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
					errs <- err
					ctx.Error("too large", fasthttp.StatusRequestEntityTooLarge)
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			fields := [][2]string{
				{":method", fasthttp.MethodPost},
				{":scheme", "http"},
				{":authority", "example.com"},
				{":path", "/upload"},
			}
			if tc.declared {
				fields = append(fields, [2]string{"content-length", "100"})
			}
			peer.writeHeaders(1, false, fields...)
			if err := peer.framer.WriteData(1, false, make([]byte, 2*limit)); err != nil {
				t.Fatal(err)
			}
			var reset *xhttp2.RSTStreamFrame
			statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
				rst, ok := frame.(*xhttp2.RSTStreamFrame)
				if ok && rst.StreamID == 1 {
					reset = rst
				}
				return ok
			})
			if !slices.Equal(statuses, []string{"413"}) || reset.ErrCode != xhttp2.ErrCodeNo {
				t.Fatalf("statuses = %q, reset = %v; want [413] then RST_STREAM(NO_ERROR)", statuses, reset.ErrCode)
			}
			if err := <-errs; !errors.Is(err, fasthttp.ErrBodyTooLarge) {
				t.Fatalf("ErrorHandler error = %v, want %v", err, fasthttp.ErrBodyTooLarge)
			}
			peer.request(3, "/", true)
			if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("request after the refusal: statuses = %q", statuses)
			}
		})
	}
	t.Run("default-handler", func(t *testing.T) {
		testServer := newTestServer(t, &fasthttp.Server{
			MaxRequestBodySize: limit,
			Handler:            func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("handled") },
		}, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.writeHeaders(1, false,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
			[2]string{"content-length", "100"},
		)
		statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			headers, ok := frame.(*xhttp2.MetaHeadersFrame)
			return ok && headers.StreamID == 1
		})
		if !slices.Equal(statuses, []string{"400"}) {
			t.Fatalf("statuses = %q, want HTTP/1's default [400]", statuses)
		}
	})
	t.Run("streaming-handler", func(t *testing.T) {
		// A streamed body isn't capped: the handler reads it all, as in HTTP/1.
		type readResult struct {
			n   int
			err error
		}
		reads := make(chan readResult, 1)
		server := &fasthttp.Server{
			MaxRequestBodySize: limit,
			StreamRequestBody:  true,
			Handler: func(ctx *fasthttp.RequestCtx) {
				n, err := io.Copy(io.Discard, ctx.RequestBodyStream())
				reads <- readResult{int(n), err}
			},
			ErrorHandler: func(*fasthttp.RequestCtx, error) { t.Error("ErrorHandler ran for a streamed body") },
		}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.writeHeaders(1, false,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
		)
		if err := peer.framer.WriteData(1, true, make([]byte, 2*limit)); err != nil {
			t.Fatal(err)
		}
		if status, reset := streamOutcome(peer, 1); reset != nil || status != "200" {
			t.Fatalf("status = %q, reset = %v; want 200 with END_STREAM", status, reset)
		}
		select {
		case got := <-reads:
			if got.n != 2*limit || got.err != nil {
				t.Fatalf("handler read %d bytes, %v; want %d and no error", got.n, got.err, 2*limit)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("handler didn't finish")
		}
	})
}

// writePartialPost sends a POST declaring 10 body bytes and delivers 3.
func writePartialPost(peer *rawPeer, streamID uint32, extra ...[2]string) {
	peer.t.Helper()
	fields := make([][2]string, 0, 5+len(extra))
	fields = append(fields,
		[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
		[2]string{":authority", "example.com"}, [2]string{":path", "/upload"},
		[2]string{"content-length", "10"},
	)
	fields = append(fields, extra...)
	peer.writeHeaders(streamID, false, fields...)
	if err := peer.framer.WriteData(streamID, false, []byte("abc")); err != nil {
		peer.t.Fatal(err)
	}
}

// A request body that doesn't arrive within ReadTimeout is answered through
// the error path, as HTTP/1's serve loop answers a timed-out body read: the
// ErrorHandler sees a timeout *net.OpError, the default answers 408, the
// open request side is reset NO_ERROR after the response and the connection
// goes on; a handler that already reads a streamed body sees the error from
// its reads instead and answers itself (E12).
func TestRequestBodyReadTimeoutAnswersThroughErrorHandler(t *testing.T) {
	const readTimeout = 150 * time.Millisecond
	newServer := func(t *testing.T, errs chan error, maxConcurrentStreams uint32, configure func(*fasthttp.Server)) *testServer {
		t.Helper()
		server := &fasthttp.Server{
			ReadTimeout: readTimeout,
			IdleTimeout: 5 * time.Second,
			Handler:     func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("handled") },
		}
		if errs != nil {
			server.ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
				errs <- err
				ctx.Error("custom "+err.Error(), fasthttp.StatusRequestTimeout)
			}
		}
		if configure != nil {
			configure(server)
		}
		return newTestServer(t, server, ServerConfig{MaxConcurrentStreams: maxConcurrentStreams})
	}
	// requireTimeoutError checks the ErrorHandler saw a timeout *net.OpError;
	// HTTP/2's wraps ErrTimeout, HTTP/1's the connection's deadline error.
	requireTimeoutError := func(t *testing.T, errs chan error, http2 bool) {
		t.Helper()
		select {
		case err := <-errs:
			var opErr *net.OpError
			if !errors.As(err, &opErr) || !opErr.Timeout() || http2 && !errors.Is(err, fasthttp.ErrTimeout) {
				t.Fatalf("ErrorHandler error = %#v, want a timeout *net.OpError", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("ErrorHandler didn't run")
		}
	}
	// requireErrorResponse reads the 408 and the NO_ERROR reset of the still
	// open request side, then proves the connection still serves.
	requireErrorResponse := func(t *testing.T, peer *rawPeer, streamID uint32) {
		t.Helper()
		var reset *xhttp2.RSTStreamFrame
		statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			rst, ok := frame.(*xhttp2.RSTStreamFrame)
			if ok && rst.StreamID == streamID {
				reset = rst
			}
			return ok
		})
		if !slices.Equal(statuses, []string{"408"}) || reset.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("statuses = %q, reset = %v; want [408] then RST_STREAM(NO_ERROR)", statuses, reset.ErrCode)
		}
		peer.request(streamID+2, "/", true)
		if status, rst := streamOutcome(peer, streamID+2); rst != nil || status != "200" {
			t.Fatalf("request after the timeout: status %q, reset %v", status, rst)
		}
	}
	t.Run("buffered", func(t *testing.T) {
		errs := make(chan error, 2)
		testServer := newServer(t, errs, 0, nil)
		addr := testServer.listener.Addr().String()
		http1 := http1Exchange(t, addr, "POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Length: 10\r\n\r\nabc")
		if !strings.HasPrefix(http1, "HTTP/1.1 408") {
			t.Fatalf("HTTP/1 control: %q", http1)
		}
		requireTimeoutError(t, errs, false)
		peer := dialRawPeer(t, addr)
		writePartialPost(peer, 1)
		requireErrorResponse(t, peer, 1)
		requireTimeoutError(t, errs, true)
	})
	t.Run("default-handler", func(t *testing.T) {
		testServer := newServer(t, nil, 0, nil)
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		writePartialPost(peer, 1)
		var body bytes.Buffer
		statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			if data, ok := frame.(*xhttp2.DataFrame); ok && data.StreamID == 1 {
				body.Write(data.Data())
				return data.StreamEnded()
			}
			return false
		})
		if !slices.Equal(statuses, []string{"408"}) || body.String() != "Request timeout" {
			t.Fatalf("default error response = %q %q, want HTTP/1's 408 \"Request timeout\"", statuses, body.String())
		}
	})
	// blockFirstStream opens stream 1 on a handler that blocks until release
	// and has the peer reset it: closed on the wire, it keeps the one job
	// slot, so the next stream's job queues.
	blockFirstStream := func(t *testing.T, peer *rawPeer, started chan struct{}) {
		t.Helper()
		peer.request(1, "/block", true)
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("blocking handler didn't start")
		}
		if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("queued-expect", func(t *testing.T) {
		errs := make(chan error, 1)
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		testServer := newServer(t, errs, 1, func(s *fasthttp.Server) {
			s.Handler = func(ctx *fasthttp.RequestCtx) {
				if string(ctx.Path()) == "/block" {
					started <- struct{}{}
					<-release
				}
				ctx.SetBodyString("handled")
			}
		})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		blockFirstStream(t, peer, started)
		writeExpectPost(peer, 3, "/upload")
		time.Sleep(2 * readTimeout)
		close(release)
		requireErrorResponse(t, peer, 3)
		requireTimeoutError(t, errs, true)
	})
	t.Run("running-expect", func(t *testing.T) {
		errs := make(chan error, 1)
		release := make(chan struct{})
		testServer := newServer(t, errs, 0, func(s *fasthttp.Server) {
			s.ExpectHandler = func(*fasthttp.RequestCtx) int {
				<-release
				return fasthttp.StatusContinue
			}
		})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		writeExpectPost(peer, 1, "/upload")
		time.Sleep(2 * readTimeout)
		close(release)
		requireErrorResponse(t, peer, 1)
		requireTimeoutError(t, errs, true)
	})
	t.Run("pumping-error-response", func(t *testing.T) {
		// A body too large during the Expect job defers the refusal; the
		// read timer stays armed and fires while the 413's body stream pumps.
		errs := make(chan error, 1)
		release := make(chan struct{})
		bodyReader, bodyWriter := io.Pipe()
		t.Cleanup(func() { _ = bodyWriter.Close() })
		testServer := newServer(t, errs, 0, func(s *fasthttp.Server) {
			s.MaxRequestBodySize = 16
			s.ExpectHandler = func(*fasthttp.RequestCtx) int {
				<-release
				return fasthttp.StatusContinue
			}
			s.ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
				errs <- err
				ctx.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
				ctx.SetBodyStream(bodyReader, -1)
			}
		})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		writeExpectPost(peer, 1, "/upload")
		if err := peer.framer.WriteData(1, false, make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		close(release)
		statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			headers, ok := frame.(*xhttp2.MetaHeadersFrame)
			return ok && headers.StreamID == 1
		})
		if !slices.Equal(statuses, []string{"413"}) {
			t.Fatalf("statuses = %q, want [413]", statuses)
		}
		if err := <-errs; !errors.Is(err, fasthttp.ErrBodyTooLarge) {
			t.Fatalf("ErrorHandler error = %v, want ErrBodyTooLarge", err)
		}
		// The read timer fires now, with the response pumping.
		time.Sleep(2 * readTimeout)
		if counts := peer.collect(50 * time.Millisecond); len(counts) != 0 {
			t.Fatalf("frames while the error response pumped: %v", counts)
		}
		if _, err := io.WriteString(bodyWriter, "too large"); err != nil {
			t.Fatal(err)
		}
		_ = bodyWriter.Close()
		var body bytes.Buffer
		var reset *xhttp2.RSTStreamFrame
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			switch frame := frame.(type) {
			case *xhttp2.DataFrame:
				body.Write(frame.Data())
			case *xhttp2.RSTStreamFrame:
				reset = frame
				return true
			}
			return false
		})
		if body.String() != "too large" || reset.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("body %q, reset %v; want the whole error response then RST_STREAM(NO_ERROR)", body.String(), reset.ErrCode)
		}
		peer.request(3, "/", true)
		if status, rst := streamOutcome(peer, 3); rst != nil || status != "200" {
			t.Fatalf("request after the error response: status %q, reset %v", status, rst)
		}
	})
	t.Run("streaming-handler", func(t *testing.T) {
		readErrs := make(chan error, 1)
		testServer := newServer(t, nil, 0, func(s *fasthttp.Server) {
			s.StreamRequestBody = true
			s.Handler = func(ctx *fasthttp.RequestCtx) {
				_, err := io.ReadAll(ctx.RequestBodyStream())
				readErrs <- err
				ctx.Error("body: "+err.Error(), fasthttp.StatusBadRequest)
			}
		})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		writePartialPost(peer, 1)
		var reset *xhttp2.RSTStreamFrame
		statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			rst, ok := frame.(*xhttp2.RSTStreamFrame)
			if ok && rst.StreamID == 1 {
				reset = rst
			}
			return ok
		})
		if !slices.Equal(statuses, []string{"400"}) || reset.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("statuses = %q, reset = %v; want the handler's [400] then RST_STREAM(NO_ERROR)", statuses, reset.ErrCode)
		}
		if err := <-readErrs; !isTimeout(err) || !errors.Is(err, fasthttp.ErrTimeout) {
			t.Fatalf("body read error = %v, want a timeout", err)
		}
	})
	t.Run("queued-handler", func(t *testing.T) {
		readErrs := make(chan error, 1)
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		server := &fasthttp.Server{
			ReadTimeout:       readTimeout,
			IdleTimeout:       5 * time.Second,
			StreamRequestBody: true,
			Handler: func(ctx *fasthttp.RequestCtx) {
				if string(ctx.Path()) == "/block" {
					started <- struct{}{}
					<-release
					ctx.SetBodyString("handled")
					return
				}
				_, err := io.ReadAll(ctx.RequestBodyStream())
				readErrs <- err
				ctx.Error("body: "+err.Error(), fasthttp.StatusBadRequest)
			},
		}
		testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		blockFirstStream(t, peer, started)
		writePartialPost(peer, 3)
		time.Sleep(2 * readTimeout)
		close(release)
		if status, reset := streamOutcome(peer, 3); status != "400" {
			t.Fatalf("queued handler's response: status %q, reset %v; want 400", status, reset)
		}
		if err := <-readErrs; !isTimeout(err) {
			t.Fatalf("body read error = %v, want a timeout", err)
		}
	})
}

// A read timeout that fires while an error response is in progress changes
// nothing: the response stands, whether the error job still runs or its
// response is already pumping after JobDone.
func TestRequestReadTimeoutDuringErrorResponseIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*serverStream)
	}{
		{name: "running-error-job", setup: func(stream *serverStream) {
			stream.job, stream.jobState = jobError, jobStateRunning
		}},
		{name: "pumping-error-response", setup: func(stream *serverStream) {
			stream.job, stream.jobState = jobNone, jobStateFinished
			stream.handlerDone, stream.responsePumpStarted = true, true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, stream := stalledResponseConn(t, 0)
			conn.server = &fasthttp.Server{}
			stream.jobErr = fasthttp.ErrGetOnly
			stream.discardRequestBody = true
			stream.body = newRequestBody(nil, 0)
			tc.setup(stream)
			command := serverCommand{kind: serverCommandRequestReadTimeout, streamID: stream.id, err: fasthttp.ErrTimeout}
			if err := conn.processCommand(&command); err != nil {
				t.Fatalf("processCommand() error: %v", err)
			}
			if stream.isReset || !errors.Is(stream.jobErr, fasthttp.ErrGetOnly) || stream.refusedDuringJob || stream.body.isClosed {
				t.Fatalf("stream after the timeout = {reset:%v err:%v refused:%v bodyClosed:%v}, want unchanged",
					stream.isReset, stream.jobErr, stream.refusedDuringJob, stream.body.isClosed)
			}
		})
	}
}

// multipartFormBody is a well-formed form with one value and one file.
func multipartFormBody(t testing.TB) (contentType string, body []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("k", "v"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("f", "name.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("file content")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), buffer.Bytes()
}

func postMultipart(t testing.TB, client *stdhttp.Client, url, contentType string, body []byte) protocolResponse {
	t.Helper()
	resp, err := client.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Post(%s): %v", url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Post(%s): reading body: %v", url, err)
	}
	return protocolResponse{status: resp.StatusCode, contentLength: resp.Header.Get("Content-Length"), body: string(data)}
}

// A multipart form of known length is parsed before the handler under
// HTTP/1's condition: a malformed one is answered through the error path
// with the request reset, the handler never runs, and the connection goes on;
// DisablePreParseMultipartForm hands the raw body to the handler (E8).
func TestMultipartFormIsPreParsedLikeHTTP1(t *testing.T) {
	const malformedType = "multipart/form-data; boundary=abc"
	malformedBody := []byte("not a multipart body")
	contentType, formBody := multipartFormBody(t)
	type seen struct {
		formErr  error
		value    string
		streamed int64
		postBody int
	}
	t.Run("malformed", func(t *testing.T) {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("StreamRequestBody=%v", streaming), func(t *testing.T) {
				var handlerCalls atomic.Int32
				errs := make(chan error, 4)
				resets := make(chan bool, 4)
				server := &fasthttp.Server{
					StreamRequestBody: streaming,
					Handler:           func(*fasthttp.RequestCtx) { handlerCalls.Add(1) },
					ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
						errs <- err
						resets <- ctx.Request.Header.ContentLength() == 0 && len(ctx.Request.Header.ContentType()) == 0 &&
							len(ctx.Request.Body()) == 0
						ctx.Error("bad form", fasthttp.StatusBadRequest)
					},
				}
				testServer := newTestServer(t, server, ServerConfig{})
				for name, client := range map[string]*stdhttp.Client{"HTTP/1": http1Client(t), "HTTP/2": testServer.client} {
					got := postMultipart(t, client, testServer.URL("/"), malformedType, malformedBody)
					if got.status != 400 || got.body != "bad form" {
						t.Fatalf("%s: response = %+v, want 400 from ErrorHandler", name, got)
					}
					if err := <-errs; err == nil || !strings.Contains(err.Error(), "multipart") {
						t.Fatalf("%s: ErrorHandler error = %v, want a multipart error", name, err)
					}
					if reset := <-resets; !reset {
						t.Fatalf("%s: ErrorHandler saw the request, want it reset", name)
					}
				}
				if calls := handlerCalls.Load(); calls != 0 {
					t.Fatalf("handler ran %d times for malformed forms", calls)
				}
				if got := doProtocolRequest(t, testServer.client, stdhttp.MethodGet, testServer.URL("/"), nil); got.status != 200 {
					t.Fatalf("request after the refusal: %+v", got)
				}
			})
		}
	})
	t.Run("head-malformed", func(t *testing.T) {
		// The request is reset for the ErrorHandler; the response is still
		// framed for a HEAD: no DATA.
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("StreamRequestBody=%v", streaming), func(t *testing.T) {
				server := &fasthttp.Server{StreamRequestBody: streaming, Handler: func(*fasthttp.RequestCtx) { t.Error("handler ran") }}
				testServer := newTestServer(t, server, ServerConfig{})
				peer := dialRawPeer(t, testServer.listener.Addr().String())
				peer.writeHeaders(1, false,
					[2]string{":method", fasthttp.MethodHead}, [2]string{":scheme", "http"},
					[2]string{":authority", "example.com"}, [2]string{":path", "/"},
					[2]string{"content-type", malformedType}, [2]string{"content-length", strconv.Itoa(len(malformedBody))},
				)
				if err := peer.framer.WriteData(1, true, malformedBody); err != nil {
					t.Fatal(err)
				}
				var data []byte
				statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
					switch frame := frame.(type) {
					case *xhttp2.DataFrame:
						if frame.StreamID == 1 {
							data = append(data, frame.Data()...)
							return frame.StreamEnded()
						}
					case *xhttp2.MetaHeadersFrame:
						return frame.StreamID == 1 && frame.StreamEnded()
					}
					return false
				})
				if !slices.Equal(statuses, []string{"400"}) || len(data) != 0 {
					t.Fatalf("HEAD error response: statuses %q, DATA %q; want [400] without a body", statuses, data)
				}
			})
		}
	})
	t.Run("default-error-handler", func(t *testing.T) {
		testServer := newTestServer(t, &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}, ServerConfig{})
		want := postMultipart(t, http1Client(t), testServer.URL("/"), malformedType, malformedBody)
		got := postMultipart(t, testServer.client, testServer.URL("/"), malformedType, malformedBody)
		if want.status != 400 || got != want {
			t.Fatalf("HTTP/1 = %+v, HTTP/2 = %+v", want, got)
		}
	})
	t.Run("well-formed", func(t *testing.T) {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("StreamRequestBody=%v", streaming), func(t *testing.T) {
				results := make(chan seen, 1)
				server := &fasthttp.Server{StreamRequestBody: streaming, Handler: func(ctx *fasthttp.RequestCtx) {
					var result seen
					if body := ctx.RequestBodyStream(); body != nil {
						// Read before parsing: a pre-parsed body is spent.
						result.streamed, _ = io.Copy(io.Discard, body)
					}
					form, err := ctx.MultipartForm()
					result.formErr = err
					if err == nil {
						result.value = strings.Join(form.Value["k"], ",")
					}
					results <- result
					ctx.SetBodyString("ok")
				}}
				testServer := newTestServer(t, server, ServerConfig{})
				if got := postMultipart(t, testServer.client, testServer.URL("/"), contentType, formBody); got.status != 200 {
					t.Fatalf("response = %+v", got)
				}
				result := <-results
				if result.formErr != nil || result.value != "v" || result.streamed != 0 {
					t.Fatalf("handler saw %+v, want the parsed form and a spent body stream", result)
				}
			})
		}
	})
	t.Run("disabled", func(t *testing.T) {
		results := make(chan seen, 1)
		server := &fasthttp.Server{DisablePreParseMultipartForm: true, Handler: func(ctx *fasthttp.RequestCtx) {
			_, err := ctx.MultipartForm()
			results <- seen{formErr: err, postBody: len(ctx.PostBody())}
			ctx.SetBodyString("raw")
		}}
		testServer := newTestServer(t, server, ServerConfig{})
		if got := postMultipart(t, testServer.client, testServer.URL("/"), malformedType, malformedBody); got.status != 200 || got.body != "raw" {
			t.Fatalf("response = %+v, want the handler's", got)
		}
		if result := <-results; result.formErr == nil || result.postBody != len(malformedBody) {
			t.Fatalf("handler saw %+v, want the raw body and its own parse error", result)
		}
	})
}

// ExpectHandler runs on a worker: it may round-trip through the event loop
// (EarlyHints), and a slow one stalls only its own stream.
func TestExpectHandlerRunsOffTheEventLoop(t *testing.T) {
	hold := make(chan struct{})
	var holdOnce sync.Once
	server := &fasthttp.Server{
		ExpectHandler: func(ctx *fasthttp.RequestCtx) int {
			if string(ctx.Path()) == "/slow" {
				<-hold
			}
			ctx.Response.Header.Add("Link", "</a.css>; rel=preload")
			if err := ctx.EarlyHints(); err != nil {
				t.Errorf("EarlyHints() error: %v", err)
			}
			ctx.Response.Header.Del("Link")
			return fasthttp.StatusContinue
		},
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBody(ctx.Request.Body()) },
	}
	testServer := newTestServer(t, server, ServerConfig{})
	defer holdOnce.Do(func() { close(hold) })
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	writeExpectPost(peer, 1, "/slow")
	peer.request(3, "/", true)
	if statuses := peer.readHeaders(3, isStreamEnd(3)); !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("stream 3 statuses = %q while stream 1 waits for its ExpectHandler", statuses)
	}
	holdOnce.Do(func() { close(hold) })
	statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		return ok && headers.StreamID == 1 && headers.PseudoValue("status") == "100"
	})
	if err := peer.framer.WriteData(1, true, []byte("body")); err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, peer.readHeaders(1, isStreamEnd(1))...)
	if want := []string{"103", "100", "200"}; !slices.Equal(statuses, want) {
		t.Fatalf("stream 1 statuses = %q, want %q", statuses, want)
	}
}

// An upload staged while an Expect job is pending counts toward
// MaxBufferedRequestBodyPerConnection like a buffered body: a stream that
// would exceed the budget is reset, and the budget is free again once the
// request is served.
func TestExpectStagingRespectsBufferedBudget(t *testing.T) {
	hold := make(chan struct{})
	server := &fasthttp.Server{
		ExpectHandler: func(*fasthttp.RequestCtx) int {
			<-hold
			return fasthttp.StatusContinue
		},
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBody(ctx.Request.Body()) },
	}
	testServer := newTestServer(t, server, ServerConfig{MaxBufferedRequestBodyPerConnection: 8})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	body := []byte("12345678")
	stage := func(streamID uint32, ping byte) map[uint32]xhttp2.ErrCode {
		t.Helper()
		writeExpectPost(peer, streamID, "/")
		if err := peer.framer.WriteData(streamID, false, body); err != nil {
			t.Fatal(err)
		}
		if err := peer.framer.WritePing(false, [8]byte{ping}); err != nil {
			t.Fatal(err)
		}
		resets := map[uint32]xhttp2.ErrCode{}
		peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
				resets[rst.StreamID] = rst.ErrCode
			}
			return isPingAck(frame)
		})
		return resets
	}
	if resets := stage(1, 1); len(resets) != 0 {
		t.Fatalf("staging within the budget: resets = %v", resets)
	}
	if resets := stage(3, 3); resets[3] != xhttp2.ErrCodeEnhanceYourCalm || len(resets) != 1 {
		t.Fatalf("staging beyond the budget: resets = %v, want ENHANCE_YOUR_CALM on stream 3", resets)
	}
	close(hold)
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		return ok && headers.StreamID == 1 && headers.PseudoValue("status") == "100"
	})
	if err := peer.framer.WriteData(1, true, nil); err != nil {
		t.Fatal(err)
	}
	if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("stream 1 statuses = %q, want [200]", statuses)
	}
	// Stream 1 returned its budget.
	if resets := stage(5, 5); len(resets) != 0 {
		t.Fatalf("staging after the budget was returned: resets = %v", resets)
	}
}

// DATA and trailers that arrive while the Expect job runs reach the handler,
// buffered or streamed.
func TestUploadDuringExpectWaitReachesHandler(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "buffered"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			hold := make(chan struct{})
			got := make(chan string, 1)
			server := &fasthttp.Server{
				StreamRequestBody: streaming,
				ExpectHandler: func(*fasthttp.RequestCtx) int {
					<-hold
					return fasthttp.StatusContinue
				},
				Handler: func(ctx *fasthttp.RequestCtx) {
					var body []byte
					if streaming {
						var err error
						if body, err = io.ReadAll(ctx.RequestBodyStream()); err != nil {
							t.Errorf("reading the body: %v", err)
						}
					} else {
						body = ctx.Request.Body()
					}
					got <- string(body) + "|" + string(ctx.Request.Header.Peek("x-trailer"))
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			writeExpectPost(peer, 1, "/")
			for _, chunk := range []string{"hello ", "world"} {
				if err := peer.framer.WriteData(1, false, []byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			peer.writeHeaders(1, true, [2]string{"x-trailer", "yes"})
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			// The upload was processed before the job answers.
			peer.readHeaders(1, isPingAck)
			close(hold)
			select {
			case body := <-got:
				if body != "hello world|yes" {
					t.Fatalf("handler saw %q", body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handler didn't run")
			}
			// The upload is complete by the time the job accepts: no 100.
			if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("statuses = %q, want [200]", statuses)
			}
		})
	}
}

// A reset while the Expect job runs lets the job finish on its own; nothing
// starts for the stream afterwards.
func TestResetDuringExpectWaitIsClean(t *testing.T) {
	hold := make(chan struct{})
	var calls atomic.Int32
	server := &fasthttp.Server{
		ExpectHandler: func(ctx *fasthttp.RequestCtx) int {
			<-hold
			ctx.SetBodyString("touched after the reset")
			return fasthttp.StatusContinue
		},
		Handler: func(ctx *fasthttp.RequestCtx) {
			calls.Add(1)
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	writeExpectPost(peer, 1, "/")
	if err := peer.framer.WriteData(1, false, []byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{2}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isPingAck)
	close(hold)
	peer.request(3, "/", true)
	statuses := peer.readHeaders(3, func(frame xhttp2.Frame) bool {
		if frame.Header().StreamID == 1 {
			t.Fatalf("%v on the reset stream", frame)
		}
		return isStreamEnd(3)(frame)
	})
	if !slices.Equal(statuses, []string{"200"}) || calls.Load() != 1 {
		t.Fatalf("stream 3 statuses = %q, handler calls = %d", statuses, calls.Load())
	}
}

// A PUSH_PROMISE payload carries the promised stream ID besides its header
// block fragment; together they must fit the peer's SETTINGS_MAX_FRAME_SIZE.
func TestPushPromiseFitsPeerMaxFrameSize(t *testing.T) {
	pushed := make(chan error, 1)
	testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) != "/" {
			return
		}
		var header fasthttp.RequestHeader
		header.Set("X-Big", strings.Repeat("~", 2*defaultMaxFrameSize))
		pushed <- ctx.Push("/pushed", &fasthttp.PushOptions{Header: &header})
	}}, ServerConfig{EnablePush: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.framer.SetMaxReadFrameSize(defaultMaxFrameSize)
	// x/net's frame-order check doesn't expect CONTINUATION after PUSH_PROMISE,
	// and it can be lifted only without header decoding.
	peer.framer.ReadMetaHeaders = nil
	peer.framer.AllowIllegalReads = true
	peer.request(1, "/", true)
	if err := <-pushed; err != nil {
		t.Fatalf("Push() error: %v", err)
	}
	_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawPromise := false
	for {
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame() error: %v", err)
		}
		if _, ok := frame.(*xhttp2.PushPromiseFrame); ok {
			sawPromise = true
		}
		if headers, ok := frame.(*xhttp2.HeadersFrame); ok && headers.StreamID == 1 {
			break
		}
	}
	if !sawPromise {
		t.Fatal("no PUSH_PROMISE before the parent response")
	}
}

// A promised request can't declare content or carry connection-specific
// fields (RFC 9113 §8.4): Push rejects such PushOptions.Header fields before
// anything reaches the wire, whatever the key's case.
func TestPushRejectsPromisedRequestContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  func(*fasthttp.RequestHeader)
		promise bool
	}{
		{name: "content-length-1", header: func(h *fasthttp.RequestHeader) { h.SetContentLength(1) }},
		{name: "canonical-content-length", header: func(h *fasthttp.RequestHeader) { h.Set("Content-Length", "1") }},
		{name: "te-gzip", header: func(h *fasthttp.RequestHeader) { h.Set("te", "gzip") }},
		{name: "content-length-0", header: func(h *fasthttp.RequestHeader) { h.SetContentLength(0) }, promise: true},
		{name: "te-trailers", header: func(h *fasthttp.RequestHeader) { h.Set("te", "trailers") }, promise: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed := make(chan error, 1)
			testServer := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
				if string(ctx.Path()) != "/" {
					return
				}
				var header fasthttp.RequestHeader
				tc.header(&header)
				pushed <- ctx.Push("/pushed", &fasthttp.PushOptions{Header: &header})
			}}, ServerConfig{EnablePush: true})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			peer.framer.ReadMetaHeaders = nil
			peer.request(1, "/", true)
			promises := 0
			_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			for {
				frame, err := peer.framer.ReadFrame()
				if err != nil {
					t.Fatalf("ReadFrame() error: %v", err)
				}
				if _, ok := frame.(*xhttp2.PushPromiseFrame); ok {
					promises++
				}
				if headers, ok := frame.(*xhttp2.HeadersFrame); ok && headers.StreamID == 1 && headers.StreamEnded() {
					break
				}
			}
			err := <-pushed
			if tc.promise && (err != nil || promises != 1) {
				t.Fatalf("Push() error = %v, %d promises; want nil and 1", err, promises)
			}
			if !tc.promise && (err == nil || promises != 0) {
				t.Fatalf("Push() error = %v, %d promises; want an error and none", err, promises)
			}
		})
	}
}

// A client frame on a promised stream is a connection error of type
// PROTOCOL_ERROR while the stream is reserved (RFC 9113 §5.1) and a
// STREAM_CLOSED stream error once the push response headers went out.
func TestClientFramesOnPromisedStream(t *testing.T) {
	for _, tc := range []struct {
		name        string
		want        string
		headersSent bool
	}{
		{name: "reserved", want: "goaway_PROTOCOL_ERROR"},
		{name: "half-closed-remote", want: "rst_STREAM_CLOSED", headersSent: true},
	} {
		for _, kind := range []string{"headers", "data"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				release := make(chan struct{})
				pushReader, pushWriter := io.Pipe()
				server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
					if string(ctx.Path()) == "/" {
						if err := ctx.Push("/pushed", nil); err != nil {
							t.Errorf("Push() error: %v", err)
						}
						ctx.SetBodyString("parent")
						return
					}
					if tc.headersSent {
						ctx.SetBodyStream(pushReader, -1)
						return
					}
					<-release
				}}
				testServer := newTestServer(t, server, ServerConfig{EnablePush: true})
				t.Cleanup(func() {
					close(release)
					_ = pushWriter.Close()
				})
				peer := dialRawPeer(t, testServer.listener.Addr().String())
				peer.framer.ReadMetaHeaders = nil
				peer.request(1, "/", true)
				_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				promised, pushHeaders := false, false
				for !promised || tc.headersSent && !pushHeaders {
					frame, err := peer.framer.ReadFrame()
					if err != nil {
						t.Fatalf("ReadFrame() error: %v", err)
					}
					switch frame := frame.(type) {
					case *xhttp2.PushPromiseFrame:
						promised = true
					case *xhttp2.HeadersFrame:
						pushHeaders = pushHeaders || frame.StreamID == 2
					}
				}
				if kind == "headers" {
					peer.writeHeaders(2, true, [2]string{"x", "y"})
				} else if err := peer.framer.WriteData(2, true, []byte("x")); err != nil {
					t.Fatal(err)
				}
				counts, got := peer.waitForAny(2*time.Second,
					"rst_STREAM_CLOSED", "rst_PROTOCOL_ERROR", "goaway_PROTOCOL_ERROR", "goaway_STREAM_CLOSED", "closed")
				if got != tc.want {
					t.Fatalf("%s on the %s push stream: %s %v, want %s", kind, tc.name, got, counts, tc.want)
				}
			})
		}
	}
}

// A buffered response whose declared Content-Length disagrees with its body
// goes out with the body's length, as Response.Write rewrites it (E11), and
// the streams after it decode against an HPACK table that saw the block.
func TestContentLengthMismatchKeepsHPACKInSync(t *testing.T) {
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/bad" {
				ctx.Response.Header.Set("X-Bad", strings.Repeat("a", 16))
				ctx.Response.Header.SetContentLength(100)
				ctx.SetBodyString("abc")
				return
			}
			ctx.Response.Header.Set("X-Good", strings.Repeat("b", 16))
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/bad", true)
	var badFields []hpack.HeaderField
	var badBody bytes.Buffer
	statuses := peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID == 1 {
				badFields = frame.Fields
			}
		case *xhttp2.DataFrame:
			if frame.StreamID == 1 {
				badBody.Write(frame.Data())
				return frame.StreamEnded()
			}
		case *xhttp2.RSTStreamFrame:
			t.Fatalf("RST_STREAM(%v) for the declared-length mismatch", frame.ErrCode)
		}
		return false
	})
	if !slices.Equal(statuses, []string{"200"}) || badBody.String() != "abc" {
		t.Fatalf("mismatched response: statuses %q, body %q", statuses, badBody.String())
	}
	if !slices.ContainsFunc(badFields, func(f hpack.HeaderField) bool { return f.Name == "content-length" && f.Value == "3" }) {
		t.Fatalf("content-length wasn't rewritten to the body's length: %v", badFields)
	}
	for _, streamID := range []uint32{3, 5} {
		peer.request(streamID, "/good", true)
		var fields []hpack.HeaderField
		statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
				t.Fatalf("GOAWAY(%v) after the aborted response", goAway.ErrCode)
			}
			headers, ok := frame.(*xhttp2.MetaHeadersFrame)
			if ok && headers.StreamID == streamID {
				fields = headers.Fields
			}
			return ok && headers.StreamID == streamID
		})
		if !slices.Equal(statuses, []string{"200"}) {
			t.Fatalf("stream %d statuses = %q", streamID, statuses)
		}
		for _, field := range fields {
			if field.Name == "x-bad" || field.Name == "content-length" && field.Value != "2" {
				t.Fatalf("stream %d decoded a field of the aborted response: %v", streamID, fields)
			}
		}
	}
}

// http1Client is an HTTP/1 client for the listener the HTTP/2 client uses.
func http1Client(t testing.TB) *stdhttp.Client {
	t.Helper()
	transport := &stdhttp.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &stdhttp.Client{Transport: transport, Timeout: 5 * time.Second}
}

// protocolResponse is what a client saw of one response, for comparing the
// two protocols.
type protocolResponse struct {
	status        int
	contentLength string
	body          string
}

func doProtocolRequest(t testing.TB, client *stdhttp.Client, method, url string, header stdhttp.Header) protocolResponse {
	t.Helper()
	req, err := stdhttp.NewRequest(method, url, stdhttp.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, url, err)
	}
	return protocolResponse{status: resp.StatusCode, contentLength: resp.Header.Get("Content-Length"), body: string(body)}
}

// A buffered response's declared Content-Length is replaced by the body's,
// as Response.Write does for HTTP/1; a HEAD with an empty body keeps a
// declared length; CompressHandler's replacement body gets its own length.
func TestBufferedResponseContentLengthFollowsTheBody(t *testing.T) {
	body := strings.Repeat("x", 27)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/declared-empty":
			ctx.Response.Header.SetContentLength(42)
		case "/compressed":
			fasthttp.CompressHandler(func(ctx *fasthttp.RequestCtx) {
				ctx.Response.Header.SetContentLength(len(body))
				ctx.SetBodyString(body)
			})(ctx)
		default:
			ctx.Response.Header.SetContentLength(42)
			ctx.SetBodyString(body)
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	http1 := http1Client(t)
	gzip := stdhttp.Header{"Accept-Encoding": {"gzip"}}
	for _, tc := range []struct {
		name, method, path string
		header             stdhttp.Header
		want               protocolResponse
	}{
		{name: "get", method: stdhttp.MethodGet, path: "/", want: protocolResponse{200, "27", body}},
		{name: "head", method: stdhttp.MethodHead, path: "/", want: protocolResponse{200, "27", ""}},
		{name: "head-declared-empty", method: stdhttp.MethodHead, path: "/declared-empty", want: protocolResponse{200, "42", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doProtocolRequest(t, http1, tc.method, testServer.URL(tc.path), tc.header); got != tc.want {
				t.Fatalf("HTTP/1 control = %+v, want %+v", got, tc.want)
			}
			if got := doProtocolRequest(t, testServer.client, tc.method, testServer.URL(tc.path), tc.header); got != tc.want {
				t.Fatalf("HTTP/2 = %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("compress-handler", func(t *testing.T) {
		http1Response := doProtocolRequest(t, http1, stdhttp.MethodGet, testServer.URL("/compressed"), gzip)
		http2Response := doProtocolRequest(t, testServer.client, stdhttp.MethodGet, testServer.URL("/compressed"), gzip)
		if http1Response != http2Response || http2Response.status != 200 ||
			http2Response.contentLength != strconv.Itoa(len(http2Response.body)) {
			t.Fatalf("HTTP/1 = %+v, HTTP/2 = %+v", http1Response, http2Response)
		}
	})
}

// A body stream of unknown size that is an *io.LimitedReader declares its
// remaining count as the Content-Length, as Response.writeBodyStream does.
func TestLimitedReaderBodyStreamDeclaresItsLength(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetBodyStream(&io.LimitedReader{R: strings.NewReader("hello world"), N: 5}, -1)
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	http1 := http1Client(t)
	for _, tc := range []struct {
		method string
		want   protocolResponse
	}{
		{method: stdhttp.MethodGet, want: protocolResponse{200, "5", "hello"}},
		{method: stdhttp.MethodHead, want: protocolResponse{200, "5", ""}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			if got := doProtocolRequest(t, http1, tc.method, testServer.URL("/"), nil); got != tc.want {
				t.Fatalf("HTTP/1 control = %+v, want %+v", got, tc.want)
			}
			if got := doProtocolRequest(t, testServer.client, tc.method, testServer.URL("/"), nil); got != tc.want {
				t.Fatalf("HTTP/2 = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// contextCapturingHandler hands its ProtocolServerContext to the test and
// holds the connection until released.
type contextCapturingHandler struct {
	contexts chan *fasthttp.ProtocolServerContext
	release  chan struct{}
}

func (h *contextCapturingHandler) ServeConn(ctx *fasthttp.ProtocolServerContext, _ net.Conn) error {
	h.contexts <- ctx
	<-h.release
	return nil
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// A push that fails before the HPACK encoder is the caller's problem; one
// whose PUSH_PROMISE cannot be written after encoding ends the connection,
// since the peer's decoder would otherwise fall out of sync.
func TestPushPromiseWriteFailureFailsConnection(t *testing.T) {
	server := &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}
	handler := &contextCapturingHandler{
		contexts: make(chan *fasthttp.ProtocolServerContext, 1),
		release:  make(chan struct{}),
	}
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close() })
	served := make(chan error, 1)
	go func() { served <- server.ServeProtocolConn(serverSide, handler) }()
	t.Cleanup(func() {
		close(handler.release)
		if err := <-served; err != nil {
			t.Errorf("ServeProtocolConn() error: %v", err)
		}
	})
	protocolContext := <-handler.contexts
	config, err := normalizeServerConfig(server, &ServerConfig{EnablePush: true})
	if err != nil {
		t.Fatal(err)
	}
	conn := newServerConn(protocolContext, serverSide, &config)
	conn.framer = xhttp2.NewFramer(failingWriter{}, nil)
	parent := newServerStream(conn, 1)
	parent.request = protocolContext.AcquireRequestCtx(serverSide, parent)
	parent.request.Request.SetRequestURI("http://example.com/")
	conn.streams[1] = parent
	t.Cleanup(func() { protocolContext.ReleaseRequestCtx(parent.request) })

	err = conn.handlePush(parent, "/pushed", &fasthttp.PushOptions{Method: fasthttp.MethodPut})
	if _, fatal := errors.AsType[*serverError](err); err == nil || fatal {
		t.Fatalf("invalid push method error = %v, want a caller error", err)
	}
	result := make(chan error, 1)
	err = conn.processCommand(&serverCommand{kind: serverCommandPush, streamID: 1, target: "/pushed", result: result})
	if _, fatal := errors.AsType[*serverError](err); !fatal {
		t.Fatalf("push with a failing writer: processCommand() = %v, want a connection error", err)
	}
	if pushErr := <-result; pushErr == nil {
		t.Fatal("push with a failing writer reported no error to the caller")
	}
	if len(conn.streams) != 1 || conn.activePushes != 0 {
		t.Fatalf("failed push left %d streams and %d active pushes", len(conn.streams), conn.activePushes)
	}
}

// RFC 9113 §6.5.3, §6.9.2: until the client acknowledges SETTINGS it may send
// against the default 65535-byte stream window. A smaller configured window
// applies from the acknowledgement on, to open streams too.
func TestSmallStreamWindowAppliesOnceSettingsAreAcked(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	testServer := newTestServer(t, &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/hold" {
				<-release
				return
			}
			body, _ := io.ReadAll(ctx.RequestBodyStream())
			ctx.SetBody(body)
		},
	}, ServerConfig{MaxUploadBufferPerStream: 1024})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	post := func(id uint32, path string) {
		peer.writeHeaders(id, false,
			[2]string{":method", "POST"}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", path},
		)
	}
	// First flight, sent before the client processed the server's SETTINGS.
	post(1, "/echo")
	if err := peer.framer.WriteData(1, true, bytes.Repeat([]byte("a"), 4096)); err != nil {
		t.Fatal(err)
	}
	post(3, "/hold")
	if err := peer.framer.WriteData(3, false, bytes.Repeat([]byte("b"), 2048)); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{5}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(3, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.RSTStreamFrame:
			t.Fatalf("stream %d reset with %v", frame.StreamID, frame.ErrCode)
		case *xhttp2.GoAwayFrame:
			t.Fatalf("GOAWAY(%v)", frame.ErrCode)
		}
		return isPingAck(frame)
	})

	// Stream 3 has spent 2048 of a window that is now 1024.
	if err := peer.framer.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteData(3, false, []byte("c")); err != nil {
		t.Fatal(err)
	}
	counts, kind := peer.waitForAny(5*time.Second, "rst_FLOW_CONTROL_ERROR", "goaway_FLOW_CONTROL_ERROR")
	if kind != "rst_FLOW_CONTROL_ERROR" {
		t.Fatalf("DATA past the acknowledged window: %v", counts)
	}
}

// RFC 7541 §4.2, RFC 9113 §6.5.3: the client's encoder uses the default
// 4096-byte table until it acknowledges a smaller SETTINGS_HEADER_TABLE_SIZE,
// and from then on the smaller one, announced or not.
func TestSmallDecoderTableAppliesOnceSettingsAreAcked(t *testing.T) {
	value := func(c byte) string { return strings.Repeat(string(c), 100) }
	fields := [][2]string{
		{":method", "GET"},
		{":scheme", "http"},
		{":authority", "example.com"},
		{":path", "/"},
		{"x-a", value('a')},
		{"x-b", value('b')},
		{"x-c", value('c')},
	}
	for _, tc := range []struct {
		name string
		// update is the table size the client announces after the ACK, or 0.
		update uint32
		want   string
	}{
		{name: "announced", update: 256, want: "headers"},
		{name: "unannounced", want: "goaway_COMPRESSION_ERROR"},
		{name: "announced-too-large", update: defaultHeaderTableSize, want: "goaway_COMPRESSION_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testServer := newTestServer(t, &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBody(ctx.Request.Header.Peek("x-a")) },
			}, ServerConfig{MaxDecoderHeaderTableSize: 256})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			// The second block indexes entries a 256-byte table would have evicted.
			peer.writeHeaders(1, true, fields...)
			peer.writeHeaders(3, true, fields...)
			survived := map[uint32]bool{}
			peer.readHeaders(3, func(frame xhttp2.Frame) bool {
				switch frame := frame.(type) {
				case *xhttp2.MetaHeadersFrame:
					survived[frame.StreamID] = true
				case *xhttp2.GoAwayFrame:
					t.Fatalf("GOAWAY(%v) for first-flight requests", frame.ErrCode)
				}
				return survived[1] && survived[3]
			})

			if err := peer.framer.WriteSettingsAck(); err != nil {
				t.Fatal(err)
			}
			if tc.update != 0 {
				peer.encoder.SetMaxDynamicTableSize(tc.update)
			}
			peer.writeHeaders(5, true, fields...)
			if counts, kind := peer.waitForAny(5*time.Second, "goaway_COMPRESSION_ERROR", "headers"); kind != tc.want {
				t.Fatalf("request after the ACK: %v, want %s", counts, tc.want)
			}
		})
	}
}

// RFC 9113 §4.3.1: after acknowledging a table size below the capacity its
// encoder declared, the client must begin its next block with a size update,
// whether or not the block would decode without one.
func TestServerRequiresSizeUpdateAfterTableReduction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		announce bool
		want     string
	}{
		{name: "omitted", want: "goaway_COMPRESSION_ERROR"},
		{name: "announced", announce: true, want: "headers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testServer := newTestServer(t, &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
			}, ServerConfig{MaxDecoderHeaderTableSize: 256})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			peer.request(1, "/", true)
			if !peer.waitForStreamHeaders(1, 5*time.Second) {
				t.Fatal("first request failed")
			}
			if err := peer.framer.WriteSettingsAck(); err != nil {
				t.Fatal(err)
			}
			if tc.announce {
				peer.encoder.SetMaxDynamicTableSize(256)
			}
			// The block references only entries a 256-byte table still holds.
			peer.request(3, "/", true)
			if counts, kind := peer.waitForAny(5*time.Second, "headers", "goaway_COMPRESSION_ERROR"); kind != tc.want {
				t.Fatalf("request after the acknowledgement: %v, want %s", counts, tc.want)
			}
		})
	}
}

// A peer may still be sending when the final GOAWAY goes out (unknown frames
// here, WINDOW_UPDATEs in practice). Linux and macOS answer a close with
// unread input with RST, dropping the response bytes still queued, so the
// server half-closes and drains before it closes.
func TestGracefulCloseDrainsUnreadInput(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("RST on close with unread input is a Linux and macOS behavior, not %s", runtime.GOOS)
	}
	const size = 16 << 20
	release := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			<-release
			ctx.SetBody(bytes.Repeat([]byte("y"), size))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	conn, err := net.Dial("tcp", testServer.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var writeMu sync.Mutex
	framer := xhttp2.NewFramer(conn, conn)
	write := func(f func() error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return f()
	}
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error {
			return framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
		},
		func() error { return framer.WriteWindowUpdate(0, 1<<31-1-65535) },
		framer.WriteSettingsAck,
	} {
		if err := write(f); err != nil {
			t.Fatal(err)
		}
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/big"}} {
		if err := encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(func() error {
		return framer.WriteHeaders(xhttp2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true})
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// The server's SETTINGS prove it accepted the connection: Shutdown closes
	// the listener, and the kernel resets a connection still queued on it.
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if settings, ok := frame.(*xhttp2.SettingsFrame); ok && !settings.IsAck() {
			break
		}
	}
	go func() { _ = server.Shutdown() }()
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if ping, ok := frame.(*xhttp2.PingFrame); ok && !ping.IsAck() {
			data := ping.Data
			if err := write(func() error { return framer.WritePing(true, data) }); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	close(release)
	// Input the server never reads before it closes.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		junk := make([]byte, 512)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
			_ = write(func() error { return framer.WriteRawFrame(0xfe, 0, 0, junk) })
		}
	}()
	received, frames := 0, 0
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("connection failed after %d of %d body bytes: %v", received, size, err)
		}
		frames++
		if frames%64 == 0 {
			// A consumer slower than loopback.
			time.Sleep(time.Millisecond)
		}
		if data, ok := frame.(*xhttp2.DataFrame); ok && data.StreamID == 1 {
			received += len(data.Data())
			if data.StreamEnded() {
				break
			}
		}
	}
	if received != size {
		t.Fatalf("received %d of %d body bytes", received, size)
	}
}

// A malformed frame earns a GOAWAY(PROTOCOL_ERROR) over a transport that
// still works, so the input the peer keeps sending is drained before Close
// as after a graceful end: otherwise the RST drops the GOAWAY and the
// response bytes queued before it.
func TestProtocolErrorGoAwayIsDrainedBeforeClose(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("RST on close with unread input is a Linux and macOS behavior, not %s", runtime.GOOS)
	}
	const size = 16 << 20
	release := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			<-release
			ctx.SetBody(bytes.Repeat([]byte("y"), size))
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	conn, err := net.Dial("tcp", testServer.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var writeMu sync.Mutex
	framer := xhttp2.NewFramer(conn, conn)
	write := func(f func() error) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return f()
	}
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error {
			return framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1})
		},
		func() error { return framer.WriteWindowUpdate(0, 1<<31-1-65535) },
		framer.WriteSettingsAck,
	} {
		if err := write(f); err != nil {
			t.Fatal(err)
		}
	}
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/big"}} {
		if err := encoder.WriteField(hpack.HeaderField{Name: field[0], Value: field[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := write(func() error {
		return framer.WriteHeaders(xhttp2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: true, EndHeaders: true})
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// Once the response flows, a DATA frame on stream 0 fails the connection
	// at the framer, followed by input the server never reads.
	received := 0
	for received == 0 {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if data, ok := frame.(*xhttp2.DataFrame); ok && data.StreamID == 1 {
			received += len(data.Data())
		}
	}
	if err := write(func() error { return framer.WriteRawFrame(xhttp2.FrameData, 0, 0, []byte("x")) }); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		junk := make([]byte, 512)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
			_ = write(func() error { return framer.WriteRawFrame(0xfe, 0, 0, junk) })
		}
	}()
	frames := 0
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			t.Fatalf("connection failed after %d body bytes without a GOAWAY: %v", received, err)
		}
		frames++
		if frames%64 == 0 {
			// A consumer slower than loopback.
			time.Sleep(time.Millisecond)
		}
		switch frame := frame.(type) {
		case *xhttp2.DataFrame:
			if frame.StreamID == 1 {
				received += len(frame.Data())
			}
		case *xhttp2.GoAwayFrame:
			if frame.ErrCode != xhttp2.ErrCodeProtocol {
				t.Fatalf("GOAWAY(%v), want PROTOCOL_ERROR", frame.ErrCode)
			}
			return
		}
	}
}

// Graceful shutdown announces GOAWAY(2^31-1), waits for its PING, then names
// the last stream it processed. A connection that goes idle in between stays
// open for it: requests the client sent meanwhile are still served.
func TestShutdownWaitsForTheFinalGoAway(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/slow" {
				close(started)
				<-release
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/slow", true)
	<-started
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- server.Shutdown() }()

	var shutdownPing [8]byte
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		if ping, ok := frame.(*xhttp2.PingFrame); ok && !ping.IsAck() {
			shutdownPing = ping.Data
			return true
		}
		return false
	})
	close(release)
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		data, ok := frame.(*xhttp2.DataFrame)
		return ok && data.StreamID == 1 && data.StreamEnded()
	})
	// Shutdown's idle sweep runs every 100ms; the connection must outlive it
	// while the PING is outstanding.
	deadline := time.Now().Add(500 * time.Millisecond)
	_ = peer.conn.SetReadDeadline(deadline)
	for {
		frame, err := peer.framer.ReadFrame()
		if isTimeout(err) {
			break
		}
		if err != nil {
			t.Fatalf("connection closed before the shutdown PING was answered: %v", err)
		}
		if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
			t.Fatalf("GOAWAY(%d, %v) before the shutdown PING was answered", goAway.LastStreamID, goAway.ErrCode)
		}
	}
	peer.request(3, "/", true)
	if err := peer.framer.WritePing(true, shutdownPing); err != nil {
		t.Fatal(err)
	}
	var lastStreamID uint32
	statuses := peer.readHeaders(3, func(frame xhttp2.Frame) bool {
		if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
			lastStreamID = goAway.LastStreamID
		}
		data, ok := frame.(*xhttp2.DataFrame)
		return lastStreamID != 0 && ok && data.StreamID == 3 && data.StreamEnded()
	})
	if lastStreamID != 3 || !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("final GOAWAY last stream = %d, stream 3 statuses = %q; want 3 and [200]", lastStreamID, statuses)
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown() didn't return")
	}
}

// Connection-close intent ends the connection's use for requests as HTTP/1's
// close does: Server.DisableKeepalive, ctx.SetConnectionClose and a raw
// Connection: close header each start the graceful drain after the response,
// so the next request lands on a new connection (E5).
func TestConnectionCloseIntentDrainsTheConnection(t *testing.T) {
	triggers := []struct {
		name    string
		server  func(*fasthttp.Server)
		handler func(*fasthttp.RequestCtx)
	}{
		{name: "disable-keepalive", server: func(s *fasthttp.Server) { s.DisableKeepalive = true }},
		{name: "set-connection-close", handler: func(ctx *fasthttp.RequestCtx) { ctx.SetConnectionClose() }},
		{name: "raw-header", handler: func(ctx *fasthttp.RequestCtx) { ctx.Response.Header.Set("Connection", "close") }},
	}
	for _, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
				if tc.handler != nil {
					tc.handler(ctx)
				}
				fmt.Fprintf(ctx, "%d", ctx.ConnRequestNum())
			}}
			if tc.server != nil {
				tc.server(server)
			}
			testServer := newTestServer(t, server, ServerConfig{})
			for name, client := range map[string]*stdhttp.Client{"HTTP/1": http1Client(t), "HTTP/2": testServer.client} {
				for range 2 {
					got := doProtocolRequest(t, client, stdhttp.MethodGet, testServer.URL("/"), nil)
					if got.status != 200 || got.body != "1" {
						t.Fatalf("%s: response = %+v, want request 1 of a connection", name, got)
					}
				}
			}
			if dials := testServer.dials.Load(); dials != 2 {
				t.Fatalf("HTTP/2 client made %d connections, want 2", dials)
			}
		})
	}
	t.Run("wire", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/slow" {
				close(started)
				<-release
			} else {
				ctx.SetConnectionClose()
			}
			ctx.SetBodyString("ok")
		}}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/slow", true)
		<-started
		peer.request(3, "/", true)
		var fields []hpack.HeaderField
		var goAway *xhttp2.GoAwayFrame
		var ping [8]byte
		peer.readHeaders(3, func(frame xhttp2.Frame) bool {
			switch frame := frame.(type) {
			case *xhttp2.MetaHeadersFrame:
				if frame.StreamID == 3 {
					fields = frame.Fields
				}
			case *xhttp2.GoAwayFrame:
				goAway = frame
			case *xhttp2.PingFrame:
				ping = frame.Data
				return !frame.IsAck()
			}
			return false
		})
		if fields == nil || goAway == nil || goAway.LastStreamID != math.MaxInt32 || goAway.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("response fields %v, GOAWAY %v; want the response and GOAWAY(2^31-1, NO_ERROR) before the PING", fields, goAway)
		}
		for _, field := range fields {
			if field.Name == "connection" {
				t.Fatalf("connection field on the wire: %v", fields)
			}
		}
		// Until the PING is answered new streams are still accepted.
		peer.request(5, "/", true)
		if status, reset := streamOutcome(peer, 5); reset != nil || status != "200" {
			t.Fatalf("stream opened during the drain: status %q, reset %v", status, reset)
		}
		close(release)
		if status, reset := streamOutcome(peer, 1); reset != nil || status != "200" {
			t.Fatalf("stream in flight at the drain: status %q, reset %v", status, reset)
		}
		if err := peer.framer.WritePing(true, ping); err != nil {
			t.Fatal(err)
		}
		_ = peer.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatalf("after the PING ACK: %v, want the final GOAWAY", err)
		}
		if final, ok := frame.(*xhttp2.GoAwayFrame); !ok || final.LastStreamID != 5 {
			t.Fatalf("after the PING ACK: %v, want GOAWAY(5)", frame)
		}
		if _, err := peer.framer.ReadFrame(); err == nil {
			t.Fatal("connection stayed open after the final GOAWAY")
		}
	})
	t.Run("no-ping-ack", func(t *testing.T) {
		const pingTimeout = 100 * time.Millisecond
		server := &fasthttp.Server{DisableKeepalive: true, Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }}
		testServer := newTestServer(t, server, ServerConfig{PingTimeout: pingTimeout})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/", true)
		start := time.Now()
		var goAways []uint32
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
				goAways = append(goAways, goAway.LastStreamID)
				return goAway.LastStreamID == 1
			}
			return false
		})
		if !slices.Equal(goAways, []uint32{math.MaxInt32, 1}) {
			t.Fatalf("GOAWAY last stream IDs = %v, want [2^31-1 1]", goAways)
		}
		if elapsed := time.Since(start); elapsed < pingTimeout || elapsed > 10*pingTimeout {
			t.Fatalf("final GOAWAY after %v, want about PingTimeout %v", elapsed, pingTimeout)
		}
		_ = peer.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := peer.framer.ReadFrame(); err == nil {
			t.Fatal("connection stayed open after the final GOAWAY")
		}
	})
}

// Shutdown must end a connection in every phase, without waiting on a peer
// that stays silent past PingTimeout.
func TestShutdownEndsConnectionsInEveryPhase(t *testing.T) {
	shutdown := func(t *testing.T, server *fasthttp.Server) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.ShutdownWithContext(ctx); err != nil {
			t.Fatalf("ShutdownWithContext() error: %v", err)
		}
	}
	t.Run("awaiting-preface", func(t *testing.T) {
		server := &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}
		listener := fasthttputil.NewInmemoryListener()
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		// Dial returns once Serve accepts, so Serve has registered the
		// listener Shutdown closes.
		probe, err := listener.Dial()
		if err != nil {
			t.Fatal(err)
		}
		probe.Close()
		serverSide, clientSide := net.Pipe()
		defer clientSide.Close()
		served := make(chan error, 1)
		go func() { served <- ServeConn(server, serverSide, ServerConfig{}) }()
		// net.Pipe hands the byte over only once the server reads the preface.
		if _, err := clientSide.Write([]byte(clientPreface[:1])); err != nil {
			t.Fatal(err)
		}
		shutdown(t, server)
		if err := <-served; err != nil {
			t.Fatalf("ServeConn() error: %v", err)
		}
		if err := <-serveDone; err != nil {
			t.Fatalf("Serve() error: %v", err)
		}
	})
	for _, phase := range []string{"awaiting-settings", "idle", "silent-peer"} {
		t.Run(phase, func(t *testing.T) {
			server := &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}}
			// An answered PING ends the idle connection well before the default.
			config := ServerConfig{}
			if phase != "idle" {
				config.PingTimeout = 50 * time.Millisecond
			}
			testServer := newTestServer(t, server, config)
			conn, err := net.Dial("tcp", testServer.listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			framer := xhttp2.NewFramer(conn, conn)
			if _, err := io.WriteString(conn, clientPreface); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if phase != "awaiting-settings" {
				if err := framer.WriteSettings(); err != nil {
					t.Fatal(err)
				}
				if err := framer.WritePing(false, [8]byte{6}); err != nil {
					t.Fatal(err)
				}
				for {
					frame, err := framer.ReadFrame()
					if err != nil {
						t.Fatal(err)
					}
					if isPingAck(frame) {
						break
					}
				}
			}
			go func() {
				for {
					frame, err := framer.ReadFrame()
					if err != nil {
						return
					}
					if ping, ok := frame.(*xhttp2.PingFrame); ok && !ping.IsAck() && phase == "idle" {
						_ = framer.WritePing(true, ping.Data)
					}
				}
			}()
			shutdown(t, server)
		})
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

func TestRepeatedHeaderTableSizeSettingsSignalTheSmallest(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("X-Indexed", "value")
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.framer.ReadMetaHeaders = nil
	_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	firstBlock := func(streamID uint32) []byte {
		for {
			frame, err := peer.framer.ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame() error: %v", err)
			}
			if headers, ok := frame.(*xhttp2.HeadersFrame); ok && headers.StreamID == streamID {
				return headers.HeaderBlockFragment()
			}
		}
	}
	peer.request(1, "/", true)
	firstBlock(1)
	// Zero then the default clears the peer's table; the encoder must say so.
	if err := peer.framer.WriteSettings(
		xhttp2.Setting{ID: xhttp2.SettingHeaderTableSize, Val: 0},
		xhttp2.Setting{ID: xhttp2.SettingHeaderTableSize, Val: defaultHeaderTableSize},
	); err != nil {
		t.Fatalf("WriteSettings() error: %v", err)
	}
	peer.request(3, "/", true)
	if block := firstBlock(3); !bytes.HasPrefix(block, []byte{0x20, 0x3f, 0xe1, 0x1f}) {
		t.Fatalf("header block = %x, want size updates to 0 then %d first", block, defaultHeaderTableSize)
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
			InsecureSkipVerify: true,
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

// recordingLogger keeps what the server logs.
type recordingLogger struct {
	mu   sync.Mutex
	logs strings.Builder
}

func (l *recordingLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.logs, format+"\n", args...)
}

func (l *recordingLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.String()
}

// A TLS 1.2 connection with a cipher suite RFC 9113 appendix A forbids gets
// GOAWAY(INADEQUATE_SECURITY) before the close (§9.2.2); the server still
// logs the rejection.
func TestInadequateTLSIsAnnouncedWithGoAway(t *testing.T) {
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
	logger := &recordingLogger{}
	server := &fasthttp.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Logger:    logger,
		Handler:   func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
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
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites:       []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA},
		NextProtos:         []string{"h2"},
	})
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer conn.Close()
	if proto := conn.ConnectionState().NegotiatedProtocol; proto != "h2" {
		t.Fatalf("negotiated %q, want h2", proto)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	framer := xhttp2.NewFramer(nil, conn)
	frame, err := framer.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame() error: %v, want GOAWAY", err)
	}
	goAway, ok := frame.(*xhttp2.GoAwayFrame)
	if !ok || goAway.ErrCode != xhttp2.ErrCodeInadequateSecurity {
		t.Fatalf("first frame = %v, want GOAWAY(INADEQUATE_SECURITY)", frame)
	}
	if frame, err := framer.ReadFrame(); err == nil {
		t.Fatalf("frame after the GOAWAY: %v", frame)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logger.String(), errInadequateTLS.Error()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if logs := logger.String(); !strings.Contains(logs, errInadequateTLS.Error()) {
		t.Fatalf("rejection wasn't logged: %q", logs)
	}
}

// :scheme reaches the handler's URI as HTTP/1's absolute-form does, and a
// push from such a request keeps its origin.
func TestRequestSchemeComesFromThePseudoHeader(t *testing.T) {
	schemes := make(chan string, 2)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		schemes <- string(ctx.URI().Scheme())
		if string(ctx.Path()) == "/" {
			if err := ctx.Push("/pushed", nil); err != nil {
				t.Errorf("Push() error: %v", err)
			}
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{EnablePush: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	// PUSH_PROMISE blocks aren't decoded by x/net's meta-header reader.
	peer.framer.ReadMetaHeaders = nil
	peer.writeHeaders(1, true,
		[2]string{":method", "GET"}, [2]string{":scheme", "https"},
		[2]string{":authority", "example.com"}, [2]string{":path", "/"},
	)
	var promised uint32
	peer.readHeaders(1, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.PushPromiseFrame:
			promised = frame.PromiseID
		case *xhttp2.HeadersFrame:
			return frame.StreamID == 1 && frame.StreamEnded()
		}
		return false
	})
	if promised == 0 {
		t.Fatal("no PUSH_PROMISE before the parent response")
	}
	for range 2 {
		select {
		case scheme := <-schemes:
			if scheme != "https" {
				t.Fatalf("handler URI scheme = %q, want the wire :scheme https", scheme)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("handler didn't run")
		}
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

// The default Content-Type follows ResponseHeader.AppendBytes' condition, as
// HTTP/1 writes it: a body of nonzero or unknown length on a status that
// allows one. An explicitly set value is always written, also when it equals
// the default.
func TestDefaultContentTypeFollowsTheBody(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		switch string(ctx.Path()) {
		case "/empty":
		case "/redirect":
			ctx.Redirect("/body", fasthttp.StatusFound)
		case "/204":
			ctx.SetStatusCode(fasthttp.StatusNoContent)
		case "/304":
			ctx.SetStatusCode(fasthttp.StatusNotModified)
		case "/empty-stream":
			ctx.SetBodyStream(strings.NewReader(""), 0)
		case "/body":
			ctx.SetBodyString("body")
		case "/stream":
			ctx.SetBodyStream(strings.NewReader("body"), -1)
		case "/explicit":
			ctx.SetContentType("application/json")
		case "/explicit-default":
			ctx.SetContentType("text/plain; charset=utf-8")
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	noRedirects := func(*stdhttp.Request, []*stdhttp.Request) error { return stdhttp.ErrUseLastResponse }
	http1 := http1Client(t)
	http1.CheckRedirect = noRedirects
	http2 := &stdhttp.Client{Transport: testServer.transport, CheckRedirect: noRedirects}
	for _, tc := range []struct {
		method, path, want string
	}{
		{stdhttp.MethodGet, "/empty", ""},
		{stdhttp.MethodGet, "/redirect", ""},
		{stdhttp.MethodGet, "/204", ""},
		{stdhttp.MethodGet, "/304", ""},
		{stdhttp.MethodGet, "/empty-stream", ""},
		{stdhttp.MethodGet, "/body", "text/plain; charset=utf-8"},
		{stdhttp.MethodHead, "/body", "text/plain; charset=utf-8"},
		{stdhttp.MethodGet, "/stream", "text/plain; charset=utf-8"},
		{stdhttp.MethodGet, "/explicit", "application/json"},
		{stdhttp.MethodGet, "/explicit-default", "text/plain; charset=utf-8"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			for _, client := range []struct {
				name   string
				client *stdhttp.Client
			}{{"http1", http1}, {"http2", http2}} {
				req, err := stdhttp.NewRequest(tc.method, testServer.URL(tc.path), stdhttp.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := client.client.Do(req)
				if err != nil {
					t.Fatalf("%s: %v", client.name, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				values := resp.Header.Values("Content-Type")
				switch {
				case tc.want == "" && len(values) != 0:
					t.Errorf("%s: Content-Type = %q, want none", client.name, values)
				case tc.want != "" && !slices.Equal(values, []string{tc.want}):
					t.Errorf("%s: Content-Type = %q, want [%q]", client.name, values, tc.want)
				}
			}
		})
	}
}

// stalledResponseConn returns a connection whose only stream owes data it
// cannot send for lack of credit, its response deadline armed as the
// handler's return arms it.
func stalledResponseConn(t *testing.T, writeTimeout time.Duration) (*serverConn, *serverStream) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(context.Canceled) })
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
		},
		ctx:           ctx,
		commands:      make(chan serverCommand, 1),
		framer:        xhttp2.NewFramer(io.Discard, nil),
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{peerMaxFrameSize: defaultMaxFrameSize},
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.writeTimeout = writeTimeout
	stream.pendingData = []byte("blocked")
	conn.streams[stream.id] = stream
	conn.armResponseWriteTimeout(stream)
	conn.queueFlush(stream)
	return conn, stream
}

// Flush passes that make no progress don't postpone the response deadline.
func TestResponseWriteTimeoutIsNotPostponedByFlushes(t *testing.T) {
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
			t.Fatalf("processing the response deadline: %v", err)
		}
	default:
		t.Fatal("repeated flushes postponed the response deadline")
	}
	if !stream.isReset {
		t.Fatal("stalled stream wasn't reset")
	}
}

// Progress doesn't disarm the response deadline: the response has to be
// queued whole. A timeout of another generation is ignored, and so is one
// that arrives after the response completed.
func TestResponseWriteTimeoutOutlivesProgress(t *testing.T) {
	conn, stream := stalledResponseConn(t, time.Hour)
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if stream.writeTimer == nil {
		t.Fatal("response deadline wasn't armed")
	}
	conn.send.window = 1 << 20
	stream.send.window = 1 << 20
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if len(stream.pendingData) != 0 || stream.writeTimer == nil {
		t.Fatalf("after progress: pending=%d timer=%v, want drained and still armed", len(stream.pendingData), stream.writeTimer != nil)
	}
	stale := serverCommand{kind: serverCommandResponseWriteTimeout, streamID: 1, generation: stream.writeTimeoutGen + 1, err: errWriteTimeout}
	if err := conn.processCommand(&stale); err != nil {
		t.Fatalf("processing stale timeout: %v", err)
	}
	if stream.isReset {
		t.Fatal("a timeout of another generation reset the stream")
	}
	current := serverCommand{kind: serverCommandResponseWriteTimeout, streamID: 1, generation: stream.writeTimeoutGen, err: errWriteTimeout}
	if err := conn.processCommand(&current); err != nil {
		t.Fatalf("processing current timeout: %v", err)
	}
	if !stream.isReset {
		t.Fatal("the response deadline didn't reset the incomplete response")
	}

	conn, stream = stalledResponseConn(t, time.Hour)
	stream.responseEOF = true
	conn.send.window = 1 << 20
	stream.send.window = 1 << 20
	if err := conn.flushResponses(); err != nil {
		t.Fatalf("flushResponses() error: %v", err)
	}
	if !stream.localClosed || stream.writeTimer != nil {
		t.Fatalf("completed response: localClosed=%v timer=%v, want closed and disarmed", stream.localClosed, stream.writeTimer != nil)
	}
	late := serverCommand{kind: serverCommandResponseWriteTimeout, streamID: 1, generation: stream.writeTimeoutGen, err: errWriteTimeout}
	if err := conn.processCommand(&late); err != nil {
		t.Fatalf("processing late timeout: %v", err)
	}
	if stream.isReset {
		t.Fatal("a timeout after the response completed reset the stream")
	}
}

func TestAcceptedStreamWriteFailsWhenOwnerStops(t *testing.T) {
	conn := &serverConn{
		commands:  make(chan serverCommand, 4),
		ownerDone: make(chan struct{}),
		streams:   make(map[uint32]*serverStream),
	}
	stream := &serverStream{id: 1, conn: conn}
	conn.streams[stream.id] = stream
	streamConn := newStreamConn(conn, stream)
	result := make(chan error, 1)
	go func() {
		_, err := streamConn.Write([]byte("payload"))
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(conn.commands) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(conn.commands) == 0 {
		t.Fatal("write command wasn't queued")
	}
	conn.failPendingStreams(io.ErrUnexpectedEOF)
	select {
	case err := <-result:
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("Write() error = %v, want %v", err, io.ErrUnexpectedEOF)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted write outlived the connection owner")
	}
}

func TestAcceptedStreamWriteReportsFramedBytesWhenOwnerStops(t *testing.T) {
	conn := &serverConn{
		config: serverConfig{
			maxConcurrentStreams:    1,
			maxRapidResetsPerSecond: 100,
		},
		commands:      make(chan serverCommand, 4),
		ownerDone:     make(chan struct{}),
		framer:        xhttp2.NewFramer(io.Discard, nil),
		streams:       make(map[uint32]*serverStream),
		connFlowState: connFlowState{send: sendWindow{window: 2}, peerMaxFrameSize: defaultMaxFrameSize},
	}
	stream := newServerStream(conn, 1)
	stream.handlerStarted = true
	stream.send.window = 2
	conn.streams[stream.id] = stream
	streamConn := newStreamConn(conn, stream)
	type writeResult struct {
		n   int
		err error
	}
	result := make(chan writeResult, 1)
	go func() {
		n, err := streamConn.Write([]byte("four"))
		result <- writeResult{n, err}
	}()
	command := <-conn.commands
	if err := conn.processCommand(&command); err != nil {
		t.Fatalf("queueing stream write: %v", err)
	}
	if more, err := conn.flushStream(stream, false); err != nil || !more {
		t.Fatalf("partial flush = %v, %v; want more to send", more, err)
	}
	conn.failPendingStreams(io.ErrUnexpectedEOF)
	select {
	case got := <-result:
		if got.n != 2 || !errors.Is(got.err, io.ErrUnexpectedEOF) {
			t.Fatalf("Write() = %d, %v; want 2, %v", got.n, got.err, io.ErrUnexpectedEOF)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted write outlived the connection owner")
	}
}

// A write deadline set after a stream write blocked on flow control must
// still end the write.
func TestExtendedConnectWriteDeadlineSetWhileBlocked(t *testing.T) {
	result := make(chan error, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
				go func() {
					time.Sleep(50 * time.Millisecond)
					_ = stream.SetWriteDeadline(time.Now())
				}()
				_, err := stream.Write([]byte("blocked"))
				result <- err
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
	case err := <-result:
		if !isTimeout(err) {
			t.Fatalf("Write() error = %v, want timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked stream write ignored a deadline set after it began")
	}
}

// A server StreamConn closed past its write deadline resets the stream every
// time: an END_STREAM can't be sent past the deadline, and the expired cut
// precedes the handoff rather than racing it.
func TestStreamConnCloseWithExpiredWriteDeadlineResets(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_ = conn.SetWriteDeadline(time.Now().Add(-time.Hour))
			_ = conn.Close()
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxConcurrentStreams: 1000})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	const rounds = 200
	ended := 0
	for i := range rounds {
		streamID := uint32(2*i + 1)
		openRawTunnel(peer, streamID)
		peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			switch frame := frame.(type) {
			case *xhttp2.DataFrame:
				if frame.StreamID == streamID && frame.StreamEnded() {
					ended++
					return true
				}
			case *xhttp2.RSTStreamFrame:
				return frame.StreamID == streamID
			}
			return false
		})
	}
	if ended != 0 {
		t.Fatalf("Close() past the write deadline sent END_STREAM on %d of %d streams", ended, rounds)
	}
}

// Reads and writes racing a Close that resets the stream fail with
// net.ErrClosed, never with the stream's internal closed error.
func TestStreamConnOperationsRacingCloseGetErrClosed(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	bad := 0
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			readResult := make(chan error, 1)
			writeResult := make(chan error, 1)
			go func() {
				_, err := conn.Read(make([]byte, 1))
				readResult <- err
			}()
			go func() {
				// No send window: an accepted write stalls on flow control.
				_, err := conn.Write([]byte("payload"))
				writeResult <- err
			}()
			// Both block before the Close that resets the stream.
			time.Sleep(2 * time.Millisecond)
			_ = conn.Close()
			for _, err := range []error{<-readResult, <-writeResult} {
				mu.Lock()
				counts[fmt.Sprint(err)]++
				if err != nil && !errors.Is(err, net.ErrClosed) {
					bad++
				}
				mu.Unlock()
			}
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, MaxConcurrentStreams: 1000})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	const rounds = 500
	for i := range rounds {
		streamID := uint32(2*i + 1)
		openRawTunnel(peer, streamID)
		peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
			rst, ok := frame.(*xhttp2.RSTStreamFrame)
			return ok && rst.StreamID == streamID
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if bad != 0 {
		t.Fatalf("%d operations racing Close failed without net.ErrClosed: %v", bad, counts)
	}
}

// A write WriteByteTimeout tears down reports a write timeout, not a
// deadline error: the peer grants credit but stops reading the socket, so the
// physical write stalls and the connection fails (E7 revised the setup: a
// flow-control stall on a tunnel is bounded only by the StreamConn's own
// deadline).
func TestWriteByteTimeoutFailsStreamWritesWithWriteTimeout(t *testing.T) {
	type result struct {
		first, second error
	}
	results := make(chan result, 1)
	payload := bytes.Repeat([]byte("p"), 32<<20)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		_ = ctx.AcceptStream(func(conn fasthttp.StreamConn) {
			_, first := conn.Write(payload)
			_, second := conn.Write([]byte("again"))
			results <- result{first, second}
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true, WriteByteTimeout: 100 * time.Millisecond})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1<<31 - 1}); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteWindowUpdate(0, 1<<31-1-65535); err != nil {
		t.Fatal(err)
	}
	openRawTunnel(peer, 1)
	// The peer reads nothing from here on.
	var got result
	select {
	case got = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("Write() didn't return")
	}
	for _, err := range []error{got.first, got.second} {
		var netErr net.Error
		if !errors.Is(err, errWriteTimeout) || errors.Is(err, os.ErrDeadlineExceeded) ||
			!errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("Write() errors = (%v, %v), want %v for both", got.first, got.second, errWriteTimeout)
		}
	}
}

// Response.SkipBody suppresses the body as it does for HTTP/1: a HEAD
// middleware that delegates to a GET handler sends headers only.
func TestServerHonorsResponseSkipBody(t *testing.T) {
	const body = "body explicitly suppressed by the handler"
	server := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.Request.Header.SetMethod(fasthttp.MethodGet)
		ctx.SetBodyString(body)
		ctx.Response.SkipBody = true
	}}, ServerConfig{})
	peer := dialRawPeer(t, server.listener.Addr().String())
	peer.writeHeaders(1, true,
		[2]string{":method", "HEAD"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/"},
	)
	_ = peer.conn.SetReadDeadline(time.Now().Add(time.Second))
	for {
		frame, err := peer.framer.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch f := frame.(type) {
		case *xhttp2.MetaHeadersFrame:
			if f.StreamID == 1 && f.StreamEnded() {
				return
			}
		case *xhttp2.DataFrame:
			if f.StreamID == 1 && len(f.Data()) > 0 {
				t.Fatalf("DATA %q sent despite Response.SkipBody", f.Data())
			}
			if f.StreamID == 1 && f.StreamEnded() {
				return
			}
		}
	}
}

// A CONNECT's request target is its authority, as HTTP/1 handlers see it.
func TestConnectRequestURIMatchesHTTP1(t *testing.T) {
	observed := make(chan string, 2)
	server := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		observed <- string(ctx.RequestURI())
		ctx.SetStatusCode(fasthttp.StatusForbidden)
	}}, ServerConfig{})
	conn, err := net.Dial("tcp", server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var resp fasthttp.Response
	if err := resp.Read(bufio.NewReader(conn)); err != nil {
		t.Fatal(err)
	}
	http1 := <-observed
	peer := dialRawPeer(t, server.listener.Addr().String())
	peer.writeHeaders(1, true, [2]string{":method", "CONNECT"}, [2]string{":authority", "example.com:443"})
	select {
	case http2 := <-observed:
		if http2 != http1 {
			t.Fatalf("RequestURI() = %q over HTTP/2, %q over HTTP/1", http2, http1)
		}
	case <-time.After(time.Second):
		t.Fatal("the HTTP/2 handler wasn't called")
	}
}

// A :method that isn't a token is malformed (RFC 9113 §8.3).
func TestMalformedMethodIsRejected(t *testing.T) {
	observed := make(chan string, 1)
	server := newTestServer(t, &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		observed <- string(ctx.Method())
	}}, ServerConfig{})
	peer := dialRawPeer(t, server.listener.Addr().String())
	peer.writeHeaders(1, true,
		[2]string{":method", "GET BAD"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "example.com"},
		[2]string{":path", "/"},
	)
	if _, kind := peer.waitForAny(time.Second, "headers", "rst_PROTOCOL_ERROR", "goaway_PROTOCOL_ERROR"); kind == "headers" {
		t.Fatalf("the handler ran with method %q", <-observed)
	}
}

// http1Exchange sends one HTTP/1.1 request to the server and returns the raw
// reply, read to the close the request asks for.
func http1Exchange(t testing.TB, addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(wire)
}

// streamOutcome reads a stream's response: its final status when HEADERS or
// DATA carried END_STREAM, or the RST_STREAM code that ended it.
func streamOutcome(peer *rawPeer, streamID uint32) (status string, reset *xhttp2.ErrCode) {
	peer.t.Helper()
	statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok && rst.StreamID == streamID {
			code := rst.ErrCode
			reset = &code
			return true
		}
		return isStreamEnd(streamID)(frame)
	})
	if len(statuses) != 0 {
		status = statuses[len(statuses)-1]
	}
	return status, reset
}

// An idle connection whose peer never sent its SETTINGS gets the final GOAWAY
// and the half-close at the idle deadline, without the PING round trip; a
// handshake-complete idle peer gets the PING, its final GOAWAY bounded by
// PingTimeout when it withholds the ACK and prompt when it answers.
func TestIdleCloseSkipsThePingForAnIncompleteHandshake(t *testing.T) {
	const idle, pingTimeout = 200 * time.Millisecond, time.Second
	type frameSeen struct {
		kind string
		at   time.Time
	}
	// readUntilClosed reads frames up to the server's FIN, naming each.
	readUntilClosed := func(t *testing.T, conn net.Conn, framer *xhttp2.Framer, onFrame func(xhttp2.Frame)) []frameSeen {
		t.Helper()
		var seen []frameSeen
		_ = conn.SetReadDeadline(time.Now().Add(idle + 2*pingTimeout))
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("frames %v, then %v; want the server's FIN", seen, err)
				}
				return append(seen, frameSeen{kind: "eof", at: time.Now()})
			}
			kind := frame.Header().Type.String()
			if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
				kind += "_" + goAway.ErrCode.String()
			}
			seen = append(seen, frameSeen{kind: kind, at: time.Now()})
			if onFrame != nil {
				onFrame(frame)
			}
		}
	}
	at := func(seen []frameSeen, kind string, nth int) time.Time {
		for _, frame := range seen {
			if frame.kind == kind {
				if nth == 0 {
					return frame.at
				}
				nth--
			}
		}
		return time.Time{}
	}
	server := func(t *testing.T) string {
		t.Helper()
		testServer := newTestServer(t, &fasthttp.Server{Handler: func(*fasthttp.RequestCtx) {}, ReadTimeout: idle},
			ServerConfig{PingTimeout: pingTimeout})
		return testServer.listener.Addr().String()
	}
	t.Run("bare-preface", func(t *testing.T) {
		conn, err := net.Dial("tcp", server(t))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		start := time.Now()
		if _, err := io.WriteString(conn, clientPreface); err != nil {
			t.Fatal(err)
		}
		seen := readUntilClosed(t, conn, xhttp2.NewFramer(nil, conn), nil)
		goAway, eof := at(seen, "GOAWAY_NO_ERROR", 0), at(seen, "eof", 0)
		if goAway.IsZero() || goAway.Sub(start) < idle/2 || goAway.Sub(start) >= idle+pingTimeout/2 {
			t.Fatalf("frames %v: GOAWAY %v after the preface, want about %v without the PING wait", seen, goAway.Sub(start), idle)
		}
		if eof.Sub(goAway) >= pingTimeout/2 {
			t.Fatalf("frames %v: FIN %v after the GOAWAY", seen, eof.Sub(goAway))
		}
		if !at(seen, "PING", 0).IsZero() {
			t.Fatalf("frames %v: a PING for a peer that never sent SETTINGS", seen)
		}
	})
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "withheld-ack", true: "acked"}[ack], func(t *testing.T) {
			peer := dialRawPeer(t, server(t))
			seen := readUntilClosed(t, peer.conn, peer.framer, func(frame xhttp2.Frame) {
				if ping, ok := frame.(*xhttp2.PingFrame); ok && !ping.IsAck() && ack {
					if err := peer.framer.WritePing(true, ping.Data); err != nil {
						t.Error(err)
					}
				}
			})
			first, final := at(seen, "GOAWAY_NO_ERROR", 0), at(seen, "GOAWAY_NO_ERROR", 1)
			if first.IsZero() || final.IsZero() || at(seen, "PING", 0).IsZero() {
				t.Fatalf("frames %v: want GOAWAY, PING and the final GOAWAY", seen)
			}
			wait := final.Sub(first)
			switch {
			case ack && wait >= pingTimeout/2:
				t.Fatalf("frames %v: final GOAWAY %v after the first with the PING answered", seen, wait)
			case !ack && wait < pingTimeout*4/5:
				t.Fatalf("frames %v: final GOAWAY %v after the first with the PING unanswered, want PingTimeout %v", seen, wait, pingTimeout)
			}
		})
	}
}

// IdleTimeout runs from the moment the last stream closed, also for a stream
// that came and went within one event-loop round; a busy connection is never
// closed as idle, and an idle one is closed once it has been idle that long.
func TestIdleTimeoutRunsFromTheLastStreamClose(t *testing.T) {
	const idle = 300 * time.Millisecond
	for _, tc := range []struct {
		name   string
		server *fasthttp.Server
		config ServerConfig
	}{
		{name: "idle-timeout", server: &fasthttp.Server{}, config: ServerConfig{IdleTimeout: idle}},
		{name: "read-timeout-fallback", server: &fasthttp.Server{ReadTimeout: idle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.server.Handler = func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }
			testServer := newTestServer(t, tc.server, tc.config)
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			start := time.Now()
			streamID := uint32(1)
			for time.Since(start) < 3*idle {
				peer.request(streamID, "/", true)
				statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
					if goAway, ok := frame.(*xhttp2.GoAwayFrame); ok {
						t.Fatalf("GOAWAY(%v) after %v on a connection busy every %v", goAway.ErrCode, time.Since(start), idle/5)
					}
					return isStreamEnd(streamID)(frame)
				})
				if !slices.Equal(statuses, []string{"200"}) {
					t.Fatalf("stream %d statuses = %q", streamID, statuses)
				}
				streamID += 2
				time.Sleep(idle / 5)
			}
			// Idle from here: the timeout runs from the last stream's end.
			if counts, kind := peer.waitForAny(3*idle, "goaway_NO_ERROR", "closed"); kind != "goaway_NO_ERROR" {
				t.Fatalf("no shutdown after %v idle: %v", 3*idle, counts)
			}
		})
	}
}

// A 204 drops its body as HTTP/1 does (Response.write skips it) instead of
// failing the stream, buffered or streamed.
func TestNoContentResponseDropsItsBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler fasthttp.RequestHandler
	}{
		{name: "buffered", handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusNoContent)
			ctx.SetBodyString("dropped")
		}},
		{name: "stream", handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusNoContent)
			ctx.SetBodyStreamWriter(func(w *bufio.Writer) { _, _ = w.WriteString("dropped") })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testServer := newTestServer(t, &fasthttp.Server{Handler: tc.handler}, ServerConfig{})
			addr := testServer.listener.Addr().String()
			http1 := http1Exchange(t, addr, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
			if !strings.HasPrefix(http1, "HTTP/1.1 204") || strings.Contains(http1, "dropped") {
				t.Fatalf("HTTP/1 control: %q", http1)
			}
			peer := dialRawPeer(t, addr)
			peer.request(1, "/", true)
			status, reset := streamOutcome(peer, 1)
			if reset != nil {
				t.Fatalf("RST_STREAM(%v) after status %q, want 204 with END_STREAM", *reset, status)
			}
			if status != "204" {
				t.Fatalf("status = %q, want 204", status)
			}
			peer.request(3, "/", true)
			if status, reset := streamOutcome(peer, 3); reset != nil || status != "204" {
				t.Fatalf("next request: status %q, reset %v", status, reset)
			}
		})
	}
}

// closeCountingConn counts its Close calls; it stays a net.Conn.
type closeCountingConn struct {
	net.Conn

	closes atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

// A body stream a bodyless response drops is closed once, although both the
// handler's completion and the stream's finalization abandon it.
func TestDroppedBodyStreamIsClosedOnce(t *testing.T) {
	reader, writer := net.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	body := &closeCountingConn{Conn: reader}
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(fasthttp.StatusNoContent)
		ctx.SetBodyStream(body, -1)
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/", true)
	if status, reset := streamOutcome(peer, 1); reset != nil || status != "204" {
		t.Fatalf("status = %q, reset = %v, want 204", status, reset)
	}
	deadline := time.Now().Add(2 * time.Second)
	for body.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if closes := body.closes.Load(); closes != 1 {
		t.Fatalf("dropped body stream closed %d times, want 1", closes)
	}
}

// responseFields reads one stream's response header block.
func (p *rawPeer) responseFields(streamID uint32) map[string]string {
	p.t.Helper()
	fields := map[string]string{}
	p.readHeaders(streamID, func(frame xhttp2.Frame) bool {
		headers, ok := frame.(*xhttp2.MetaHeadersFrame)
		if !ok || headers.StreamID != streamID {
			return false
		}
		for _, field := range headers.Fields {
			fields[field.Name] = field.Value
		}
		return true
	})
	return fields
}

// A Content-Length set before the status became 204 stays off the wire, as
// HTTP/1 drops it; a 304 keeps its declared length in both protocols.
func TestBodylessStatusContentLengthOnTheWire(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.SetContentLength(7)
		if string(ctx.Path()) == "/not-modified" {
			ctx.SetStatusCode(fasthttp.StatusNotModified)
			return
		}
		ctx.SetStatusCode(fasthttp.StatusNoContent)
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		path, status, contentLength string
	}{
		{path: "/", status: "204", contentLength: ""},
		{path: "/not-modified", status: "304", contentLength: "7"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			http1 := http1Exchange(t, addr, "GET "+tc.path+" HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
			wantLine := strings.Contains(http1, "\r\nContent-Length: 7\r\n")
			if !strings.HasPrefix(http1, "HTTP/1.1 "+tc.status) || wantLine != (tc.contentLength != "") {
				t.Fatalf("HTTP/1 control: %q", http1)
			}
			peer := dialRawPeer(t, addr)
			peer.request(1, tc.path, true)
			fields := peer.responseFields(1)
			if fields[":status"] != tc.status || fields["content-length"] != tc.contentLength {
				t.Fatalf("response fields = %v, want status %s and content-length %q", fields, tc.status, tc.contentLength)
			}
		})
	}
}

// RFC 9218 §7.1: a PRIORITY_UPDATE for a promised stream that closed is
// discarded; only one for a stream still idle is an error.
func TestPriorityUpdateForClosedPushStreamIsIgnored(t *testing.T) {
	conn := &serverConn{
		config:             serverConfig{maxConcurrentStreams: 1},
		streams:            make(map[uint32]*serverStream),
		priorityUpdates:    make(map[uint32]priority),
		lastClientStreamID: 1,
		nextPushStreamID:   4, // stream 2 was promised and has since closed
	}
	if err := conn.processPriorityUpdate(&incomingFrame{streamID: 2, priority: "u=1"}); err != nil {
		t.Fatalf("PRIORITY_UPDATE for the closed push stream 2: %v", err)
	}
	err := conn.processPriorityUpdate(&incomingFrame{streamID: 4, priority: "u=1"})
	var protocolErr *serverError
	if !errors.As(err, &protocolErr) || protocolErr.code != xhttp2.ErrCodeProtocol {
		t.Fatalf("PRIORITY_UPDATE for the idle push stream 4: error = %v, want PROTOCOL_ERROR", err)
	}
}

func streamWorkerGoroutines() int {
	buffer := make([]byte, 1<<20)
	n := runtime.Stack(buffer, true)
	return strings.Count(string(buffer[:n]), "(*streamWorker).run(")
}

// Workers a burst parked are reaped on a connection that stays busy, not
// only when the idle timer fires.
func TestIdleStreamWorkersAreReapedOnABusyConnection(t *testing.T) {
	const burst = 40
	reapAfter := maxIdleStreamWorkerDuration
	maxIdleStreamWorkerDuration = 200 * time.Millisecond
	t.Cleanup(func() { maxIdleStreamWorkerDuration = reapAfter })
	release := make(chan struct{})
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) == "/burst" {
			<-release
		}
		ctx.SetBodyString("ok")
	}}
	// No idle timer: Server.IdleTimeout and ReadTimeout are zero.
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 100})
	get := func(path string) {
		t.Helper()
		resp, err := testServer.client.Get(testServer.URL(path))
		if err != nil {
			t.Errorf("Get(%s) error: %v", path, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	get("/")
	var wg sync.WaitGroup
	for range burst {
		wg.Go(func() { get("/burst") })
	}
	deadline := time.Now().Add(2 * time.Second)
	for streamWorkerGoroutines() < burst && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if parked := streamWorkerGoroutines(); parked < burst {
		t.Fatalf("only %d workers after the burst", parked)
	}
	// Busy past the reap interval: one request at a time, one worker.
	stop := time.Now().Add(3 * maxIdleStreamWorkerDuration)
	for time.Now().Before(stop) {
		get("/")
		time.Sleep(20 * time.Millisecond)
	}
	if dials := testServer.dials.Load(); dials != 1 {
		t.Fatalf("test used %d connections", dials)
	}
	if after := streamWorkerGoroutines(); after > burst/2 {
		t.Fatalf("%d of %d burst workers still parked on the busy connection", after, burst)
	}
}

// Workers a burst parked are reaped on a connection that goes silent, with
// no idle timer and no further event to run the reaper, and the connection
// still serves afterwards. The idle list is read through the owner.
func TestIdleStreamWorkersAreReapedOnASilentConnection(t *testing.T) {
	const burst = 20
	reapAfter := maxIdleStreamWorkerDuration
	maxIdleStreamWorkerDuration = 100 * time.Millisecond
	t.Cleanup(func() { maxIdleStreamWorkerDuration = reapAfter })
	started := make(chan struct{}, burst)
	release := make(chan struct{})
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) == "/burst" {
			started <- struct{}{}
			<-release
		}
		ctx.SetBodyString("ok")
	}}
	// No idle timer: Server.IdleTimeout and ReadTimeout are zero.
	testServer, conns := newOwnedTestServer(t, server, ServerConfig{MaxConcurrentStreams: 100})
	get := func(path string) {
		t.Helper()
		resp, err := testServer.client.Get(testServer.URL(path))
		if err != nil {
			t.Errorf("Get(%s) error: %v", path, err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	get("/")
	var conn *serverConn
	select {
	case conn = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("no connection served")
	}
	workers := func() (parked, all int) {
		t.Helper()
		inspectOwner(t, conn, func(c *serverConn) { parked, all = len(c.idleWorkers), len(c.allWorkers) })
		return parked, all
	}
	var wg sync.WaitGroup
	for range burst {
		wg.Go(func() { get("/burst") })
	}
	for range burst {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("handlers didn't start")
		}
	}
	close(release)
	wg.Wait()
	if parked, all := workers(); parked != burst || all != burst {
		t.Fatalf("%d of %d workers parked after the burst, want all %d", parked, all, burst)
	}
	// Silence: nothing but the reap timer runs on the connection.
	deadline := time.Now().Add(10 * maxIdleStreamWorkerDuration)
	parked, all := workers()
	for (parked != 0 || all != 0) && time.Now().Before(deadline) {
		time.Sleep(maxIdleStreamWorkerDuration / 4)
		parked, all = workers()
	}
	if parked != 0 || all != 0 {
		t.Fatalf("%d workers parked, %d alive on the silent connection, want none", parked, all)
	}
	get("/")
	if dials := testServer.dials.Load(); dials != 1 {
		t.Fatalf("test used %d connections", dials)
	}
}

// HeaderReceived's per-request ReadTimeout bounds the body read and its
// WriteTimeout the response, as the HTTP/1 serve loop applies them.
func TestHeaderReceivedTimeoutsApplyPerRequest(t *testing.T) {
	const perRequest = 100 * time.Millisecond
	t.Run("read", func(t *testing.T) {
		server := &fasthttp.Server{
			HeaderReceived: func(*fasthttp.RequestHeader) fasthttp.RequestConfig {
				return fasthttp.RequestConfig{ReadTimeout: perRequest}
			},
			Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
		}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.writeHeaders(1, false,
			[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
			[2]string{":authority", "example.com"}, [2]string{":path", "/"},
		)
		// The body never comes: the error path answers (E12).
		if counts, kind := peer.waitForAny(5*perRequest, "rst_NO_ERROR", "rst_CANCEL"); kind != "rst_NO_ERROR" || counts["headers"] != 1 {
			t.Fatalf("request without a body survived a per-request ReadTimeout of %v: %s %v", perRequest, kind, counts)
		}
	})
	t.Run("write", func(t *testing.T) {
		server := &fasthttp.Server{
			HeaderReceived: func(*fasthttp.RequestHeader) fasthttp.RequestConfig {
				return fasthttp.RequestConfig{WriteTimeout: perRequest}
			},
			Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("stalled by the peer's window") },
		}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		// No send window: the response body stalls.
		if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
			t.Fatal(err)
		}
		peer.request(1, "/", true)
		if counts, kind := peer.waitForAny(5*perRequest, "rst_INTERNAL_ERROR"); kind != "rst_INTERNAL_ERROR" {
			t.Fatalf("stalled response survived a per-request WriteTimeout of %v: %v", perRequest, counts)
		}
	})
}

// Server.WriteTimeout is one deadline per response, armed when the handler
// returns, as HTTP/1's write deadline is: a response that keeps progressing
// but isn't queued whole by then is reset with INTERNAL_ERROR, a stalled one
// too; a fast one completes; an accepted tunnel has no response deadline (E7).
func TestWriteTimeoutBoundsTheWholeResponse(t *testing.T) {
	const writeTimeout = 40 * time.Millisecond
	t.Run("slow-body-stream", func(t *testing.T) {
		server := &fasthttp.Server{WriteTimeout: writeTimeout, Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
				for range 8 {
					time.Sleep(25 * time.Millisecond)
					if _, err := w.WriteString("chunk"); err != nil {
						return
					}
					if err := w.Flush(); err != nil {
						return
					}
				}
			})
		}}
		testServer := newTestServer(t, server, ServerConfig{})
		for _, client := range []struct {
			name   string
			client *stdhttp.Client
		}{{"HTTP/1", http1Client(t)}, {"HTTP/2", testServer.client}} {
			start := time.Now()
			resp, err := client.client.Get(testServer.URL("/"))
			if err != nil {
				t.Fatalf("%s: Get() error: %v", client.name, err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if elapsed := time.Since(start); err == nil || elapsed > 150*time.Millisecond {
				t.Fatalf("%s: body %q after %v with error %v; want the response cut at about %v", client.name, body, elapsed, err, writeTimeout)
			}
		}
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/", true)
		if status, reset := streamOutcome(peer, 1); reset == nil || *reset != xhttp2.ErrCodeInternal {
			t.Fatalf("status %q, reset %v; want RST_STREAM(INTERNAL_ERROR)", status, reset)
		}
	})
	t.Run("fast-response", func(t *testing.T) {
		body := bytes.Repeat([]byte("f"), 1<<20)
		server := &fasthttp.Server{WriteTimeout: writeTimeout, Handler: func(ctx *fasthttp.RequestCtx) { ctx.Response.SetBodyRaw(body) }}
		testServer := newTestServer(t, server, ServerConfig{})
		got := doProtocolRequest(t, testServer.client, stdhttp.MethodGet, testServer.URL("/"), nil)
		if got.status != 200 || len(got.body) != len(body) {
			t.Fatalf("response = status %d, %d body bytes; want 200 and %d", got.status, len(got.body), len(body))
		}
	})
	t.Run("stalled-peer", func(t *testing.T) {
		server := &fasthttp.Server{WriteTimeout: writeTimeout, Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("stalled") }}
		testServer := newTestServer(t, server, ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
			t.Fatal(err)
		}
		peer.request(1, "/", true)
		if status, reset := streamOutcome(peer, 1); status != "200" || reset == nil || *reset != xhttp2.ErrCodeInternal {
			t.Fatalf("status %q, reset %v; want 200 then RST_STREAM(INTERNAL_ERROR)", status, reset)
		}
	})
	t.Run("accepted-tunnel-outlives-it", func(t *testing.T) {
		server := &fasthttp.Server{WriteTimeout: writeTimeout, Handler: func(ctx *fasthttp.RequestCtx) {
			if err := ctx.AcceptStream(func(conn fasthttp.StreamConn) {
				time.Sleep(3 * writeTimeout)
				_, _ = conn.Write([]byte("late"))
			}); err != nil {
				t.Errorf("AcceptStream() error: %v", err)
			}
		}}
		testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		openRawTunnel(peer, 1)
		var data bytes.Buffer
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			switch frame := frame.(type) {
			case *xhttp2.DataFrame:
				data.Write(frame.Data())
				return frame.StreamEnded()
			case *xhttp2.RSTStreamFrame:
				t.Fatalf("tunnel reset with %v", frame.ErrCode)
			}
			return false
		})
		if data.String() != "late" {
			t.Fatalf("tunnel data = %q, want %q", data.String(), "late")
		}
	})
}

// With StreamRequestBody a declared Content-Length, zero included, gives the
// handler a body stream, as HTTP/1's ContinueReadBodyStream does; a request
// without body framing has none.
func TestZeroContentLengthStreamedRequestHasAnEmptyBodyStream(t *testing.T) {
	type seen struct {
		proto  string
		stream bool
	}
	results := make(chan seen, 2)
	server := &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			results <- seen{proto: string(ctx.Request.Header.Protocol()), stream: ctx.RequestBodyStream() != nil}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	for _, tc := range []struct {
		name   string
		http1  string
		fields [][2]string
	}{
		{
			name:  "content-length-zero",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			fields: [][2]string{
				{":method", fasthttp.MethodPost},
				{":scheme", "http"},
				{":authority", "example.com"},
				{":path", "/"},
				{"content-length", "0"},
			},
		},
		{
			name:  "no-body",
			http1: "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
			fields: [][2]string{
				{":method", fasthttp.MethodGet},
				{":scheme", "http"},
				{":authority", "example.com"},
				{":path", "/"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = http1Exchange(t, addr, tc.http1)
			http1 := <-results
			peer := dialRawPeer(t, addr)
			peer.writeHeaders(1, true, tc.fields...)
			peer.readHeaders(1, isStreamEnd(1))
			http2 := <-results
			if http1.stream != http2.stream {
				t.Fatalf("RequestBodyStream() != nil: %s %v, %s %v", http1.proto, http1.stream, http2.proto, http2.stream)
			}
		})
	}
}

// A buffered body without a declared length reports ContentLength() -1 as
// HTTP/1 does after a chunked body, with the same Transfer-Encoding entry.
func TestBufferedBodyWithoutContentLengthReportsChunkedLength(t *testing.T) {
	type seen struct {
		proto    string
		length   int
		read     int
		encoding string
	}
	results := make(chan seen, 2)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		results <- seen{
			proto:    string(ctx.Request.Header.Protocol()),
			length:   ctx.Request.Header.ContentLength(),
			read:     len(ctx.PostBody()),
			encoding: string(ctx.Request.Header.Peek(fasthttp.HeaderTransferEncoding)),
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	_ = http1Exchange(t, addr, "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
	http1 := <-results
	peer := dialRawPeer(t, addr)
	peer.writeHeaders(1, false,
		[2]string{":method", fasthttp.MethodPost}, [2]string{":scheme", "http"},
		[2]string{":authority", "example.com"}, [2]string{":path", "/"},
	)
	if err := peer.framer.WriteData(1, true, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isStreamEnd(1))
	http2 := <-results
	if http1.read != http2.read || http1.length != http2.length || http1.encoding != http2.encoding {
		t.Fatalf("handler saw %+v and %+v", http1, http2)
	}
}

// A request's framing metadata reads as HTTP/1's: -2 for a GET without a
// declared length, 0 for a bodyless POST without one, -1 with
// Transfer-Encoding: chunked once an undeclared body is framed, the declared
// value otherwise; in both body modes.
func TestRequestFramingMetadataMatchesHTTP1(t *testing.T) {
	type seen struct {
		length                int
		lengthField, encoding string
		body                  int
	}
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			results := make(chan seen, 1)
			server := &fasthttp.Server{
				StreamRequestBody: streaming,
				Handler: func(ctx *fasthttp.RequestCtx) {
					results <- seen{
						length:      ctx.Request.Header.ContentLength(),
						lengthField: string(ctx.Request.Header.Peek(fasthttp.HeaderContentLength)),
						encoding:    string(ctx.Request.Header.Peek(fasthttp.HeaderTransferEncoding)),
						body:        len(ctx.PostBody()),
					}
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			addr := testServer.listener.Addr().String()
			for _, tc := range []struct {
				name, http1 string
				method      string
				fields      [][2]string
				data        string
				want        seen
			}{
				{
					name:   "get",
					http1:  "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
					method: fasthttp.MethodGet,
					want:   seen{length: -2},
				},
				{
					name:   "post-declared",
					http1:  "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello",
					method: fasthttp.MethodPost,
					fields: [][2]string{{"content-length", "5"}},
					data:   "hello",
					want:   seen{length: 5, lengthField: "5", body: 5},
				},
				{
					name:   "post-undeclared",
					http1:  "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n5\r\nhello\r\n0\r\n\r\n",
					method: fasthttp.MethodPost,
					data:   "hello",
					want:   seen{length: -1, encoding: "chunked", body: 5},
				},
				{
					name:   "post-bodyless",
					http1:  "POST / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
					method: fasthttp.MethodPost,
					want:   seen{length: 0, lengthField: "0"},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_ = http1Exchange(t, addr, tc.http1)
					http1 := <-results
					if http1 != tc.want {
						t.Fatalf("HTTP/1 control = %+v, want %+v", http1, tc.want)
					}
					peer := dialRawPeer(t, addr)
					fields := append([][2]string{
						{":method", tc.method}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/"},
					}, tc.fields...)
					peer.writeHeaders(1, tc.data == "", fields...)
					if tc.data != "" {
						if err := peer.framer.WriteData(1, true, []byte(tc.data)); err != nil {
							t.Fatal(err)
						}
					}
					peer.readHeaders(1, isStreamEnd(1))
					if http2 := <-results; http2 != tc.want {
						t.Fatalf("HTTP/2 = %+v, want %+v", http2, tc.want)
					}
				})
			}
		})
	}
}

// An upload over the limit while the Expect job is pending, queued or
// running, is answered through ErrorHandler once the job is out of the way,
// as it is for a request the job already answered.
func TestTooLargeBodyDuringExpectJobAnswersThroughErrorHandler(t *testing.T) {
	const limit = 16
	for _, queued := range []bool{false, true} {
		name := "running"
		if queued {
			name = "queued"
		}
		t.Run(name, func(t *testing.T) {
			hold := make(chan struct{})
			errs := make(chan error, 1)
			var expectCalls atomic.Int32
			server := &fasthttp.Server{
				MaxRequestBodySize: limit,
				ExpectHandler: func(*fasthttp.RequestCtx) int {
					expectCalls.Add(1)
					<-hold
					return fasthttp.StatusContinue
				},
				Handler: func(ctx *fasthttp.RequestCtx) {
					if string(ctx.Path()) == "/hold" {
						<-hold
					}
					ctx.SetBodyString("handled")
				},
				ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
					errs <- err
					ctx.Error("too large", fasthttp.StatusRequestEntityTooLarge)
				},
			}
			testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			uploadID := uint32(1)
			if queued {
				// Stream 1's handler holds the only job slot; stream 3's
				// Expect job queues behind it.
				peer.request(1, "/hold", true)
				if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
					t.Fatal(err)
				}
				uploadID = 3
			}
			writeExpectPost(peer, uploadID, "/upload")
			if err := peer.framer.WriteData(uploadID, false, make([]byte, 2*limit)); err != nil {
				t.Fatal(err)
			}
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			peer.readHeaders(uploadID, func(frame xhttp2.Frame) bool {
				if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok && rst.StreamID == uploadID {
					t.Fatalf("oversized upload during the Expect job: RST_STREAM(%v)", rst.ErrCode)
				}
				return isPingAck(frame)
			})
			close(hold)
			var reset *xhttp2.RSTStreamFrame
			statuses := peer.readHeaders(uploadID, func(frame xhttp2.Frame) bool {
				rst, ok := frame.(*xhttp2.RSTStreamFrame)
				if ok && rst.StreamID == uploadID {
					reset = rst
				}
				return ok
			})
			if !slices.Equal(statuses, []string{"413"}) || reset.ErrCode != xhttp2.ErrCodeNo {
				t.Fatalf("statuses = %q, reset = %v; want [413] then RST_STREAM(NO_ERROR)", statuses, reset.ErrCode)
			}
			select {
			case err := <-errs:
				if !errors.Is(err, fasthttp.ErrBodyTooLarge) {
					t.Fatalf("ErrorHandler error = %v, want %v", err, fasthttp.ErrBodyTooLarge)
				}
			default:
				t.Fatal("ErrorHandler didn't run")
			}
			if queued && expectCalls.Load() != 0 {
				t.Fatalf("ExpectHandler ran %d times for a request refused while queued", expectCalls.Load())
			}
		})
	}
}

// With StreamRequestBody the body isn't capped by MaxRequestBodySize, as in
// HTTP/1: the handler reads everything, declared or not.
func TestStreamedRequestBodyIsNotCapped(t *testing.T) {
	const limit = 16
	type seen struct {
		proto string
		read  int
		err   error
	}
	body := bytes.Repeat([]byte("abcdefgh"), limit/2)
	for _, declared := range []bool{true, false} {
		name := "undeclared"
		if declared {
			name = "declared"
		}
		t.Run(name, func(t *testing.T) {
			results := make(chan seen, 2)
			refused := make(chan error, 2)
			server := &fasthttp.Server{
				StreamRequestBody:  true,
				MaxRequestBodySize: limit,
				Handler: func(ctx *fasthttp.RequestCtx) {
					data, err := io.ReadAll(ctx.RequestBodyStream())
					results <- seen{proto: string(ctx.Request.Header.Protocol()), read: len(data), err: err}
					ctx.SetBodyString("ok")
				},
				ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
					refused <- err
					ctx.Error("refused", fasthttp.StatusRequestEntityTooLarge)
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			addr := testServer.listener.Addr().String()
			request := "POST / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n"
			if declared {
				request += fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
			} else {
				request += fmt.Sprintf("Transfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body)
			}
			_ = http1Exchange(t, addr, request)
			var http1 seen
			select {
			case http1 = <-results:
			case err := <-refused:
				t.Fatalf("HTTP/1 refused the streamed upload: %v", err)
			}
			peer := dialRawPeer(t, addr)
			fields := [][2]string{
				{":method", fasthttp.MethodPost},
				{":scheme", "http"},
				{":authority", "example.com"},
				{":path", "/"},
			}
			if declared {
				fields = append(fields, [2]string{"content-length", strconv.Itoa(len(body))})
			}
			peer.writeHeaders(1, false, fields...)
			if err := peer.framer.WriteData(1, true, body); err != nil {
				t.Fatal(err)
			}
			select {
			case http2 := <-results:
				if http2.read != http1.read || http2.err != nil || http1.err != nil {
					t.Fatalf("handler saw %+v and %+v", http1, http2)
				}
			case err := <-refused:
				t.Fatalf("HTTP/2 refused the streamed upload through ErrorHandler: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP/2 handler didn't run")
			}
			if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("statuses = %q", statuses)
			}
		})
	}
}

// Trailers on a client stream ID below the newest that is neither open nor
// remembered as closed name a closed stream, implicitly (RFC 9113 §5.1.1) or
// with its tombstone aged out of the ring: they reset that stream with
// STREAM_CLOSED. A request on such an ID still fails the connection.
func TestHeadersOnAnEvictedClosedStream(t *testing.T) {
	for _, trailers := range []bool{true, false} {
		name := "request"
		if trailers {
			name = "trailers"
		}
		t.Run(name, func(t *testing.T) {
			testServer := newTestServer(t, &fasthttp.Server{
				Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
			}, ServerConfig{MaxConcurrentStreams: 1}) // four tombstones
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			for streamID := uint32(1); streamID <= 11; streamID += 2 {
				peer.request(streamID, "/", true)
				peer.readHeaders(streamID, isStreamEnd(streamID))
			}
			// Stream 1's tombstone is gone.
			if trailers {
				peer.writeHeaders(1, true, [2]string{"x-trailer", "late"})
			} else {
				peer.request(1, "/", true)
			}
			if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
				t.Fatal(err)
			}
			kinds := []string{"rst_STREAM_CLOSED", "goaway_PROTOCOL_ERROR", "closed"}
			counts, kind := peer.waitForAny(5*time.Second, kinds...)
			if trailers && kind != "rst_STREAM_CLOSED" || !trailers && kind != "goaway_PROTOCOL_ERROR" {
				t.Fatalf("HEADERS on the evicted stream: %v", counts)
			}
			if trailers {
				// The connection is still serving.
				peer.readHeaders(1, isPingAck)
			}
		})
	}
}

// The body writer runs only once the pump reads, after the response headers
// went out: a trailer value it sets never races with header encoding, and
// reaches the peer in the trailers.
func TestBodyWriterTrailerSetDoesNotRaceHeaderEncoding(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if err := ctx.Response.Header.AddTrailer("Grpc-Status"); err != nil {
			t.Errorf("AddTrailer() error: %v", err)
		}
		ctx.Response.Header.Set("Grpc-Status", "0")
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			_, _ = w.WriteString("one small message")
			if err := w.Flush(); err != nil {
				return
			}
			ctx.Response.Header.Set("Grpc-Status", "13")
		})
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	for range 20 {
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		peer.request(1, "/chat", true)
		trailer := ""
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			if headers, ok := frame.(*xhttp2.MetaHeadersFrame); ok && headers.StreamID == 1 {
				for _, field := range headers.Fields {
					if field.Name == "grpc-status" {
						trailer = field.Value
					}
				}
			}
			return isStreamEnd(1)(frame)
		})
		if trailer != "13" {
			t.Fatalf("grpc-status = %q, want the writer's 13 in the trailers", trailer)
		}
		_ = peer.conn.Close()
	}
}

// An undeclared buffered upload that completes while the Expect job runs
// reports its length as the one that completes afterwards: -1.
func TestExpectCompletedUploadReportsUndeclaredLength(t *testing.T) {
	hold := make(chan struct{})
	type observation struct {
		length int
		body   string
	}
	got := make(chan observation, 1)
	server := newTestServer(t, &fasthttp.Server{
		ExpectHandler: func(*fasthttp.RequestCtx) int { <-hold; return fasthttp.StatusContinue },
		Handler: func(ctx *fasthttp.RequestCtx) {
			got <- observation{ctx.Request.Header.ContentLength(), string(ctx.PostBody())}
		},
	}, ServerConfig{})
	peer := dialRawPeer(t, server.listener.Addr().String())
	writeExpectPost(peer, 1, "/")
	if err := peer.framer.WriteData(1, true, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{7}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isPingAck)
	close(hold)
	peer.readHeaders(1, isStreamEnd(1))
	if v := <-got; v.length != -1 || v.body != "hello" {
		t.Fatalf("ContentLength()=%d body=%q, want -1 and \"hello\"", v.length, v.body)
	}
}

// An Expect job queued behind the handler bound and reset by the peer before
// it runs finalizes without running: the PING barrier shows nothing is left
// on the stream, and the next stream takes the slot.
func TestQueuedExpectJobResetFinalizes(t *testing.T) {
	release := make(chan struct{})
	var expectCalls atomic.Int32
	server := &fasthttp.Server{
		ExpectHandler: func(*fasthttp.RequestCtx) int {
			expectCalls.Add(1)
			return fasthttp.StatusContinue
		},
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/hold" {
				<-release
			}
			ctx.SetBodyString("ok")
		},
	}
	testServer := newTestServer(t, server, ServerConfig{MaxConcurrentStreams: 1})
	defer close(release)
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	peer.request(1, "/hold", true)
	// Stream 1 is reset on the wire but keeps its handler and the job slot.
	if err := peer.framer.WriteRSTStream(1, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	// Stream 3's Expect job queues behind it; staged data arrives, then the
	// reset.
	writeExpectPost(peer, 3, "/expect")
	if err := peer.framer.WriteData(3, false, []byte("staged")); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteRSTStream(3, xhttp2.ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WritePing(false, [8]byte{3}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(3, func(frame xhttp2.Frame) bool {
		if rst, ok := frame.(*xhttp2.RSTStreamFrame); ok {
			t.Fatalf("RST_STREAM(%v) on stream %d", rst.ErrCode, rst.StreamID)
		}
		return isPingAck(frame)
	})
	// Stream 5 takes the freed wire slot; its handler waits for stream 1's.
	peer.request(5, "/next", true)
	release <- struct{}{}
	if statuses := peer.readHeaders(5, isStreamEnd(5)); !slices.Equal(statuses, []string{"200"}) {
		t.Fatalf("stream 5 statuses = %q", statuses)
	}
	if got := expectCalls.Load(); got != 0 {
		t.Fatalf("Expect job ran %d times for a stream reset while queued", got)
	}
}

// A tunnel handler blocked in Write on a zero send window when the peer
// drops the connection: the write fails and the connection winds down, so
// Shutdown completes.
func TestBlockedTunnelWriteUnblocksOnConnectionLoss(t *testing.T) {
	written := make(chan error, 1)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		if err := ctx.AcceptStream(func(stream fasthttp.StreamConn) {
			_, err := stream.Write(bytes.Repeat([]byte("x"), 1024))
			written <- err
		}); err != nil {
			t.Errorf("AcceptStream() error: %v", err)
		}
	}}
	testServer := newTestServer(t, server, ServerConfig{EnableExtendedConnect: true})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	openRawTunnel(peer, 1)
	peer.waitForAny(2*time.Second, "headers")
	// The write reaches the owner and stalls on flow control.
	if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
		t.Fatal(err)
	}
	peer.readHeaders(1, isPingAck)
	select {
	case err := <-written:
		t.Fatalf("Write() returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = peer.conn.Close()
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("Write() reported success on a dead connection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked tunnel write never returned after the connection was lost")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := testServer.server.ShutdownWithContext(ctx); err != nil {
		t.Fatalf("ShutdownWithContext() error: %v", err)
	}
}

// Expect accepted on a buffered request, then the client ends the request
// with a trailers-only HEADERS: the handler starts with an empty body and
// sees the trailer, whether the end arrives after the 100 or while the
// Expect job still runs; a request complete by then gets no 100.
func TestExpectThenTrailersOnlyEnd(t *testing.T) {
	for _, duringJob := range []bool{false, true} {
		name := "after-continue"
		if duringJob {
			name = "during-job"
		}
		t.Run(name, func(t *testing.T) {
			hold := make(chan struct{})
			got := make(chan string, 1)
			server := &fasthttp.Server{
				ExpectHandler: func(*fasthttp.RequestCtx) int {
					if duringJob {
						<-hold
					}
					return fasthttp.StatusContinue
				},
				Handler: func(ctx *fasthttp.RequestCtx) {
					got <- string(ctx.Request.Body()) + "|" + string(ctx.Request.Header.Peek("x-trailer"))
					ctx.SetBodyString("ok")
				},
			}
			testServer := newTestServer(t, server, ServerConfig{})
			peer := dialRawPeer(t, testServer.listener.Addr().String())
			writeExpectPost(peer, 1, "/")
			if duringJob {
				peer.writeHeaders(1, true, [2]string{"x-trailer", "yes"})
				if err := peer.framer.WritePing(false, [8]byte{1}); err != nil {
					t.Fatal(err)
				}
				peer.readHeaders(1, isPingAck)
				close(hold)
			} else {
				peer.readHeaders(1, func(frame xhttp2.Frame) bool {
					headers, ok := frame.(*xhttp2.MetaHeadersFrame)
					return ok && headers.StreamID == 1 && headers.PseudoValue("status") == "100"
				})
				peer.writeHeaders(1, true, [2]string{"x-trailer", "yes"})
			}
			select {
			case body := <-got:
				if body != "|yes" {
					t.Fatalf("handler saw %q, want an empty body and the trailer", body)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handler didn't run")
			}
			if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"200"}) {
				t.Fatalf("statuses = %q, want [200]", statuses)
			}
		})
	}
}

// runServerChaos drives randomized but valid frame interleavings over one
// connection: buffered, streamed and Expect uploads, early responses,
// resets, trailers, tunnels, priority updates, window toggles and handler
// queueing. With shutdownStep >= 0 the server starts shutting down mid-run.
// The connection must survive until the peer ends it (PING barrier), nothing
// may panic, every started handler must return and Shutdown must complete.
func runServerChaos(t *testing.T, seed int64, streams, shutdownStep int) {
	t.Helper()
	const (
		maxBody  = 48
		maxConc  = 4
		bodyUnit = 7
	)
	var handlersStarted, handlersDone atomic.Int32
	server := &fasthttp.Server{
		StreamRequestBody:  true,
		MaxRequestBodySize: maxBody,
		ExpectHandler: func(ctx *fasthttp.RequestCtx) int {
			if strings.HasSuffix(string(ctx.Path()), "/deny") {
				ctx.SetBodyString("denied")
				return fasthttp.StatusExpectationFailed
			}
			return fasthttp.StatusContinue
		},
		Handler: func(ctx *fasthttp.RequestCtx) {
			handlersStarted.Add(1)
			defer handlersDone.Add(1)
			if len(ctx.Request.Header.ConnectProtocol()) != 0 {
				_ = ctx.AcceptStream(func(stream fasthttp.StreamConn) {
					_ = stream.SetDeadline(time.Now().Add(200 * time.Millisecond))
					buffer := make([]byte, 32)
					for {
						n, err := stream.Read(buffer)
						if n > 0 {
							if _, err := stream.Write(buffer[:n]); err != nil {
								return
							}
						}
						if err != nil {
							_ = stream.CloseWrite()
							return
						}
					}
				})
				return
			}
			path := string(ctx.Path())
			switch {
			case strings.Contains(path, "/read"):
				if body := ctx.RequestBodyStream(); body != nil {
					_, _ = io.Copy(io.Discard, body)
				}
				ctx.SetBodyString("read")
			case strings.Contains(path, "/stream"):
				_ = ctx.Response.Header.AddTrailer("X-Sum")
				ctx.Response.Header.Set("X-Sum", "done")
				ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
					for range 4 {
						_, _ = w.Write(bytes.Repeat([]byte("s"), 4096))
						if err := w.Flush(); err != nil {
							return
						}
					}
				})
			case strings.Contains(path, "/head"):
				ctx.SetStatusCode(fasthttp.StatusNoContent)
			default:
				ctx.SetBodyString("plain")
			}
		},
	}
	testServer := newTestServer(t, server, ServerConfig{
		MaxConcurrentStreams:  maxConc,
		EnableExtendedConnect: true,
		// The raw peer doesn't answer the shutdown PING.
		PingTimeout: 200 * time.Millisecond,
	})
	peer := dialRawPeer(t, testServer.listener.Addr().String())
	rng := rand.New(rand.NewSource(seed))

	var wg sync.WaitGroup
	readerDone := make(chan struct{})
	pingAcks := make(chan [8]byte, 16)
	var goAway atomic.Pointer[xhttp2.GoAwayFrame]
	var closedByServer sync.Map
	wg.Go(func() {
		defer close(readerDone)
		for {
			_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			frame, err := peer.framer.ReadFrame()
			if err != nil {
				return
			}
			switch frame := frame.(type) {
			case *xhttp2.PingFrame:
				if frame.IsAck() {
					pingAcks <- frame.Data
				}
			case *xhttp2.GoAwayFrame:
				copied := *frame
				goAway.Store(&copied)
				if copied.ErrCode != xhttp2.ErrCodeNo {
					return
				}
			case *xhttp2.RSTStreamFrame:
				closedByServer.Store(frame.StreamID, true)
			case *xhttp2.DataFrame:
				if frame.StreamEnded() {
					closedByServer.Store(frame.StreamID, true)
				}
			case *xhttp2.MetaHeadersFrame:
				if frame.StreamEnded() {
					closedByServer.Store(frame.StreamID, true)
				}
			}
		}
	})
	// pingSync waits for the server to answer a PING, so every frame it sent
	// before it, resets included, has been seen by the reader.
	pingSeq := byte(0)
	pingSync := func() bool {
		pingSeq++
		data := [8]byte{'s', 'y', 'n', 'c', pingSeq}
		if err := peer.framer.WritePing(false, data); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case got := <-pingAcks:
				if got == data {
					return true
				}
			case <-readerDone:
				return false
			case <-time.After(5 * time.Second):
				t.Fatal("no PING ack")
			}
		}
	}

	type open struct {
		id      uint32
		sent    int
		ended   bool
		tunnel  bool
		nextOps int
	}
	var live []*open
	nextID := uint32(1)
	kinds := []string{"/plain", "/read", "/stream", "/head", "/read/deny", "/plain/x"}
	shutdownResult := make(chan error, 1)
	shuttingDown := false
	for step := 0; step < streams*6; step++ {
		if step == shutdownStep {
			shuttingDown = true
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				shutdownResult <- testServer.server.ShutdownWithContext(ctx)
			}()
		}
		if goAway.Load() != nil {
			shuttingDown = true
		}
		if !shuttingDown && len(live) < maxConc*3 && (len(live) == 0 || rng.Intn(3) == 0) && nextID < uint32(2*streams) { // #nosec G115
			id := nextID
			nextID += 2
			stream := &open{id: id, nextOps: rng.Intn(5)}
			if rng.Intn(5) == 0 {
				stream.tunnel = true
				openRawTunnel(peer, id)
			} else {
				path := kinds[rng.Intn(len(kinds))]
				endStream := rng.Intn(3) == 0
				fields := [][2]string{
					{":method", fasthttp.MethodPost},
					{":scheme", "http"},
					{":authority", "example.com"},
					{":path", path},
				}
				if !endStream && rng.Intn(3) == 0 {
					fields = append(fields, [2]string{"expect", "100-continue"})
				}
				if rng.Intn(4) == 0 && !endStream {
					fields = append(fields, [2]string{"content-length", "14"})
					stream.nextOps = 2
				}
				peer.writeHeaders(id, endStream, fields...)
				stream.ended = endStream
			}
			live = append(live, stream)
			continue
		}
		if len(live) == 0 {
			if shuttingDown {
				break
			}
			continue
		}
		i := rng.Intn(len(live))
		stream := live[i]
		remove := false
		if _, closed := closedByServer.Load(stream.id); closed {
			// The server ended or reset it; a compliant peer sends nothing more.
			live[i] = live[len(live)-1]
			live = live[:len(live)-1]
			continue
		}
		switch rng.Intn(8) {
		case 0, 1:
			if stream.ended {
				break
			}
			end := stream.nextOps == 0 || rng.Intn(4) == 0
			payload := bytes.Repeat([]byte("d"), bodyUnit)
			if stream.sent+bodyUnit > 14 && !stream.tunnel {
				payload = nil
			}
			if err := peer.framer.WriteData(stream.id, end && !stream.tunnel, payload); err != nil {
				t.Fatal(err)
			}
			stream.sent += len(payload)
			if stream.nextOps > 0 {
				stream.nextOps--
			}
			if end && !stream.tunnel {
				stream.ended = true
			}
		case 2:
			if stream.ended || stream.tunnel {
				break
			}
			// Trailers on a stream the server ended long ago are a stream
			// error, and a connection error once its tombstone aged out:
			// synchronize first.
			if !pingSync() {
				break
			}
			if _, closed := closedByServer.Load(stream.id); closed {
				remove = true
				break
			}
			peer.writeHeaders(stream.id, true, [2]string{"x-trailer", "t"})
			stream.ended = true
		case 3:
			if err := peer.framer.WriteRSTStream(stream.id, xhttp2.ErrCodeCancel); err != nil {
				t.Fatal(err)
			}
			remove = true
		case 4:
			if err := peer.framer.WriteWindowUpdate(stream.id, 1024); err != nil {
				t.Fatal(err)
			}
		case 5:
			if stream.ended && rng.Intn(2) == 0 {
				remove = true
			}
		case 6:
			if err := peer.framer.WritePriorityUpdate(stream.id, "u=2, i"); err != nil {
				t.Fatal(err)
			}
		case 7:
			window := uint32(65535)
			if rng.Intn(2) == 0 {
				window = 0
			}
			if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: window}); err != nil {
				t.Fatal(err)
			}
		}
		if remove {
			live[i] = live[len(live)-1]
			live = live[:len(live)-1]
		}
		if step%40 == 0 {
			if err := peer.framer.WriteWindowUpdate(0, 65535); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Responses the peer starved of credit finish once the windows reopen.
	if err := peer.framer.WriteSettings(xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := peer.framer.WriteWindowUpdate(0, 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, stream := range live {
		if err := peer.framer.WriteRSTStream(stream.id, xhttp2.ErrCodeCancel); err != nil {
			t.Fatal(err)
		}
	}
	if !pingSync() {
		if g := goAway.Load(); g != nil && g.ErrCode != xhttp2.ErrCodeNo {
			t.Fatalf("connection failed: GOAWAY(%v) last=%d", g.ErrCode, g.LastStreamID)
		}
		if !shuttingDown {
			t.Fatal("connection closed before the PING ack")
		}
	}
	if shutdownStep >= 0 {
		select {
		case err := <-shutdownResult:
			if err != nil {
				t.Fatalf("ShutdownWithContext() error: %v (handlers started=%d done=%d)", err, handlersStarted.Load(), handlersDone.Load())
			}
		case <-time.After(6 * time.Second):
			t.Fatal("Shutdown didn't return")
		}
	}
	_ = peer.conn.Close()
	wg.Wait()
	if shutdownStep < 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := testServer.server.ShutdownWithContext(ctx); err != nil {
			t.Fatalf("ShutdownWithContext() error: %v (handlers started=%d done=%d)", err, handlersStarted.Load(), handlersDone.Load())
		}
	}
	if handlersStarted.Load() != handlersDone.Load() {
		t.Fatalf("handlers started=%d done=%d", handlersStarted.Load(), handlersDone.Load())
	}
}

func TestServerStateMachineChaos(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		runServerChaos(t, 20261101+seed, 60, -1)
	}
}

func TestServerStateMachineChaosDuringShutdown(t *testing.T) {
	for seed := int64(1); seed <= 2; seed++ {
		runServerChaos(t, 20261111+seed, 60, 90+int(seed)*37)
	}
}

// A handler's TE response field is dropped like the other connection-specific
// fields instead of failing the response; HTTP/1 writes it. A request's TE
// other than trailers stays malformed.
func TestResponseTEIsDropped(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("TE", "trailers")
		ctx.Response.Header.Set("X-Kept", "yes")
		ctx.SetBodyString("ok")
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	http1 := http1Exchange(t, addr, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(http1, "HTTP/1.1 200") || !strings.Contains(http1, "\r\nTe: trailers\r\n") {
		t.Fatalf("HTTP/1 control: %q", http1)
	}
	peer := dialRawPeer(t, addr)
	peer.request(1, "/", true)
	fields := peer.responseFields(1)
	if fields[":status"] != "200" || fields["x-kept"] != "yes" || fields["te"] != "" {
		t.Fatalf("response fields = %v, want 200 with x-kept and without te", fields)
	}
	peer.writeHeaders(3, true, append(requestFields("/"), [2]string{"te", "gzip"})...)
	if status, reset := streamOutcome(peer, 3); status != "" || reset == nil || *reset != xhttp2.ErrCodeProtocol {
		t.Fatalf("te: gzip request: status = %q, reset = %v, want RST_STREAM(PROTOCOL_ERROR)", status, reset)
	}
}

// A second content-length field in a request header block is malformed, an
// identical value included, as HTTP/1's parser rejects it; one field is
// served.
func TestDuplicateContentLengthIsMalformed(t *testing.T) {
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		ctx.SetBody(ctx.PostBody())
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	http1 := http1Exchange(t, addr, "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 2\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
	if !strings.HasPrefix(http1, "HTTP/1.1 400") {
		t.Fatalf("HTTP/1 control: %q", http1)
	}
	peer := dialRawPeer(t, addr)
	post := func(streamID uint32, lengths ...string) {
		fields := make([][2]string, 0, 4+len(lengths))
		fields = append(fields, [2]string{":method", "POST"}, [2]string{":scheme", "http"}, [2]string{":authority", "example.com"}, [2]string{":path", "/"})
		for _, length := range lengths {
			fields = append(fields, [2]string{"content-length", length})
		}
		peer.writeHeaders(streamID, lengths[0] == "0", fields...)
		if lengths[0] != "0" {
			if err := peer.framer.WriteData(streamID, true, []byte("hi")); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, lengths := range [][]string{{"2", "2"}, {"0", "0"}} {
		streamID := uint32(2*i + 1) // #nosec G115
		post(streamID, lengths...)
		if status, reset := streamOutcome(peer, streamID); status != "" || reset == nil || *reset != xhttp2.ErrCodeProtocol {
			t.Fatalf("content-length %v: status = %q, reset = %v, want RST_STREAM(PROTOCOL_ERROR)", lengths, status, reset)
		}
	}
	post(5, "2")
	if status, reset := streamOutcome(peer, 5); status != "200" || reset != nil {
		t.Fatalf("single content-length: status = %q, reset = %v, want 200", status, reset)
	}
}

// An undeclared buffered body that completes empty reports ContentLength() 0
// without a Transfer-Encoding entry, as HTTP/1's empty chunked body does;
// padding isn't content, and the Expect route and a trailers-only completion
// read the same. Content keeps -1 with the entry.
func TestEmptyUndeclaredBodyReportsZeroLength(t *testing.T) {
	results := make(chan string, 1)
	hold := make(chan struct{})
	server := &fasthttp.Server{
		ExpectHandler: func(*fasthttp.RequestCtx) int { <-hold; return fasthttp.StatusContinue },
		Handler: func(ctx *fasthttp.RequestCtx) {
			results <- fmt.Sprintf("cl=%d te=%q body=%q", ctx.Request.Header.ContentLength(),
				ctx.Request.Header.Peek(fasthttp.HeaderTransferEncoding), ctx.PostBody())
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	post := func(peer *rawPeer, fields ...[2]string) {
		peer.writeHeaders(1, false, append([][2]string{
			{":method", "POST"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/"},
		}, fields...)...)
	}
	for _, tc := range []struct {
		name, http1 string
		http2       func(peer *rawPeer)
	}{
		{
			name:  "empty",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n",
			http2: func(peer *rawPeer) {
				post(peer)
				if err := peer.framer.WriteData(1, true, nil); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "padded-empty",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n",
			http2: func(peer *rawPeer) {
				post(peer)
				if err := peer.framer.WriteDataPadded(1, true, nil, make([]byte, 8)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "expect-staged-empty",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\n\r\n",
			http2: func(peer *rawPeer) {
				post(peer, [2]string{"expect", "100-continue"})
				if err := peer.framer.WriteData(1, true, nil); err != nil {
					t.Fatal(err)
				}
				// The DATA is staged before the Expect job answers.
				if err := peer.framer.WritePing(false, [8]byte{7}); err != nil {
					t.Fatal(err)
				}
				peer.readHeaders(1, isPingAck)
				hold <- struct{}{}
			},
		},
		{
			name:  "trailers-only",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nTrailer: X-T\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n0\r\nX-T: v\r\n\r\n",
			http2: func(peer *rawPeer) {
				post(peer, [2]string{"trailer", "x-t"})
				peer.writeHeaders(1, true, [2]string{"x-t", "v"})
			},
		},
		{
			name:  "content",
			http1: "POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\nhi\r\n0\r\n\r\n",
			http2: func(peer *rawPeer) {
				post(peer)
				if err := peer.framer.WriteData(1, true, []byte("hi")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "expect-staged-empty" {
				go func() { hold <- struct{}{} }()
			}
			_ = http1Exchange(t, addr, tc.http1)
			http1 := <-results
			peer := dialRawPeer(t, addr)
			tc.http2(peer)
			if status, reset := streamOutcome(peer, 1); status != "200" {
				t.Fatalf("status = %q, reset = %v", status, reset)
			}
			if http2 := <-results; http2 != http1 {
				t.Fatalf("HTTP/2 handler saw %s, HTTP/1 %s", http2, http1)
			}
		})
	}
}

// A Trailer declaration naming a forbidden field answers 400 through
// ErrorHandler with ErrBadTrailer, as HTTP/1's parser failure does, with the
// connection still usable; repeated declarations accumulate, and a
// header-section field of a declared name stays there when the trailer
// arrives, as HTTP/1 keeps it.
func TestForbiddenTrailerDeclarationIsRefused(t *testing.T) {
	type seen struct {
		keys, check string
	}
	handled := make(chan seen, 1)
	refused := make(chan error, 1)
	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if ctx.Request.IsBodyStream() {
				_, _ = io.Copy(io.Discard, ctx.RequestBodyStream())
			}
			trailerKeys := ctx.Request.Header.PeekTrailerKeys()
			keys := make([]string, 0, len(trailerKeys))
			for _, key := range trailerKeys {
				keys = append(keys, string(key))
			}
			handled <- seen{keys: strings.Join(keys, ","), check: string(ctx.Request.Header.Peek("X-Check"))}
			ctx.SetBodyString("ok")
		},
		ErrorHandler: func(ctx *fasthttp.RequestCtx, err error) {
			refused <- err
			ctx.Error("refused", fasthttp.StatusBadRequest)
		},
	}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()
	http1 := http1Exchange(t, addr, "GET / HTTP/1.1\r\nHost: example.com\r\nTrailer: Content-Length\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(http1, "HTTP/1.1 400") || !errors.Is(<-refused, fasthttp.ErrBadTrailer) {
		t.Fatalf("HTTP/1 control: %q", http1)
	}
	peer := dialRawPeer(t, addr)
	post := func(streamID uint32, endStream bool, fields ...[2]string) {
		peer.writeHeaders(streamID, endStream, append([][2]string{
			{":method", "POST"}, {":scheme", "http"}, {":authority", "example.com"}, {":path", "/"},
		}, fields...)...)
	}
	post(1, true, [2]string{"trailer", "content-length"})
	if status, reset := streamOutcome(peer, 1); status != "400" || reset != nil {
		t.Fatalf("declaration complete at headers: status = %q, reset = %v, want 400", status, reset)
	}
	if err := <-refused; !errors.Is(err, fasthttp.ErrBadTrailer) {
		t.Fatalf("ErrorHandler error = %v, want %v", err, fasthttp.ErrBadTrailer)
	}
	post(3, false, [2]string{"trailer", "content-length, x-a"})
	if status, reset := streamOutcome(peer, 3); status != "400" {
		t.Fatalf("declaration with an open upload: status = %q, reset = %v, want 400", status, reset)
	}
	if err := <-refused; !errors.Is(err, fasthttp.ErrBadTrailer) {
		t.Fatalf("ErrorHandler error = %v, want %v", err, fasthttp.ErrBadTrailer)
	}
	if err := peer.framer.WriteData(3, true, []byte("dropped")); err != nil {
		t.Fatal(err)
	}
	post(5, false, [2]string{"x-check", "initial"}, [2]string{"trailer", "x-check"}, [2]string{"trailer", "x-more"})
	if err := peer.framer.WriteData(5, false, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	peer.writeHeaders(5, true, [2]string{"x-check", "tv"})
	if status, reset := streamOutcome(peer, 5); status != "200" || reset != nil {
		t.Fatalf("valid declarations: status = %q, reset = %v, want 200", status, reset)
	}
	if got := <-handled; got.keys != "X-Check,X-More" || got.check != "initial" {
		t.Fatalf("handler saw %+v, want both names declared and the header-section value", got)
	}
}

// A plain CONNECT is a tunnel like an extended one: the handler runs at the
// headers, as HTTP/1 runs it for a CONNECT without a body, seeing the same
// method, Host and request target; no body timer answers 408; an Expect job
// precedes it; a 2xx with AcceptStream moves bytes both ways without the
// extended-CONNECT setting, which a request carrying :protocol still needs.
func TestPlainConnectIsATunnel(t *testing.T) {
	type seen struct {
		method, host, uri string
	}
	observed := make(chan seen, 2)
	newServer := func(streaming bool, readTimeout time.Duration) *fasthttp.Server {
		return &fasthttp.Server{
			StreamRequestBody: streaming,
			ReadTimeout:       readTimeout,
			Handler: func(ctx *fasthttp.RequestCtx) {
				observed <- seen{string(ctx.Method()), string(ctx.Host()), string(ctx.RequestURI())}
				if string(ctx.Host()) == "echo" {
					if err := ctx.AcceptStream(func(conn fasthttp.StreamConn) {
						_, _ = io.Copy(conn, conn)
					}); err != nil {
						t.Errorf("AcceptStream() error: %v", err)
					}
					return
				}
				ctx.SetStatusCode(fasthttp.StatusMethodNotAllowed)
				ctx.SetBodyString("no tunnels here")
			},
		}
	}
	connect := func(peer *rawPeer, streamID uint32, authority string, fields ...[2]string) {
		peer.writeHeaders(streamID, false, append([][2]string{{":method", "CONNECT"}, {":authority", authority}}, fields...)...)
	}
	for _, tc := range []struct {
		name        string
		streaming   bool
		readTimeout time.Duration
	}{
		{name: "buffered"},
		{name: "buffered-read-timeout", readTimeout: 200 * time.Millisecond},
		{name: "streaming", streaming: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testServer := newTestServer(t, newServer(tc.streaming, tc.readTimeout), ServerConfig{})
			addr := testServer.listener.Addr().String()
			http1 := http1Exchange(t, addr, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nConnection: close\r\n\r\n")
			if !strings.HasPrefix(http1, "HTTP/1.1 405") {
				t.Fatalf("HTTP/1 control: %q", http1)
			}
			control := <-observed
			peer := dialRawPeer(t, addr)
			connect(peer, 1, "example.com:443")
			if status, reset := streamOutcome(peer, 1); status != "405" || reset != nil {
				t.Fatalf("status = %q, reset = %v, want the handler's 405", status, reset)
			}
			if got := <-observed; got != control {
				t.Fatalf("handler saw %+v over HTTP/2, %+v over HTTP/1", got, control)
			}
		})
	}
	t.Run("expect", func(t *testing.T) {
		testServer := newTestServer(t, newServer(false, 0), ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		connect(peer, 1, "example.com:443", [2]string{"expect", "100-continue"})
		if statuses := peer.readHeaders(1, isStreamEnd(1)); !slices.Equal(statuses, []string{"100", "405"}) {
			t.Fatalf("statuses = %q, want the 100 and the handler's 405", statuses)
		}
		<-observed
	})
	t.Run("accept-stream", func(t *testing.T) {
		testServer := newTestServer(t, newServer(false, 0), ServerConfig{})
		peer := dialRawPeer(t, testServer.listener.Addr().String())
		connect(peer, 1, "echo")
		if fields := peer.responseFields(1); fields[":status"] != "200" {
			t.Fatalf("response = %v, want 200", fields)
		}
		<-observed
		if err := peer.framer.WriteData(1, false, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		var echoed []byte
		peer.readHeaders(1, func(frame xhttp2.Frame) bool {
			data, ok := frame.(*xhttp2.DataFrame)
			if ok && data.StreamID == 1 {
				echoed = append(echoed, data.Data()...)
			}
			return len(echoed) == 4
		})
		if string(echoed) != "ping" {
			t.Fatalf("echoed %q, want ping", echoed)
		}
		if err := peer.framer.WriteData(1, true, nil); err != nil {
			t.Fatal(err)
		}
		if status, reset := streamOutcome(peer, 1); reset != nil {
			t.Fatalf("after the peer's END_STREAM: status = %q, reset = %v", status, reset)
		}
		// Extended CONNECT stays gated on its setting.
		openRawTunnel(peer, 3)
		if status, reset := streamOutcome(peer, 3); status != "" || reset == nil || *reset != xhttp2.ErrCodeProtocol {
			t.Fatalf("extended CONNECT without the setting: status = %q, reset = %v", status, reset)
		}
	})
}

// collectStream reads a stream's response: its status, the DATA bytes,
// whether END_STREAM arrived, and the RST_STREAM code that ended it instead.
func collectStream(peer *rawPeer, streamID uint32) (status string, body []byte, ended bool, reset *xhttp2.ErrCode) {
	peer.t.Helper()
	statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
		switch frame := frame.(type) {
		case *xhttp2.RSTStreamFrame:
			if frame.StreamID == streamID {
				code := frame.ErrCode
				reset = &code
				return true
			}
		case *xhttp2.DataFrame:
			if frame.StreamID == streamID {
				body = append(body, frame.Data()...)
				ended = frame.StreamEnded()
				return ended
			}
		case *xhttp2.MetaHeadersFrame:
			if frame.StreamID == streamID && frame.StreamEnded() {
				ended = true
				return true
			}
		}
		return false
	})
	if len(statuses) != 0 {
		status = statuses[len(statuses)-1]
	}
	return status, body, ended, reset
}

// readFirstData reads a stream's response up to its first DATA frame and
// returns the status and that frame's bytes.
func readFirstData(peer *rawPeer, streamID uint32) (status string, body []byte) {
	peer.t.Helper()
	statuses := peer.readHeaders(streamID, func(frame xhttp2.Frame) bool {
		data, ok := frame.(*xhttp2.DataFrame)
		if ok && data.StreamID == streamID {
			body = append(body, data.Data()...)
			return true
		}
		return false
	})
	if len(statuses) != 0 {
		status = statuses[len(statuses)-1]
	}
	return status, body
}

type closeErrorReader struct {
	io.Reader

	closeErr error
}

func (r closeErrorReader) Close() error { return r.closeErr }

// wrappedEOFReader returns its bytes with an error wrapping io.EOF.
type wrappedEOFReader struct {
	data []byte
}

var errReaderFailed = errors.New("reader failed")

func (r *wrappedEOFReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, fmt.Errorf("%w: %w", errReaderFailed, io.EOF)
}

// panicReader yields one chunk, then panics.
type panicReader struct {
	reads int
}

func (r *panicReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return copy(p, "hello"), nil
	}
	panic("read boom")
}

type panicCloser struct {
	io.Reader
}

func (panicCloser) Close() error { panic("close boom") }

// panicCloseConn releases a blocked Read, then panics.
type panicCloseConn struct {
	net.Conn
}

func (c panicCloseConn) Close() error {
	_ = c.Conn.Close()
	panic("cancel boom")
}

// A credit record is applied once, whichever side moves first: a body closed
// with consumed bytes still queued returns every byte's credit once, and a
// record of a stream reset in the meantime returns nothing twice.
func TestQueuedCreditIsAppliedOnce(t *testing.T) {
	const window = 8
	setup := func(t *testing.T) (*serverConn, *serverStream, *bytes.Buffer) {
		t.Helper()
		var wire bytes.Buffer
		conn := &serverConn{
			config:  serverConfig{connectionWindowSize: window, streamWindowSize: window},
			framer:  xhttp2.NewFramer(&wire, nil),
			streams: make(map[uint32]*serverStream),
		}
		conn.initQueues()
		stream := newServerStream(conn, 1)
		stream.handlerStarted = true
		stream.recv.window = 0
		stream.unconsumedFlow = window
		stream.body = newRequestBody(conn, stream.id)
		conn.streams[stream.id] = stream
		conn.openStreams = 1
		if err := stream.body.writeOwned([]byte("12345678"), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.body.Read(make([]byte, 3)); err != nil {
			t.Fatal(err)
		}
		return conn, stream, &wire
	}
	windowUpdates := func(t *testing.T, wire *bytes.Buffer) map[uint32]uint32 {
		t.Helper()
		totals := map[uint32]uint32{}
		framer := xhttp2.NewFramer(nil, wire)
		for wire.Len() > 0 {
			frame, err := framer.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			update, ok := frame.(*xhttp2.WindowUpdateFrame)
			if !ok {
				t.Fatalf("frame %T, want WINDOW_UPDATE", frame)
			}
			totals[update.StreamID] += update.Increment
		}
		return totals
	}
	t.Run("close-while-queued", func(t *testing.T) {
		conn, stream, wire := setup(t)
		if err := stream.body.Close(); err != nil {
			t.Fatal(err)
		}
		if err := conn.applyQueuedCredit(); err != nil {
			t.Fatalf("applyQueuedCredit() error: %v", err)
		}
		if conn.recv.window != window || stream.recv.window != window || stream.unconsumedFlow != 0 {
			t.Fatalf("credit: conn=%d stream=%d retained=%d, want %d, %d, 0", conn.recv.window, stream.recv.window, stream.unconsumedFlow, window, window)
		}
		if totals := windowUpdates(t, wire); totals[0] != window || totals[1] != window {
			t.Fatalf("wire credit = %v, want %d on both", totals, window)
		}
	})
	t.Run("reset-while-queued", func(t *testing.T) {
		conn, stream, wire := setup(t)
		conn.teardownStream(stream, peerResetError{code: xhttp2.ErrCodeCancel})
		if err := conn.applyQueuedCredit(); err != nil {
			t.Fatalf("applyQueuedCredit() error: %v", err)
		}
		if conn.recv.window != window || stream.unconsumedFlow != 0 {
			t.Fatalf("credit: conn=%d retained=%d, want %d and 0", conn.recv.window, stream.unconsumedFlow, window)
		}
		if totals := windowUpdates(t, wire); totals[0] != window || totals[1] != 0 {
			t.Fatalf("wire credit = %v, want %d on the connection alone", totals, window)
		}
	})
}

// The hijack no-response intent a handler sets after Hijack is the one the
// stream acts on, as HTTP/1 reads the final setting after the handler
// returns: Hijack then HijackSetNoResponse(true) aborts the stream with
// RST_STREAM(INTERNAL_ERROR) and no response, while HijackSetNoResponse(true),
// Hijack, HijackSetNoResponse(false) answers the 501. HTTP/1 on the same
// listener hands the first sequence's connection to the hijack handler
// without a response and answers the second with the handler's 200.
func TestHijackNoResponseOrderOverHTTP2(t *testing.T) {
	hijacked := make(chan struct{}, 4)
	server := &fasthttp.Server{Handler: func(ctx *fasthttp.RequestCtx) {
		handler := func(net.Conn) { hijacked <- struct{}{} }
		switch string(ctx.Path()) {
		case "/hijack-then-no-response":
			ctx.Hijack(handler)
			ctx.HijackSetNoResponse(true)
		case "/no-response-then-hijack-then-response":
			ctx.HijackSetNoResponse(true)
			ctx.Hijack(handler)
			ctx.HijackSetNoResponse(false)
		}
		ctx.SetBodyString("handler body")
	}}
	testServer := newTestServer(t, server, ServerConfig{})
	addr := testServer.listener.Addr().String()

	http1 := http1Exchange(t, addr, "GET /hijack-then-no-response HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if http1 != "" {
		t.Fatalf("HTTP/1 hijack without response answered %q", http1)
	}
	select {
	case <-hijacked:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP/1 didn't hand the connection to the hijack handler")
	}
	http1 = http1Exchange(t, addr, "GET /no-response-then-hijack-then-response HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if !strings.HasPrefix(http1, "HTTP/1.1 200") || !strings.HasSuffix(http1, "handler body") {
		t.Fatalf("HTTP/1 hijack with response answered %q", http1)
	}
	select {
	case <-hijacked:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP/1 didn't hand the connection to the hijack handler")
	}

	peer := dialRawPeer(t, addr)
	peer.request(1, "/hijack-then-no-response", true)
	if status, reset := streamOutcome(peer, 1); status != "" || reset == nil || *reset != xhttp2.ErrCodeInternal {
		t.Fatalf("Hijack then HijackSetNoResponse(true): status = %q, reset = %v, want RST_STREAM(INTERNAL_ERROR) alone", status, reset)
	}
	peer.request(3, "/no-response-then-hijack-then-response", true)
	if status, reset := streamOutcome(peer, 3); status != "501" || reset != nil {
		t.Fatalf("HijackSetNoResponse(false) last: status = %q, reset = %v, want 501", status, reset)
	}
	select {
	case <-hijacked:
		t.Fatal("an HTTP/2 request invoked a connection hijack handler")
	default:
	}
}
