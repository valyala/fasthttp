package http2

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func BenchmarkEndToEnd(b *testing.B) {
	nativeServer := newTestServer(b, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}, ServerConfig{MaxConcurrentStreams: 1000})
	goServer := newGoBenchmarkServer(b)
	nativeClient := newBenchmarkHostClient(b, nativeServer.listener.Addr().String())
	goClient, goDials := newGoBenchmarkClient(b)
	warmFasthttpClient(b, nativeClient, nativeServer.URL("/"))
	warmGoClient(b, goClient, goServer.URL("/"))

	for _, concurrency := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("fasthttp/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, nativeClient, nativeServer.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(nativeClient.ConnsCount()))
		})
		b.Run(fmt.Sprintf("net-http2/streams-%d", concurrency), func(b *testing.B) {
			benchmarkNetHTTP2Client(b, goClient, goServer.URL("/"), concurrency)
			assertBenchmarkConnections(b, goDials.Load())
		})
	}
}

func BenchmarkClientsAgainstGoServer(b *testing.B) {
	server := newGoBenchmarkServer(b)
	nativeClient := newBenchmarkHostClient(b, server.listener.Addr().String())
	goClient, goDials := newGoBenchmarkClient(b)
	warmFasthttpClient(b, nativeClient, server.URL("/"))
	warmGoClient(b, goClient, server.URL("/"))

	for _, concurrency := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("fasthttp/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, nativeClient, server.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(nativeClient.ConnsCount()))
		})
		b.Run(fmt.Sprintf("net-http2/streams-%d", concurrency), func(b *testing.B) {
			benchmarkNetHTTP2Client(b, goClient, server.URL("/"), concurrency)
			assertBenchmarkConnections(b, goDials.Load())
		})
	}
}

func BenchmarkServersWithFasthttpClient(b *testing.B) {
	nativeServer := newTestServer(b, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}, ServerConfig{MaxConcurrentStreams: 1000})
	goServer := newGoBenchmarkServer(b)
	nativeServerClient := newBenchmarkHostClient(b, nativeServer.listener.Addr().String())
	goServerClient := newBenchmarkHostClient(b, goServer.listener.Addr().String())
	warmFasthttpClient(b, nativeServerClient, nativeServer.URL("/"))
	warmFasthttpClient(b, goServerClient, goServer.URL("/"))

	for _, concurrency := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("fasthttp-server/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, nativeServerClient, nativeServer.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(nativeServerClient.ConnsCount()))
		})
		b.Run(fmt.Sprintf("go-server/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, goServerClient, goServer.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(goServerClient.ConnsCount()))
		})
	}
}

func BenchmarkHeaderCodec(b *testing.B) {
	wire := benchmarkRequestHeaderFrame(b)
	b.Run("private-codec", func(b *testing.B) {
		framer := xhttp2.NewFramer(io.Discard, &cyclingReader{data: wire})
		codec := newHeaderCodec(defaultHeaderTableSize, 64<<10)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			frame, err := framer.ReadFrame()
			if err != nil {
				b.Fatal(err)
			}
			headers := frame.(*xhttp2.HeadersFrame) //nolint:forcetypeassert
			fieldStorage := acquireIncomingHeaderFields(8)
			fields, truncated, invalid, err := codec.decode(
				framer,
				headers.StreamID,
				headers,
				fieldStorage.fields,
			)
			event := incomingFrame{fields: fields, fieldStorage: fieldStorage}
			releaseIncomingFrame(&event)
			if err != nil || truncated || invalid != nil || len(fields) != 4 {
				b.Fatalf("decode = (%d fields, truncated=%v, invalid=%v, err=%v)", len(fields), truncated, invalid, err)
			}
		}
	})
	b.Run("x-net-meta-headers", func(b *testing.B) {
		framer := xhttp2.NewFramer(io.Discard, &cyclingReader{data: wire})
		decoder := hpack.NewDecoder(defaultHeaderTableSize, nil)
		framer.ReadMetaHeaders = decoder
		framer.MaxHeaderListSize = 64 << 10
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			frame, err := framer.ReadFrame()
			if err != nil {
				b.Fatal(err)
			}
			headers := frame.(*xhttp2.MetaHeadersFrame) //nolint:forcetypeassert
			if headers.Truncated || len(headers.Fields) != 4 {
				b.Fatalf("decode = (%d fields, truncated=%v)", len(headers.Fields), headers.Truncated)
			}
		}
	})
}

func BenchmarkBodies(b *testing.B) {
	payload4KiB := bytes.Repeat([]byte("a"), 4<<10)
	payload1MiB := bytes.Repeat([]byte("b"), 1<<20)
	bufferedServer := newTestServer(b, &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			switch string(ctx.Path()) {
			case "/get-4k":
				ctx.Response.SetBodyRaw(payload4KiB)
			case "/get-1m":
				ctx.Response.SetBodyRaw(payload1MiB)
			default:
				ctx.SetBodyString("ok")
			}
		},
	}, ServerConfig{MaxConcurrentStreams: 1000})
	streamingServer := newTestServer(b, &fasthttp.Server{
		StreamRequestBody: true,
		Handler: func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/stream-response" {
				ctx.Response.SetBodyStream(bytes.NewReader(payload1MiB), len(payload1MiB))
				return
			}
			if body := ctx.RequestBodyStream(); body != nil {
				_, _ = io.Copy(io.Discard, body)
			}
			ctx.SetBodyString("ok")
		},
	}, ServerConfig{MaxConcurrentStreams: 1000})
	bufferedClient := newBenchmarkHostClient(b, bufferedServer.listener.Addr().String())
	streamingRequestClient := newBenchmarkHostClient(b, streamingServer.listener.Addr().String())
	streamingResponseClient := newBenchmarkHostClient(b, streamingServer.listener.Addr().String())
	streamingResponseClient.StreamResponseBody = true
	warmFasthttpClient(b, bufferedClient, bufferedServer.URL("/get-4k"))
	warmFasthttpClient(b, streamingRequestClient, streamingServer.URL("/stream-request"))
	warmFasthttpClient(b, streamingResponseClient, streamingServer.URL("/stream-response"))
	warmGoClient(b, bufferedServer.client, bufferedServer.URL("/get-4k"))
	warmGoClient(b, streamingServer.client, streamingServer.URL("/stream-response"))

	scenarios := []struct {
		name           string
		client         *fasthttp.HostClient
		goClient       *stdhttp.Client
		goDials        *atomic.Int64
		requestURI     string
		method         string
		requestBody    []byte
		responseSize   int
		streamRequest  bool
		streamResponse bool
	}{
		{"get-4k", bufferedClient, bufferedServer.client, bufferedServer.dials, bufferedServer.URL("/get-4k"), fasthttp.MethodGet, nil, len(payload4KiB), false, false},
		{"get-1m", bufferedClient, bufferedServer.client, bufferedServer.dials, bufferedServer.URL("/get-1m"), fasthttp.MethodGet, nil, len(payload1MiB), false, false},
		{"post-4k", bufferedClient, bufferedServer.client, bufferedServer.dials, bufferedServer.URL("/post-4k"), fasthttp.MethodPost, payload4KiB, 2, false, false},
		{"post-1m", bufferedClient, bufferedServer.client, bufferedServer.dials, bufferedServer.URL("/post-1m"), fasthttp.MethodPost, payload1MiB, 2, false, false},
		{"stream-request-1m", streamingRequestClient, streamingServer.client, streamingServer.dials, streamingServer.URL("/stream-request"), fasthttp.MethodPost, payload1MiB, 2, true, false},
		{"stream-response-1m", streamingResponseClient, streamingServer.client, streamingServer.dials, streamingServer.URL("/stream-response"), fasthttp.MethodGet, nil, len(payload1MiB), false, true},
	}
	for _, scenario := range scenarios {
		b.Run("fasthttp/"+scenario.name, func(b *testing.B) {
			benchmarkFasthttpBody(
				b,
				scenario.client,
				scenario.requestURI,
				scenario.method,
				scenario.requestBody,
				scenario.responseSize,
				scenario.streamRequest,
				scenario.streamResponse,
			)
			assertBenchmarkConnections(b, int64(scenario.client.ConnsCount()))
		})
		b.Run("net-http2/"+scenario.name, func(b *testing.B) {
			benchmarkNetHTTP2Body(
				b,
				scenario.goClient,
				scenario.requestURI,
				scenario.method,
				scenario.requestBody,
				scenario.responseSize,
			)
			assertBenchmarkConnections(b, scenario.goDials.Load())
		})
	}
}

func BenchmarkTLSGET(b *testing.B) {
	server := newTLSBenchmarkServer(b)
	nativeClient := newTLSBenchmarkHostClient(b, server.listener.Addr().String())
	warmFasthttpClient(b, nativeClient, server.URL("/"))
	warmGoClient(b, server.client, server.URL("/"))
	for _, concurrency := range []int{1, 100} {
		b.Run(fmt.Sprintf("fasthttp/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, nativeClient, server.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(nativeClient.ConnsCount()))
		})
		b.Run(fmt.Sprintf("net-http2/streams-%d", concurrency), func(b *testing.B) {
			benchmarkNetHTTP2Client(b, server.client, server.URL("/"), concurrency)
			assertBenchmarkConnections(b, server.dials.Load())
		})
	}
}

func newTLSBenchmarkServer(b *testing.B) *testServer {
	b.Helper()
	certificateData, keyData, err := fasthttp.GenerateTestCertificate("localhost")
	if err != nil {
		b.Fatalf("generating benchmark certificate: %v", err)
	}
	certificate, err := tls.X509KeyPair(certificateData, keyData)
	if err != nil {
		b.Fatalf("loading benchmark certificate: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listening for TLS benchmark: %v", err)
	}
	server := &fasthttp.Server{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString("ok")
		},
	}
	if err := ConfigureServer(server, ServerConfig{MaxConcurrentStreams: 1000}); err != nil {
		_ = listener.Close()
		b.Fatalf("ConfigureServer() error: %v", err)
	}
	tlsListener := tls.NewListener(listener, server.TLSConfig.Clone())
	dials := new(atomic.Int64)
	transport := &xhttp2.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Benchmark-only certificate.
		},
		DialTLSContext: func(ctx context.Context, network, addr string, config *tls.Config) (net.Conn, error) {
			dials.Add(1)
			var dialer net.Dialer
			rawConn, dialErr := dialer.DialContext(ctx, network, addr)
			if dialErr != nil {
				return nil, dialErr
			}
			tlsConn := tls.Client(rawConn, config)
			if handshakeErr := tlsConn.HandshakeContext(ctx); handshakeErr != nil {
				_ = rawConn.Close()
				return nil, handshakeErr
			}
			return tlsConn, nil
		},
	}
	result := &testServer{
		server:    server,
		listener:  tlsListener,
		scheme:    "https",
		transport: transport,
		client:    &stdhttp.Client{Transport: transport},
		dials:     dials,
		serveDone: make(chan error, 1),
	}
	go func() { result.serveDone <- server.Serve(tlsListener) }()
	b.Cleanup(func() {
		transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownWithContext(ctx)
		select {
		case <-result.serveDone:
		case <-time.After(time.Second):
			b.Error("TLS benchmark server didn't stop")
		}
	})
	return result
}

func newTLSBenchmarkHostClient(b *testing.B, addr string) *fasthttp.HostClient {
	b.Helper()
	client := &fasthttp.HostClient{
		Addr:               addr,
		IsTLS:              true,
		MaxConns:           1,
		MaxConnWaitTimeout: time.Minute,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // Benchmark-only certificate.
		},
	}
	if err := ConfigureHostClient(client, ClientConfig{MaxConcurrentStreams: 1000}); err != nil {
		b.Fatalf("ConfigureHostClient() error: %v", err)
	}
	b.Cleanup(client.CloseIdleConnections)
	return client
}

func benchmarkFasthttpBody(
	b *testing.B,
	client *fasthttp.HostClient,
	requestURI string,
	method string,
	requestBody []byte,
	responseSize int,
	streamRequest bool,
	streamResponse bool,
) {
	b.Helper()
	b.ReportAllocs()
	var next atomic.Int64
	var wait sync.WaitGroup
	b.ResetTimer()
	for range 100 {
		wait.Go(func() {
			request := fasthttp.AcquireRequest()
			response := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(request)
			defer fasthttp.ReleaseResponse(response)
			request.Header.SetMethod(method)
			request.SetRequestURI(requestURI)
			if len(requestBody) != 0 && !streamRequest {
				request.SetBodyRaw(requestBody)
			}
			for {
				if next.Add(1) > int64(b.N) {
					return
				}
				if streamRequest {
					request.SetBodyStream(bytes.NewReader(requestBody), len(requestBody))
				}
				if err := client.Do(request, response); err != nil {
					b.Errorf("Do() error: %v", err)
					return
				}
				if streamResponse {
					read, err := io.Copy(io.Discard, response.BodyStream())
					closeErr := response.CloseBodyStream()
					if err != nil || closeErr != nil || read != int64(responseSize) {
						b.Errorf("stream response = (%d bytes, read=%v, close=%v)", read, err, closeErr)
						return
					}
				} else if len(response.Body()) != responseSize {
					b.Errorf("response length = %d, want %d", len(response.Body()), responseSize)
					return
				}
				response.ResetBody()
			}
		})
	}
	wait.Wait()
}

func benchmarkNetHTTP2Body(
	b *testing.B,
	client *stdhttp.Client,
	requestURI string,
	method string,
	requestBody []byte,
	responseSize int,
) {
	b.Helper()
	b.ReportAllocs()
	var next atomic.Int64
	var wait sync.WaitGroup
	b.ResetTimer()
	for range 100 {
		wait.Go(func() {
			request, err := stdhttp.NewRequest(method, requestURI, stdhttp.NoBody)
			if err != nil {
				b.Errorf("NewRequest() error: %v", err)
				return
			}
			for {
				if next.Add(1) > int64(b.N) {
					return
				}
				if len(requestBody) != 0 {
					request.Body = io.NopCloser(bytes.NewReader(requestBody))
					request.ContentLength = int64(len(requestBody))
				}
				response, err := client.Do(request)
				if err != nil {
					b.Errorf("Do() error: %v", err)
					return
				}
				read, readErr := io.Copy(io.Discard, response.Body)
				closeErr := response.Body.Close()
				if readErr != nil || closeErr != nil || read != int64(responseSize) {
					b.Errorf("response = (%d bytes, read=%v, close=%v)", read, readErr, closeErr)
					return
				}
			}
		})
	}
	wait.Wait()
}

func benchmarkRequestHeaderFrame(b *testing.B) []byte {
	b.Helper()
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "example.com", Sensitive: true},
		{Name: ":path", Value: "/"},
	} {
		if err := encoder.WriteField(field); err != nil {
			b.Fatal(err)
		}
	}
	var wire bytes.Buffer
	framer := xhttp2.NewFramer(&wire, nil)
	if err := framer.WriteHeaders(xhttp2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: block.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	}); err != nil {
		b.Fatal(err)
	}
	return bytes.Clone(wire.Bytes())
}

type cyclingReader struct {
	data   []byte
	offset int
}

func (r *cyclingReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		amount := copy(destination[written:], r.data[r.offset:])
		written += amount
		r.offset += amount
		if r.offset == len(r.data) {
			r.offset = 0
		}
	}
	return written, nil
}

type goBenchmarkServer struct {
	listener net.Listener
}

func newGoBenchmarkServer(b *testing.B) *goBenchmarkServer {
	b.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listening for Go HTTP/2 benchmark: %v", err)
	}
	server := &xhttp2.Server{MaxConcurrentStreams: 1000}
	result := &goBenchmarkServer{listener: listener}
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.ServeConn(conn, &xhttp2.ServeConnOpts{
				Handler: stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, _ *stdhttp.Request) {
					_, _ = io.WriteString(writer, "ok")
				}),
			})
		}
	}()
	b.Cleanup(func() { _ = listener.Close() })
	return result
}

func (s *goBenchmarkServer) URL(path string) string {
	return "http://" + s.listener.Addr().String() + path
}

func newBenchmarkHostClient(b *testing.B, addr string) *fasthttp.HostClient {
	b.Helper()
	client := &fasthttp.HostClient{
		Addr:                addr,
		MaxConns:            1,
		MaxConnWaitTimeout:  time.Minute,
		MaxIdleConnDuration: time.Minute,
	}
	if err := ConfigureHostClient(client, ClientConfig{
		Mode:                 PriorKnowledge,
		MaxConcurrentStreams: 1000,
	}); err != nil {
		b.Fatalf("ConfigureHostClient() error: %v", err)
	}
	b.Cleanup(client.CloseIdleConnections)
	return client
}

func newGoBenchmarkClient(b *testing.B) (*stdhttp.Client, *atomic.Int64) {
	b.Helper()
	dials := new(atomic.Int64)
	transport := &xhttp2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			dials.Add(1)
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
	}
	b.Cleanup(transport.CloseIdleConnections)
	return &stdhttp.Client{Transport: transport}, dials
}

func assertBenchmarkConnections(b *testing.B, connections int64) {
	b.Helper()
	if connections != 1 {
		b.Fatalf("physical connections = %d, want 1", connections)
	}
}

func warmFasthttpClient(b *testing.B, client *fasthttp.HostClient, requestURI string) {
	b.Helper()
	request := fasthttp.AcquireRequest()
	response := fasthttp.AcquireResponse()
	request.SetRequestURI(requestURI)
	if err := client.Do(request, response); err != nil {
		b.Fatalf("warming fasthttp HTTP/2 connection: %v", err)
	}
	fasthttp.ReleaseRequest(request)
	fasthttp.ReleaseResponse(response)
}

func warmGoClient(b *testing.B, client *stdhttp.Client, requestURI string) {
	b.Helper()
	response, err := client.Get(requestURI)
	if err != nil {
		b.Fatalf("warming Go HTTP/2 connection: %v", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		b.Fatalf("warming Go HTTP/2 response: read=%v close=%v", readErr, closeErr)
	}
}

func benchmarkFasthttpClient(
	b *testing.B,
	client *fasthttp.HostClient,
	requestURI string,
	concurrency int,
) {
	b.Helper()
	b.ReportAllocs()
	var next atomic.Int64
	var wait sync.WaitGroup
	b.ResetTimer()
	for range concurrency {
		wait.Go(func() {
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)
			req.SetRequestURI(requestURI)
			for {
				iteration := next.Add(1)
				if iteration > int64(b.N) {
					return
				}
				if err := client.Do(req, resp); err != nil {
					b.Errorf("Do() error: %v", err)
					return
				}
				resp.ResetBody()
			}
		})
	}
	wait.Wait()
	b.StopTimer()
	runtime.KeepAlive(client)
}

func benchmarkNetHTTP2Client(
	b *testing.B,
	client *stdhttp.Client,
	requestURI string,
	concurrency int,
) {
	b.Helper()
	b.ReportAllocs()
	var next atomic.Int64
	var wait sync.WaitGroup
	b.ResetTimer()
	for range concurrency {
		wait.Go(func() {
			req, err := stdhttp.NewRequest(stdhttp.MethodGet, requestURI, stdhttp.NoBody)
			if err != nil {
				b.Errorf("NewRequest() error: %v", err)
				return
			}
			for {
				iteration := next.Add(1)
				if iteration > int64(b.N) {
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					b.Errorf("Do() error: %v", err)
					return
				}
				_, err = io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if err != nil || closeErr != nil {
					b.Errorf("reading response: %v, close: %v", err, closeErr)
					return
				}
			}
		})
	}
	wait.Wait()
	b.StopTimer()
	runtime.KeepAlive(client)
}

// BenchmarkHTTP1VersusHTTP2 answers the adoption question for existing
// fasthttp users: what does the same workload cost over native HTTP/2
// compared with fasthttp's own HTTP/1.1, with identical handlers? The HTTP/1
// arm uses fasthttp's deployment model of one pooled connection per in-flight
// request; the HTTP/2 arm multiplexes every stream over a single connection.
func BenchmarkHTTP1VersusHTTP2(b *testing.B) {
	handler := func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") }

	http1Listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listening for HTTP/1 benchmark: %v", err)
	}
	http1Server := &fasthttp.Server{Handler: handler}
	http1Done := make(chan struct{})
	go func() {
		defer close(http1Done)
		_ = http1Server.Serve(http1Listener)
	}()
	b.Cleanup(func() {
		_ = http1Server.Shutdown()
		<-http1Done
	})

	http2Server := newTestServer(b, &fasthttp.Server{Handler: handler}, ServerConfig{MaxConcurrentStreams: 1000})

	http1Client := &fasthttp.HostClient{
		Addr:                http1Listener.Addr().String(),
		MaxConns:            1024,
		MaxIdleConnDuration: time.Minute,
	}
	b.Cleanup(http1Client.CloseIdleConnections)
	http2Client := newBenchmarkHostClient(b, http2Server.listener.Addr().String())

	http1URL := "http://" + http1Listener.Addr().String() + "/"
	warmFasthttpClient(b, http1Client, http1URL)
	warmFasthttpClient(b, http2Client, http2Server.URL("/"))

	for _, concurrency := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("http1/conns-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, http1Client, http1URL, concurrency)
			b.ReportMetric(float64(http1Client.ConnsCount()), "phys-conns")
		})
		b.Run(fmt.Sprintf("http2/streams-%d", concurrency), func(b *testing.B) {
			benchmarkFasthttpClient(b, http2Client, http2Server.URL("/"), concurrency)
			assertBenchmarkConnections(b, int64(http2Client.ConnsCount()))
		})
	}

	uploadPayload := bytes.Repeat([]byte("u"), 4<<10)
	b.Run("http1/post-4KiB-conns-100", func(b *testing.B) {
		benchmarkFasthttpBody(b, http1Client, http1URL, fasthttp.MethodPost, uploadPayload, 2, false, false)
		b.ReportMetric(float64(http1Client.ConnsCount()), "phys-conns")
	})
	b.Run("http2/post-4KiB-streams-100", func(b *testing.B) {
		benchmarkFasthttpBody(b, http2Client, http2Server.URL("/"), fasthttp.MethodPost, uploadPayload, 2, false, false)
		assertBenchmarkConnections(b, int64(http2Client.ConnsCount()))
	})
}

func BenchmarkRequestBodyTinyChunks(b *testing.B) {
	for _, size := range []int{128 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				body := newRequestBody(nil)
				for range size {
					if err := body.writeOwned([]byte{'x'}, nil); err != nil {
						b.Fatal(err)
					}
				}
				body.discardWithError(errStreamClosed)
			}
		})
	}
}

func BenchmarkApplyDistinctRequestTrailers(b *testing.B) {
	const fieldCount = 1724
	fields := make([]hpack.HeaderField, fieldCount)
	for i := range fields {
		fields[i] = hpack.HeaderField{Name: fmt.Sprintf("t%05x", i)}
	}
	b.ReportAllocs()
	for b.Loop() {
		var validation, request fasthttp.RequestHeader
		if err := applyRequestTrailers(&validation, fields); err != nil {
			b.Fatal(err)
		}
		if err := applyRequestTrailers(&request, fields); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRepeatedInitialWindowSettings(b *testing.B) {
	settings := make([]xhttp2.Setting, 2730)
	for i := range settings {
		settings[i] = xhttp2.Setting{ID: xhttp2.SettingInitialWindowSize, Val: 65535}
	}
	conn := &serverConn{
		config:        serverConfig{maxEncoderTableSize: defaultHeaderTableSize},
		streams:       make(map[uint32]*serverStream, 250),
		connFlowState: connFlowState{peerInitialStreamWindow: 65535},
	}
	for id := uint32(1); id <= 499; id += 2 {
		conn.streams[id] = &serverStream{id: id, streamFlowState: streamFlowState{send: sendWindow{window: 65535}}}
	}
	conn.headerEncoder.init(defaultHeaderTableSize)
	b.ReportAllocs()
	for b.Loop() {
		if err := conn.applySettings(settings); err != nil {
			b.Fatal(err)
		}
	}
}
