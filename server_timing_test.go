package fasthttp

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

var defaultClientsCount = runtime.NumCPU()

func BenchmarkServerGet1ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, defaultClientsCount, 1)
}

func BenchmarkServerGet2ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, defaultClientsCount, 2)
}

func BenchmarkServerGet10ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, defaultClientsCount, 10)
}

func BenchmarkServerGet10000ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, defaultClientsCount, 10000)
}

func BenchmarkNetHTTPServerGet1ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, defaultClientsCount, 1)
}

func BenchmarkNetHTTPServerGet2ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, defaultClientsCount, 2)
}

func BenchmarkNetHTTPServerGet10ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, defaultClientsCount, 10)
}

func BenchmarkNetHTTPServerGet10000ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, defaultClientsCount, 10000)
}

func BenchmarkServerPost1ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, defaultClientsCount, 1)
}

func BenchmarkServerPost2ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, defaultClientsCount, 2)
}

func BenchmarkServerPost10ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, defaultClientsCount, 10)
}

func BenchmarkServerPost10KReqPerConn(b *testing.B) {
	benchmarkServerPost(b, defaultClientsCount, 10000)
}

func BenchmarkNetHTTPServerPost1ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, defaultClientsCount, 1)
}

func BenchmarkNetHTTPServerPost2ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, defaultClientsCount, 2)
}

func BenchmarkNetHTTPServerPost10ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, defaultClientsCount, 10)
}

func BenchmarkNetHTTPServerPost10KReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, defaultClientsCount, 10000)
}

func BenchmarkServerGet1ReqPerConn1KClients(b *testing.B) {
	benchmarkServerGet(b, 1000, 1)
}

func BenchmarkServerGet2ReqPerConn1KClients(b *testing.B) {
	benchmarkServerGet(b, 1000, 2)
}

func BenchmarkServerGet10ReqPerConn1KClients(b *testing.B) {
	benchmarkServerGet(b, 1000, 10)
}

func BenchmarkServerGet10KReqPerConn1KClients(b *testing.B) {
	benchmarkServerGet(b, 1000, 10000)
}

func BenchmarkNetHTTPServerGet1ReqPerConn1KClients(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 1000, 1)
}

func BenchmarkNetHTTPServerGet2ReqPerConn1KClients(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 1000, 2)
}

func BenchmarkNetHTTPServerGet10ReqPerConn1KClients(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 1000, 10)
}

func BenchmarkNetHTTPServerGet10KReqPerConn1KClients(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 1000, 10000)
}

func BenchmarkServerMaxConnsPerIP(b *testing.B) {
	clientsCount := 1000
	requestsPerConn := 10
	ch := make(chan struct{}, b.N)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Success("foobar", []byte("123"))
			registerServedRequest(b, ch)
		},
		MaxConnsPerIP: clientsCount * 2,
	}
	req := "GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n"
	benchmarkServer(b, &testServer{s, clientsCount}, clientsCount, requestsPerConn, req)
	verifyRequestsServed(b, ch)
}

func BenchmarkServerTimeoutError(b *testing.B) {
	clientsCount := 1
	requestsPerConn := 10
	ch := make(chan struct{}, b.N)
	n := uint32(0)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if atomic.AddUint32(&n, 1)&7 == 0 {
				ctx.TimeoutError("xxx")
				go func() {
					ctx.Success("foobar", []byte("123"))
				}()
			} else {
				ctx.Success("foobar", []byte("123"))
			}
			registerServedRequest(b, ch)
		},
	}
	req := "GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n"
	benchmarkServer(b, &testServer{s, clientsCount}, clientsCount, requestsPerConn, req)
	verifyRequestsServed(b, ch)
}

type fakeServerConn struct {
	net.TCPConn
	ln            *fakeListener
	requestsCount int
	closed        uint32
}

func (c *fakeServerConn) Read(b []byte) (int, error) {
	nn := 0
	for len(b) > len(c.ln.request) {
		if c.requestsCount == 0 {
			if nn == 0 {
				return 0, io.EOF
			}
			return nn, nil
		}
		n := copy(b, c.ln.request)
		b = b[n:]
		nn += n
		c.requestsCount--
	}
	if nn == 0 {
		panic("server has too small buffer")
	}
	return nn, nil
}

func (c *fakeServerConn) Write(b []byte) (int, error) {
	return len(b), nil
}

var fakeAddr = net.TCPAddr{
	IP:   []byte{1, 2, 3, 4},
	Port: 12345,
}

func (c *fakeServerConn) RemoteAddr() net.Addr {
	return &fakeAddr
}

func (c *fakeServerConn) Close() error {
	if atomic.AddUint32(&c.closed, 1) == 1 {
		c.ln.ch <- c
	}
	return nil
}

func (c *fakeServerConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *fakeServerConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type fakeListener struct {
	requestsCount   int
	requestsPerConn int
	request         []byte
	ch              chan *fakeServerConn
	done            chan struct{}
}

func (ln *fakeListener) Accept() (net.Conn, error) {
	if ln.requestsCount == 0 {
		for len(ln.ch) < cap(ln.ch) {
			time.Sleep(10 * time.Millisecond)
		}
		close(ln.done)
		return nil, io.EOF
	}
	requestsCount := ln.requestsPerConn
	if requestsCount > ln.requestsCount {
		requestsCount = ln.requestsCount
	}
	ln.requestsCount -= requestsCount

	c := <-ln.ch
	c.requestsCount = requestsCount
	c.closed = 0

	return c, nil
}

func (ln *fakeListener) Close() error {
	return nil
}

func (ln *fakeListener) Addr() net.Addr {
	return &fakeAddr
}

func newFakeListener(requestsCount, clientsCount, requestsPerConn int, request string) *fakeListener {
	ln := &fakeListener{
		requestsCount:   requestsCount,
		requestsPerConn: requestsPerConn,
		request:         []byte(request),
		ch:              make(chan *fakeServerConn, clientsCount),
		done:            make(chan struct{}),
	}
	for i := 0; i < clientsCount; i++ {
		ln.ch <- &fakeServerConn{
			ln: ln,
		}
	}
	return ln
}

var (
	fakeResponse = []byte("Hello, world!")
	getRequest   = "GET /foobar?baz HTTP/1.1\r\nHost: google.com\r\nUser-Agent: aaa/bbb/ccc/ddd/eee Firefox Chrome MSIE Opera\r\n" +
		"Referer: http://xxx.com/aaa?bbb=ccc\r\n\r\n"
	postRequest = fmt.Sprintf("POST /foobar?baz HTTP/1.1\r\nHost: google.com\r\nContent-Type: foo/bar\r\nContent-Length: %d\r\n"+
		"User-Agent: Opera Chrome MSIE Firefox and other/1.2.34\r\nReferer: http://google.com/aaaa/bbb/ccc\r\n\r\n%s",
		len(fakeResponse), fakeResponse)
)

func benchmarkServerGet(b *testing.B, clientsCount, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if !ctx.Request.Header.IsMethodGet() {
				b.Fatalf("Unexpected request method: %s", ctx.Request.Header.Method)
			}
			ctx.Success("text/plain", fakeResponse)
			registerServedRequest(b, ch)
		},
	}
	benchmarkServer(b, &testServer{s, clientsCount}, clientsCount, requestsPerConn, getRequest)
	verifyRequestsServed(b, ch)
}

func benchmarkNetHTTPServerGet(b *testing.B, clientsCount, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method != "GET" {
				b.Fatalf("Unexpected request method: %s", req.Method)
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write(fakeResponse)
			registerServedRequest(b, ch)
		}),
	}
	benchmarkServer(b, s, clientsCount, requestsPerConn, getRequest)
	verifyRequestsServed(b, ch)
}

func benchmarkServerPost(b *testing.B, clientsCount, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if !ctx.Request.Header.IsMethodPost() {
				b.Fatalf("Unexpected request method: %s", ctx.Request.Header.Method)
			}
			body := ctx.Request.Body
			if !bytes.Equal(body, fakeResponse) {
				b.Fatalf("Unexpected body %q. Expected %q", body, fakeResponse)
			}
			ctx.Success("text/plain", body)
			registerServedRequest(b, ch)
		},
	}
	benchmarkServer(b, &testServer{s, clientsCount}, clientsCount, requestsPerConn, postRequest)
	verifyRequestsServed(b, ch)
}

func benchmarkNetHTTPServerPost(b *testing.B, clientsCount, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method != "POST" {
				b.Fatalf("Unexpected request method: %s", req.Method)
			}
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				b.Fatalf("Unexpected error: %s", err)
			}
			req.Body.Close()
			if !bytes.Equal(body, fakeResponse) {
				b.Fatalf("Unexpected body %q. Expected %q", body, fakeResponse)
			}
			w.Header().Set("Content-Type", "text/plain")
			w.Write(body)
			registerServedRequest(b, ch)
		}),
	}
	benchmarkServer(b, s, clientsCount, requestsPerConn, postRequest)
	verifyRequestsServed(b, ch)
}

func registerServedRequest(b *testing.B, ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
		b.Fatalf("More than %d requests served", cap(ch))
	}
}

func verifyRequestsServed(b *testing.B, ch <-chan struct{}) {
	requestsServed := 0
	for len(ch) > 0 {
		<-ch
		requestsServed++
	}
	requestsSent := b.N
	for requestsServed < requestsSent {
		select {
		case <-ch:
			requestsServed++
		case <-time.After(100 * time.Millisecond):
			b.Fatalf("Unexpected number of requests served %d. Expected %d", requestsServed, requestsSent)
		}
	}
}

type realServer interface {
	Serve(ln net.Listener) error
}

type testServer struct {
	*Server
	Concurrency int
}

func (s *testServer) Serve(ln net.Listener) error {
	if s.Concurrency < runtime.NumCPU() {
		s.Concurrency = runtime.NumCPU()
	}
	return s.Server.ServeConcurrency(ln, s.Concurrency)
}

func benchmarkServer(b *testing.B, s realServer, clientsCount, requestsPerConn int, request string) {
	ln := newFakeListener(b.N, clientsCount, requestsPerConn, request)
	ch := make(chan struct{})
	go func() {
		s.Serve(ln)
		ch <- struct{}{}
	}()

	<-ln.done

	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		b.Fatalf("Server.Serve() didn't stop")
	}
}
