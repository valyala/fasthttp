package fasthttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkServerGet1ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, 1)
}

func BenchmarkServerGet2ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, 2)
}

func BenchmarkServerGet10ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, 10)
}

func BenchmarkServerGet10000ReqPerConn(b *testing.B) {
	benchmarkServerGet(b, 10000)
}

func BenchmarkNetHTTPServerGet1ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 1)
}

func BenchmarkNetHTTPServerGet2ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 2)
}

func BenchmarkNetHTTPServerGet10ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 10)
}

func BenchmarkNetHTTPServerGet10000ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerGet(b, 10000)
}

func BenchmarkServerPost1ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, 1)
}

func BenchmarkServerPost2ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, 2)
}

func BenchmarkServerPost10ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, 10)
}

func BenchmarkServerPost10000ReqPerConn(b *testing.B) {
	benchmarkServerPost(b, 10000)
}

func BenchmarkNetHTTPServerPost1ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, 1)
}

func BenchmarkNetHTTPServerPost2ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, 2)
}

func BenchmarkNetHTTPServerPost10ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, 10)
}

func BenchmarkNetHTTPServerPost10000ReqPerConn(b *testing.B) {
	benchmarkNetHTTPServerPost(b, 10000)
}

func BenchmarkServerTimeoutError(b *testing.B) {
	requestsPerConn := 10
	ch := make(chan struct{}, b.N)
	n := uint32(0)
	s := &Server{
		Handler: func(ctx *ServerCtx) {
			if atomic.AddUint32(&n, 1)&7 == 0 {
				ctx.TimeoutError("xxx", 321)
				go func() {
					ctx.Success("foobar", []byte("123"))
				}()
			} else {
				ctx.Success("foobar", []byte("123"))
			}
			registerServedRequest(b, ch)
		},
		Logger: log.New(ioutil.Discard, "", 0),
	}
	req := "GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n"
	requestsSent := benchmarkServer(b, &testServer{s}, requestsPerConn, req)
	verifyRequestsServed(b, requestsSent, ch)
}

type fakeServerConn struct {
	net.TCPConn

	addr net.Addr
	r    *io.PipeReader
	n    int
	nn   int
	next chan struct{}
	b    []byte
}

func (c *fakeServerConn) Read(b []byte) (int, error) {
	if c.nn == 0 {
		return 0, io.EOF
	}
	if len(b) > c.nn {
		b = b[:c.nn]
	}
	n, err := c.r.Read(b)
	c.nn -= n
	return n, err
}

func (c *fakeServerConn) Write(b []byte) (int, error) {
	return len(b), nil
}

func (c *fakeServerConn) RemoteAddr() net.Addr {
	return c.addr
}

func (c *fakeServerConn) Close() error {
	c.nn = c.n
	c.next <- struct{}{}
	return nil
}

type fakeListener struct {
	addr   net.TCPAddr
	stop   chan struct{}
	server fakeServerConn
	client *bufio.Writer
	w      *io.PipeWriter

	v interface{}
}

func (ln *fakeListener) Accept() (net.Conn, error) {
	select {
	case <-ln.stop:
		return nil, io.EOF
	case <-ln.server.next:
		return &ln.server, nil
	}
}

func (ln *fakeListener) Close() error {
	return nil
}

func (ln *fakeListener) Addr() net.Addr {
	return &ln.addr
}

func newFakeListener(bytesPerConn int) *fakeListener {
	r, w := io.Pipe()
	c := &fakeListener{
		stop:   make(chan struct{}, 1),
		w:      w,
		client: bufio.NewWriter(w),
	}
	c.server.addr = &c.addr
	c.server.r = r
	c.server.n = bytesPerConn
	c.server.nn = bytesPerConn
	c.server.b = make([]byte, 1024)
	c.server.next = make(chan struct{}, 1)
	c.server.next <- struct{}{}
	return c
}

var (
	fakeResponse = []byte("Hello, world!")
	getRequest   = "GET /foobar?baz HTTP/1.1\r\nHost: google.com\r\nUser-Agent: aaa/bbb/ccc/ddd/eee Firefox Chrome MSIE Opera\r\n" +
		"Referer: http://xxx.com/aaa?bbb=ccc\r\n\r\n"
	postRequest = fmt.Sprintf("POST /foobar?baz HTTP/1.1\r\nHost: google.com\r\nContent-Type: foo/bar\r\nContent-Length: %d\r\n"+
		"User-Agent: Opera Chrome MSIE Firefox and other/1.2.34\r\nReferer: http://google.com/aaaa/bbb/ccc\r\n\r\n%s",
		len(fakeResponse), fakeResponse)
)

func benchmarkServerGet(b *testing.B, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &Server{
		Handler: func(ctx *ServerCtx) {
			if !ctx.Request.Header.IsMethodGet() {
				b.Fatalf("Unexpected request method: %s", ctx.Request.Header.Method)
			}
			ctx.Success("text/plain", fakeResponse)
			registerServedRequest(b, ch)
		},
		Logger: log.New(ioutil.Discard, "", 0),
	}
	requestsSent := benchmarkServer(b, &testServer{s}, requestsPerConn, getRequest)
	verifyRequestsServed(b, requestsSent, ch)
}

func benchmarkNetHTTPServerGet(b *testing.B, requestsPerConn int) {
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
	requestsSent := benchmarkServer(b, s, requestsPerConn, getRequest)
	verifyRequestsServed(b, requestsSent, ch)
}

func benchmarkServerPost(b *testing.B, requestsPerConn int) {
	ch := make(chan struct{}, b.N)
	s := &Server{
		Handler: func(ctx *ServerCtx) {
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
		Logger: log.New(ioutil.Discard, "", 0),
	}
	requestsSent := benchmarkServer(b, &testServer{s}, requestsPerConn, postRequest)
	verifyRequestsServed(b, requestsSent, ch)
}

func benchmarkNetHTTPServerPost(b *testing.B, requestsPerConn int) {
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
	requestsSent := benchmarkServer(b, s, requestsPerConn, postRequest)
	verifyRequestsServed(b, requestsSent, ch)
}

func registerServedRequest(b *testing.B, ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
		b.Fatalf("More than %d requests served", cap(ch))
	}
}

func verifyRequestsServed(b *testing.B, requestsSent uint64, ch <-chan struct{}) {
	requestsServed := uint64(0)
	for len(ch) > 0 {
		<-ch
		requestsServed++
	}
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
}

func (s *testServer) Serve(ln net.Listener) error {
	return s.Server.ServeConcurrency(ln, runtime.NumCPU())
}

func benchmarkServer(b *testing.B, s realServer, requestsPerConn int, strRequest string) uint64 {
	request := []byte(strRequest)
	bytesPerConn := requestsPerConn * len(request)

	requestsSent := uint64(0)

	b.RunParallel(func(pb *testing.PB) {
		n := uint64(0)
		ln := newFakeListener(bytesPerConn)
		ch := make(chan struct{})
		go func() {
			s.Serve(ln)
			ch <- struct{}{}
		}()

		for pb.Next() {
			if _, err := ln.client.Write(request); err != nil {
				b.Fatalf("unexpected error when sending request to conn: %s", err)
			}
			n++
		}
		if err := ln.client.Flush(); err != nil {
			b.Fatalf("unexpected error when flushing data: %s", err)
		}

		ln.w.CloseWithError(io.EOF)
		ln.stop <- struct{}{}
		select {
		case <-ch:
		case <-time.After(500 * time.Millisecond):
			b.Fatalf("Server.Serve() didn't stop")
		}
		atomic.AddUint64(&requestsSent, n)
	})
	return requestsSent
}
