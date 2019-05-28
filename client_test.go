package fasthttp

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

func TestClientNilResp(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
		},
	}
	go s.Serve(ln)
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}
	req := AcquireRequest()
	req.Header.SetMethod(MethodGet)
	req.SetRequestURI("http://example.com")
	if err := c.Do(req, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.DoTimeout(req, nil, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestClientParseConn(t *testing.T) {
	network := "tcp"
	ln, _ := net.Listen(network, "127.0.0.1:0")
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			return
		},
	}
	go s.Serve(ln)
	host := ln.Addr().String()
	c := &Client{}
	req, res := AcquireRequest(), AcquireResponse()
	defer func() {
		ReleaseRequest(req)
		ReleaseResponse(res)
	}()
	req.SetRequestURI("http://" + host + "")
	c.Do(req, res)

	if res.RemoteAddr().Network() != network {
		t.Fatalf("req RemoteAddr parse network fail: %s, hope: %s", res.RemoteAddr().Network(), network)
	}
	if host != res.RemoteAddr().String() {
		t.Fatalf("req RemoteAddr parse addr fail: %s, hope: %s", res.RemoteAddr().String(), host)
	}

	if !regexp.MustCompile(`^127\.0\.0\.1:[0-9]{4,5}$`).MatchString(res.LocalAddr().String()) {
		t.Fatalf("res LocalAddr addr match fail: %s, hope match: %s", res.LocalAddr().String(), "^127.0.0.1:[0-9]{4,5}$")
	}

}

func TestClientPostArgs(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			body := ctx.Request.Body()
			if len(body) == 0 {
				return
			}
			ctx.Write(body)
		},
	}
	go s.Serve(ln)
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}
	req, res := AcquireRequest(), AcquireResponse()
	defer func() {
		ReleaseRequest(req)
		ReleaseResponse(res)
	}()
	args := req.PostArgs()
	args.Add("addhttp2", "support")
	args.Add("fast", "http")
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI("http://make.fasthttp.great?again")
	err := c.Do(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body()) == 0 {
		t.Fatal("cannot set args as body")
	}
}

func TestClientRedirectSameSchema(t *testing.T) {

	listenHTTPS1 := testClientRedirectListener(t, true)
	defer listenHTTPS1.Close()

	listenHTTPS2 := testClientRedirectListener(t, true)
	defer listenHTTPS2.Close()

	sHTTPS1 := testClientRedirectChangingSchemaServer(t, listenHTTPS1, listenHTTPS1, true)
	defer sHTTPS1.Stop()

	sHTTPS2 := testClientRedirectChangingSchemaServer(t, listenHTTPS2, listenHTTPS2, false)
	defer sHTTPS2.Stop()

	destURL := fmt.Sprintf("https://%s/baz", listenHTTPS1.Addr().String())

	urlParsed, err := url.Parse(destURL)
	if err != nil {
		fmt.Println(err)
		return
	}

	reqClient := &HostClient{
		IsTLS: true,
		Addr:  urlParsed.Host,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	statusCode, _, err := reqClient.GetTimeout(nil, destURL, 4000*time.Millisecond)
	if err != nil {
		t.Fatalf("HostClient error: %s", err)
		return
	}

	if statusCode != 200 {
		t.Fatalf("HostClient error code response %d", statusCode)
		return
	}

}

func TestClientRedirectChangingSchemaHttp2Https(t *testing.T) {

	listenHTTPS := testClientRedirectListener(t, true)
	defer listenHTTPS.Close()

	listenHTTP := testClientRedirectListener(t, false)
	defer listenHTTP.Close()

	sHTTPS := testClientRedirectChangingSchemaServer(t, listenHTTPS, listenHTTP, true)
	defer sHTTPS.Stop()

	sHTTP := testClientRedirectChangingSchemaServer(t, listenHTTPS, listenHTTP, false)
	defer sHTTP.Stop()

	destURL := fmt.Sprintf("http://%s/baz", listenHTTP.Addr().String())

	urlParsed, err := url.Parse(destURL)
	if err != nil {
		fmt.Println(err)
		return
	}

	reqClient := &HostClient{
		Addr: urlParsed.Host,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	statusCode, _, err := reqClient.GetTimeout(nil, destURL, 4000*time.Millisecond)
	if err != nil {
		t.Fatalf("HostClient error: %s", err)
		return
	}

	if statusCode != 200 {
		t.Fatalf("HostClient error code response %d", statusCode)
		return
	}

}

func testClientRedirectListener(t *testing.T, isTLS bool) net.Listener {

	var ln net.Listener
	var err error
	var tlsConfig *tls.Config

	if isTLS {
		certFile := "./ssl-cert-snakeoil.pem"
		keyFile := "./ssl-cert-snakeoil.key"
		cert, err1 := tls.LoadX509KeyPair(certFile, keyFile)
		if err1 != nil {
			t.Fatalf("Cannot load TLS certificate: %s", err1)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		ln, err = tls.Listen("tcp", "localhost:0", tlsConfig)
	} else {
		ln, err = net.Listen("tcp", "localhost:0")
	}

	if err != nil {
		t.Fatalf("cannot listen isTLS %v: %s", isTLS, err)
	}

	return ln
}

func testClientRedirectChangingSchemaServer(t *testing.T, https, http net.Listener, isTLS bool) *testEchoServer {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if ctx.IsTLS() {
				ctx.SetStatusCode(200)
			} else {
				ctx.Redirect(fmt.Sprintf("https://%s/baz", https.Addr().String()), 301)
			}
		},
	}

	var ln net.Listener
	if isTLS {
		ln = https
	} else {
		ln = http
	}

	ch := make(chan struct{})
	go func() {
		err := s.Serve(ln)
		if err != nil {
			t.Fatalf("unexpected error returned from Serve(): %s", err)
		}
		close(ch)
	}()
	return &testEchoServer{
		s:  s,
		ln: ln,
		ch: ch,
		t:  t,
	}
}

func TestClientHeaderCase(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("HTTP/1.1 200 OK\r\n" +
			"content-type: text/plain\r\n" +
			"transfer-encoding: chunked\r\n\r\n" +
			"24\r\nThis is the data in the first chunk \r\n" +
			"1B\r\nand this is the second one \r\n" +
			"0\r\n\r\n",
		))
	}()

	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		ReadTimeout: time.Millisecond * 10,

		// Even without name normalizing we should parse headers correctly.
		DisableHeaderNamesNormalizing: true,
	}

	code, body, err := c.Get(nil, "http://example.com")
	if err != nil {
		t.Error(err)
	} else if code != 200 {
		t.Errorf("expected status code 200 got %d", code)
	} else if string(body) != "This is the data in the first chunk and this is the second one " {
		t.Errorf("wrong body: %q", body)
	}
}

func TestClientReadTimeout(t *testing.T) {
	// This test is rather slow and increase the total test time
	// from 2.5 seconds to 6.5 seconds.
	if testing.Short() {
		t.Skip("skipping test in short mode")
	}

	ln := fasthttputil.NewInmemoryListener()

	timeout := false
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if timeout {
				time.Sleep(time.Minute)
			} else {
				timeout = true
			}
		},
		Logger: &customLogger{}, // Don't print closed pipe errors.
	}
	go s.Serve(ln)

	c := &HostClient{
		ReadTimeout:               time.Second * 4,
		MaxIdemponentCallAttempts: 1,
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	req := AcquireRequest()
	res := AcquireResponse()

	req.SetRequestURI("http://localhost")

	// Setting Connection: Close will make the connection be
	// returned to the pool.
	req.SetConnectionClose()

	if err := c.Do(req, res); err != nil {
		t.Fatal(err)
	}

	ReleaseRequest(req)
	ReleaseResponse(res)

	done := make(chan struct{})
	go func() {
		req := AcquireRequest()
		res := AcquireResponse()

		req.SetRequestURI("http://localhost")
		req.SetConnectionClose()

		c.Do(req, res)

		ReleaseRequest(req)
		ReleaseResponse(res)
		close(done)
	}()

	select {
	case <-done:
		// This shouldn't take longer than the timeout times the number of requests it is going to try to do.
		// Give it 2 seconds extra seconds just to be sure.
	case <-time.After(c.ReadTimeout*time.Duration(c.MaxIdemponentCallAttempts) + time.Second*2):
		t.Fatal("Client.ReadTimeout didn't work")
	}
}

func TestClientDefaultUserAgent(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	userAgentSeen := ""
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			userAgentSeen = string(ctx.UserAgent())
		},
	}
	go s.Serve(ln)

	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}
	req := AcquireRequest()
	res := AcquireResponse()

	req.SetRequestURI("http://example.com")

	err := c.Do(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if userAgentSeen != string(defaultUserAgent) {
		t.Fatalf("User-Agent defers %q != %q", userAgentSeen, defaultUserAgent)
	}
}

func TestClientSetUserAgent(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	userAgentSeen := ""
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			userAgentSeen = string(ctx.UserAgent())
		},
	}
	go s.Serve(ln)

	userAgent := "I'm not fasthttp"
	c := &Client{
		Name: userAgent,
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}
	req := AcquireRequest()
	res := AcquireResponse()

	req.SetRequestURI("http://example.com")

	err := c.Do(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if userAgentSeen != userAgent {
		t.Fatalf("User-Agent defers %q != %q", userAgentSeen, userAgent)
	}
}

func TestClientNoUserAgent(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	userAgentSeen := ""
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			userAgentSeen = string(ctx.UserAgent())
		},
	}
	go s.Serve(ln)

	c := &Client{
		NoDefaultUserAgentHeader: true,
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}
	req := AcquireRequest()
	res := AcquireResponse()

	req.SetRequestURI("http://example.com")

	err := c.Do(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if userAgentSeen != "" {
		t.Fatalf("User-Agent wrong %q != %q", userAgentSeen, "")
	}
}

func TestClientDoWithCustomHeaders(t *testing.T) {
	// make sure that the client sends all the request headers and body.
	ln := fasthttputil.NewInmemoryListener()
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	uri := "/foo/bar/baz?a=b&cd=12"
	headers := map[string]string{
		"Foo":          "bar",
		"Host":         "xxx.com",
		"Content-Type": "asdfsdf",
		"a-b-c-d-f":    "",
	}
	body := "request body"

	ch := make(chan error)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- fmt.Errorf("cannot accept client connection: %s", err)
			return
		}
		br := bufio.NewReader(conn)

		var req Request
		if err = req.Read(br); err != nil {
			ch <- fmt.Errorf("cannot read client request: %s", err)
			return
		}
		if string(req.Header.Method()) != MethodPost {
			ch <- fmt.Errorf("unexpected request method: %q. Expecting %q", req.Header.Method(), MethodPost)
			return
		}
		reqURI := req.RequestURI()
		if string(reqURI) != uri {
			ch <- fmt.Errorf("unexpected request uri: %q. Expecting %q", reqURI, uri)
			return
		}
		for k, v := range headers {
			hv := req.Header.Peek(k)
			if string(hv) != v {
				ch <- fmt.Errorf("unexpected value for header %q: %q. Expecting %q", k, hv, v)
				return
			}
		}
		cl := req.Header.ContentLength()
		if cl != len(body) {
			ch <- fmt.Errorf("unexpected content-length %d. Expecting %d", cl, len(body))
			return
		}
		reqBody := req.Body()
		if string(reqBody) != body {
			ch <- fmt.Errorf("unexpected request body: %q. Expecting %q", reqBody, body)
			return
		}

		var resp Response
		bw := bufio.NewWriter(conn)
		if err = resp.Write(bw); err != nil {
			ch <- fmt.Errorf("cannot send response: %s", err)
			return
		}
		if err = bw.Flush(); err != nil {
			ch <- fmt.Errorf("cannot flush response: %s", err)
			return
		}

		ch <- nil
	}()

	var req Request
	req.Header.SetMethod(MethodPost)
	req.SetRequestURI(uri)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.SetBodyString(body)

	var resp Response

	err := c.DoTimeout(&req, &resp, time.Second)
	if err != nil {
		t.Fatalf("error when doing request: %s", err)
	}

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout")
	}
}

func TestPipelineClientDoSerial(t *testing.T) {
	testPipelineClientDoConcurrent(t, 1, 0, 0)
}

func TestPipelineClientDoConcurrent(t *testing.T) {
	testPipelineClientDoConcurrent(t, 10, 0, 1)
}

func TestPipelineClientDoBatchDelayConcurrent(t *testing.T) {
	testPipelineClientDoConcurrent(t, 10, 5*time.Millisecond, 1)
}

func TestPipelineClientDoBatchDelayConcurrentMultiConn(t *testing.T) {
	testPipelineClientDoConcurrent(t, 10, 5*time.Millisecond, 3)
}

func testPipelineClientDoConcurrent(t *testing.T, concurrency int, maxBatchDelay time.Duration, maxConns int) {
	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("OK")
		},
	}

	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &PipelineClient{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		MaxConns:           maxConns,
		MaxPendingRequests: concurrency,
		MaxBatchDelay:      maxBatchDelay,
		Logger:             &customLogger{},
	}

	clientStopCh := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			testPipelineClientDo(t, c)
			clientStopCh <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-clientStopCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout")
		}
	}

	if c.PendingRequests() != 0 {
		t.Fatalf("unexpected number of pending requests: %d. Expecting zero", c.PendingRequests())
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func testPipelineClientDo(t *testing.T, c *PipelineClient) {
	var err error
	req := AcquireRequest()
	req.SetRequestURI("http://foobar/baz")
	resp := AcquireResponse()
	for i := 0; i < 10; i++ {
		if i&1 == 0 {
			err = c.DoTimeout(req, resp, time.Second)
		} else {
			err = c.Do(req, resp)
		}
		if err != nil {
			if err == ErrPipelineOverflow {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			t.Fatalf("unexpected error on iteration %d: %s", i, err)
		}
		if resp.StatusCode() != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
		}
		body := string(resp.Body())
		if body != "OK" {
			t.Fatalf("unexpected body: %q. Expecting %q", body, "OK")
		}

		// sleep for a while, so the connection to the host may expire.
		if i%5 == 0 {
			time.Sleep(30 * time.Millisecond)
		}
	}
	ReleaseRequest(req)
	ReleaseResponse(resp)
}

func TestClientDoTimeoutDisableNormalizing(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Response.Header.Set("foo-BAR", "baz")
		},
		DisableHeaderNamesNormalizing: true,
	}

	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		DisableHeaderNamesNormalizing: true,
	}

	var req Request
	req.SetRequestURI("http://aaaai.com/bsdf?sddfsd")
	var resp Response
	for i := 0; i < 5; i++ {
		if err := c.DoTimeout(&req, &resp, time.Second); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		hv := resp.Header.Peek("foo-BAR")
		if string(hv) != "baz" {
			t.Fatalf("unexpected header value: %q. Expecting %q", hv, "baz")
		}
		hv = resp.Header.Peek("Foo-Bar")
		if len(hv) > 0 {
			t.Fatalf("unexpected non-empty header value %q", hv)
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func TestHostClientPendingRequests(t *testing.T) {
	const concurrency = 10
	doneCh := make(chan struct{})
	readyCh := make(chan struct{}, concurrency)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			readyCh <- struct{}{}
			<-doneCh
		},
	}
	ln := fasthttputil.NewInmemoryListener()
	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &HostClient{
		Addr: "foobar",
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	pendingRequests := c.PendingRequests()
	if pendingRequests != 0 {
		t.Fatalf("non-zero pendingRequests: %d", pendingRequests)
	}

	resultCh := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			req := AcquireRequest()
			req.SetRequestURI("http://foobar/baz")
			resp := AcquireResponse()

			if err := c.DoTimeout(req, resp, 10*time.Second); err != nil {
				resultCh <- fmt.Errorf("unexpected error: %s", err)
				return
			}

			if resp.StatusCode() != StatusOK {
				resultCh <- fmt.Errorf("unexpected status code %d. Expecting %d", resp.StatusCode(), StatusOK)
				return
			}
			resultCh <- nil
		}()
	}

	// wait while all the requests reach server
	for i := 0; i < concurrency; i++ {
		select {
		case <-readyCh:
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}

	pendingRequests = c.PendingRequests()
	if pendingRequests != concurrency {
		t.Fatalf("unexpected pendingRequests: %d. Expecting %d", pendingRequests, concurrency)
	}

	// unblock request handlers on the server and wait until all the requests are finished.
	close(doneCh)
	for i := 0; i < concurrency; i++ {
		select {
		case err := <-resultCh:
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout")
		}
	}

	pendingRequests = c.PendingRequests()
	if pendingRequests != 0 {
		t.Fatalf("non-zero pendingRequests: %d", pendingRequests)
	}

	// stop the server
	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}
}

func TestHostClientMaxConnsWithDeadline(t *testing.T) {
	var (
		emptyBodyCount uint8
		ln             = fasthttputil.NewInmemoryListener()
		timeout        = 50 * time.Millisecond
		wg             sync.WaitGroup
	)

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if len(ctx.PostBody()) == 0 {
				emptyBodyCount++
			}

			ctx.WriteString("foo")
		},
	}
	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &HostClient{
		Addr: "foobar",
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		MaxConns: 1,
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req := AcquireRequest()
			req.SetRequestURI("http://foobar/baz")
			req.Header.SetMethod(MethodPost)
			req.SetBodyString("bar")
			resp := AcquireResponse()

			for {
				if err := c.DoDeadline(req, resp, time.Now().Add(timeout)); err != nil {
					if err == ErrNoFreeConns {
						time.Sleep(time.Millisecond)
						continue
					}
					t.Fatalf("unexpected error: %s", err)
				}
				break
			}

			if resp.StatusCode() != StatusOK {
				t.Fatalf("unexpected status code %d. Expecting %d", resp.StatusCode(), StatusOK)
			}

			body := resp.Body()
			if string(body) != "foo" {
				t.Fatalf("unexpected body %q. Expecting %q", body, "abcd")
			}
		}()
	}
	wg.Wait()

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	if emptyBodyCount > 0 {
		t.Fatalf("at least one request body was empty")
	}
}

func TestHostClientMaxConnDuration(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	connectionCloseCount := uint32(0)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("abcd")
			if ctx.Request.ConnectionClose() {
				atomic.AddUint32(&connectionCloseCount, 1)
			}
		},
	}
	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &HostClient{
		Addr: "foobar",
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		MaxConnDuration: 10 * time.Millisecond,
	}

	for i := 0; i < 5; i++ {
		statusCode, body, err := c.Get(nil, "http://aaaa.com/bbb/cc")
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code %d. Expecting %d", statusCode, StatusOK)
		}
		if string(body) != "abcd" {
			t.Fatalf("unexpected body %q. Expecting %q", body, "abcd")
		}
		time.Sleep(c.MaxConnDuration)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	if connectionCloseCount == 0 {
		t.Fatalf("expecting at least one 'Connection: close' request header")
	}
}

func TestHostClientMultipleAddrs(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Write(ctx.Host())
			ctx.SetConnectionClose()
		},
	}
	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	dialsCount := make(map[string]int)
	c := &HostClient{
		Addr: "foo,bar,baz",
		Dial: func(addr string) (net.Conn, error) {
			dialsCount[addr]++
			return ln.Dial()
		},
	}

	for i := 0; i < 9; i++ {
		statusCode, body, err := c.Get(nil, "http://foobar/baz/aaa?bbb=ddd")
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code %d. Expecting %d", statusCode, StatusOK)
		}
		if string(body) != "foobar" {
			t.Fatalf("unexpected body %q. Expecting %q", body, "foobar")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-serverStopCh:
	case <-time.After(time.Second):
		t.Fatalf("timeout")
	}

	if len(dialsCount) != 3 {
		t.Fatalf("unexpected dialsCount size %d. Expecting 3", len(dialsCount))
	}
	for _, k := range []string{"foo", "bar", "baz"} {
		if dialsCount[k] != 3 {
			t.Fatalf("unexpected dialsCount for %q. Expecting 3", k)
		}
	}
}

func TestClientFollowRedirects(t *testing.T) {
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			switch string(ctx.Path()) {
			case "/foo":
				u := ctx.URI()
				u.Update("/xy?z=wer")
				ctx.Redirect(u.String(), StatusFound)
			case "/xy":
				u := ctx.URI()
				u.Update("/bar")
				ctx.Redirect(u.String(), StatusFound)
			default:
				ctx.Success("text/plain", ctx.Path())
			}
		},
	}
	ln := fasthttputil.NewInmemoryListener()

	serverStopCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		close(serverStopCh)
	}()

	c := &HostClient{
		Addr: "xxx",
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	for i := 0; i < 10; i++ {
		statusCode, body, err := c.GetTimeout(nil, "http://xxx/foo", time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d", statusCode)
		}
		if string(body) != "/bar" {
			t.Fatalf("unexpected response %q. Expecting %q", body, "/bar")
		}
	}

	for i := 0; i < 10; i++ {
		statusCode, body, err := c.Get(nil, "http://xxx/aaab/sss")
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d", statusCode)
		}
		if string(body) != "/aaab/sss" {
			t.Fatalf("unexpected response %q. Expecting %q", body, "/aaab/sss")
		}
	}
}

func TestClientGetTimeoutSuccess(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	testClientGetTimeoutSuccess(t, &defaultClient, "http://"+s.Addr(), 100)
}

func TestClientGetTimeoutSuccessConcurrent(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientGetTimeoutSuccess(t, &defaultClient, "http://"+s.Addr(), 100)
		}()
	}
	wg.Wait()
}

func TestClientDoTimeoutSuccess(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	testClientDoTimeoutSuccess(t, &defaultClient, "http://"+s.Addr(), 100)
}

func TestClientDoTimeoutSuccessConcurrent(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientDoTimeoutSuccess(t, &defaultClient, "http://"+s.Addr(), 100)
		}()
	}
	wg.Wait()
}

func TestClientGetTimeoutError(t *testing.T) {
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return &readTimeoutConn{t: time.Second}, nil
		},
	}

	testClientGetTimeoutError(t, c, 100)
}

func TestClientGetTimeoutErrorConcurrent(t *testing.T) {
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return &readTimeoutConn{t: time.Second}, nil
		},
		MaxConnsPerHost: 1000,
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientGetTimeoutError(t, c, 100)
		}()
	}
	wg.Wait()
}

func TestClientDoTimeoutError(t *testing.T) {
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return &readTimeoutConn{t: time.Second}, nil
		},
	}

	testClientDoTimeoutError(t, c, 100)
}

func TestClientDoTimeoutErrorConcurrent(t *testing.T) {
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			return &readTimeoutConn{t: time.Second}, nil
		},
		MaxConnsPerHost: 1000,
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientDoTimeoutError(t, c, 100)
		}()
	}
	wg.Wait()
}

func testClientDoTimeoutError(t *testing.T, c *Client, n int) {
	var req Request
	var resp Response
	req.SetRequestURI("http://foobar.com/baz")
	for i := 0; i < n; i++ {
		err := c.DoTimeout(&req, &resp, time.Millisecond)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %s. Expecting %s", err, ErrTimeout)
		}
	}
}

func testClientGetTimeoutError(t *testing.T, c *Client, n int) {
	buf := make([]byte, 10)
	for i := 0; i < n; i++ {
		statusCode, body, err := c.GetTimeout(buf, "http://foobar.com/baz", time.Millisecond)
		if err == nil {
			t.Fatalf("expecting error")
		}
		if err != ErrTimeout {
			t.Fatalf("unexpected error: %s. Expecting %s", err, ErrTimeout)
		}
		if statusCode != 0 {
			t.Fatalf("unexpected statusCode=%d. Expecting %d", statusCode, 0)
		}
		if body == nil {
			t.Fatalf("body must be non-nil")
		}
	}
}

type readTimeoutConn struct {
	net.Conn
	t time.Duration
}

func (r *readTimeoutConn) Read(p []byte) (int, error) {
	time.Sleep(r.t)
	return 0, io.EOF
}

func (r *readTimeoutConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (r *readTimeoutConn) Close() error {
	return nil
}

func (r *readTimeoutConn) LocalAddr() net.Addr {
	return nil
}

func (r *readTimeoutConn) RemoteAddr() net.Addr {
	return nil
}

func TestClientNonIdempotentRetry(t *testing.T) {
	dialsCount := 0
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			dialsCount++
			switch dialsCount {
			case 1, 2:
				return &readErrorConn{}, nil
			case 3:
				return &singleReadConn{
					s: "HTTP/1.1 345 OK\r\nContent-Type: foobar\r\nContent-Length: 7\r\n\r\n0123456",
				}, nil
			default:
				t.Fatalf("unexpected number of dials: %d", dialsCount)
			}
			panic("unreachable")
		},
	}

	// This POST must succeed, since the readErrorConn closes
	// the connection before sending any response.
	// So the client must retry non-idempotent request.
	dialsCount = 0
	statusCode, body, err := c.Post(nil, "http://foobar/a/b", nil)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if statusCode != 345 {
		t.Fatalf("unexpected status code: %d. Expecting 345", statusCode)
	}
	if string(body) != "0123456" {
		t.Fatalf("unexpected body: %q. Expecting %q", body, "0123456")
	}

	// Verify that idempotent GET succeeds.
	dialsCount = 0
	statusCode, body, err = c.Get(nil, "http://foobar/a/b")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if statusCode != 345 {
		t.Fatalf("unexpected status code: %d. Expecting 345", statusCode)
	}
	if string(body) != "0123456" {
		t.Fatalf("unexpected body: %q. Expecting %q", body, "0123456")
	}
}

func TestClientIdempotentRequest(t *testing.T) {
	dialsCount := 0
	c := &Client{
		Dial: func(addr string) (net.Conn, error) {
			dialsCount++
			switch dialsCount {
			case 1:
				return &singleReadConn{
					s: "invalid response",
				}, nil
			case 2:
				return &writeErrorConn{}, nil
			case 3:
				return &readErrorConn{}, nil
			case 4:
				return &singleReadConn{
					s: "HTTP/1.1 345 OK\r\nContent-Type: foobar\r\nContent-Length: 7\r\n\r\n0123456",
				}, nil
			default:
				t.Fatalf("unexpected number of dials: %d", dialsCount)
			}
			panic("unreachable")
		},
	}

	// idempotent GET must succeed.
	statusCode, body, err := c.Get(nil, "http://foobar/a/b")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if statusCode != 345 {
		t.Fatalf("unexpected status code: %d. Expecting 345", statusCode)
	}
	if string(body) != "0123456" {
		t.Fatalf("unexpected body: %q. Expecting %q", body, "0123456")
	}

	var args Args

	// non-idempotent POST must fail on incorrect singleReadConn
	dialsCount = 0
	_, _, err = c.Post(nil, "http://foobar/a/b", &args)
	if err == nil {
		t.Fatalf("expecting error")
	}

	// non-idempotent POST must fail on incorrect singleReadConn
	dialsCount = 0
	_, _, err = c.Post(nil, "http://foobar/a/b", nil)
	if err == nil {
		t.Fatalf("expecting error")
	}
}

type writeErrorConn struct {
	net.Conn
}

func (w *writeErrorConn) Write(p []byte) (int, error) {
	return 1, fmt.Errorf("error")
}

func (w *writeErrorConn) Close() error {
	return nil
}

func (r *writeErrorConn) LocalAddr() net.Addr {
	return nil
}

func (r *writeErrorConn) RemoteAddr() net.Addr {
	return nil
}

type readErrorConn struct {
	net.Conn
}

func (r *readErrorConn) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("error")
}

func (r *readErrorConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (r *readErrorConn) Close() error {
	return nil
}

func (r *readErrorConn) LocalAddr() net.Addr {
	return nil
}

func (r *readErrorConn) RemoteAddr() net.Addr {
	return nil
}

type singleReadConn struct {
	net.Conn
	s string
	n int
}

func (r *singleReadConn) Read(p []byte) (int, error) {
	if len(r.s) == r.n {
		return 0, io.EOF
	}
	n := copy(p, []byte(r.s[r.n:]))
	r.n += n
	return n, nil
}

func (r *singleReadConn) Write(p []byte) (int, error) {
	return len(p), nil
}

func (r *singleReadConn) Close() error {
	return nil
}

func (r *singleReadConn) LocalAddr() net.Addr {
	return nil
}

func (r *singleReadConn) RemoteAddr() net.Addr {
	return nil
}

func TestClientHTTPSInvalidServerName(t *testing.T) {
	sHTTPS := startEchoServerTLS(t, "tcp", "127.0.0.1:")
	defer sHTTPS.Stop()

	var c Client

	for i := 0; i < 10; i++ {
		_, _, err := c.GetTimeout(nil, "https://"+sHTTPS.Addr(), time.Second)
		if err == nil {
			t.Fatalf("expecting TLS error")
		}
	}
}

func TestClientHTTPSConcurrent(t *testing.T) {
	sHTTP := startEchoServer(t, "tcp", "127.0.0.1:")
	defer sHTTP.Stop()

	sHTTPS := startEchoServerTLS(t, "tcp", "127.0.0.1:")
	defer sHTTPS.Stop()

	c := &Client{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		addr := "http://" + sHTTP.Addr()
		if i&1 != 0 {
			addr = "https://" + sHTTPS.Addr()
		}
		go func() {
			defer wg.Done()
			testClientGet(t, c, addr, 20)
			testClientPost(t, c, addr, 10)
		}()
	}
	wg.Wait()
}

func TestClientManyServers(t *testing.T) {
	var addrs []string
	for i := 0; i < 10; i++ {
		s := startEchoServer(t, "tcp", "127.0.0.1:")
		defer s.Stop()
		addrs = append(addrs, s.Addr())
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		addr := "http://" + addrs[i]
		go func() {
			defer wg.Done()
			testClientGet(t, &defaultClient, addr, 20)
			testClientPost(t, &defaultClient, addr, 10)
		}()
	}
	wg.Wait()
}

func TestClientGet(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	testClientGet(t, &defaultClient, "http://"+s.Addr(), 100)
}

func TestClientPost(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	testClientPost(t, &defaultClient, "http://"+s.Addr(), 100)
}

func TestClientConcurrent(t *testing.T) {
	s := startEchoServer(t, "tcp", "127.0.0.1:")
	defer s.Stop()

	addr := "http://" + s.Addr()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testClientGet(t, &defaultClient, addr, 30)
			testClientPost(t, &defaultClient, addr, 10)
		}()
	}
	wg.Wait()
}

func skipIfNotUnix(tb testing.TB) {
	switch runtime.GOOS {
	case "android", "nacl", "plan9", "windows":
		tb.Skipf("%s does not support unix sockets", runtime.GOOS)
	}
	if runtime.GOOS == "darwin" && (runtime.GOARCH == "arm" || runtime.GOARCH == "arm64") {
		tb.Skip("iOS does not support unix, unixgram")
	}
}

func TestHostClientGet(t *testing.T) {
	skipIfNotUnix(t)
	addr := "TestHostClientGet.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	testHostClientGet(t, c, 100)
}

func TestHostClientPost(t *testing.T) {
	skipIfNotUnix(t)
	addr := "./TestHostClientPost.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	testHostClientPost(t, c, 100)
}

func TestHostClientConcurrent(t *testing.T) {
	skipIfNotUnix(t)
	addr := "./TestHostClientConcurrent.unix"
	s := startEchoServer(t, "unix", addr)
	defer s.Stop()
	c := createEchoClient(t, "unix", addr)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			testHostClientGet(t, c, 30)
			testHostClientPost(t, c, 10)
		}()
	}
	wg.Wait()
}

func testClientGet(t *testing.T, c clientGetter, addr string, n int) {
	var buf []byte
	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("%s/foo/%d?bar=baz", addr, i)
		statusCode, body, err := c.Get(buf, uri)
		buf = body
		if err != nil {
			t.Fatalf("unexpected error when doing http request: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
		}
		resultURI := string(body)
		if resultURI != uri {
			t.Fatalf("unexpected uri %q. Expecting %q", resultURI, uri)
		}
	}
}

func testClientDoTimeoutSuccess(t *testing.T, c *Client, addr string, n int) {
	var req Request
	var resp Response

	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("%s/foo/%d?bar=baz", addr, i)
		req.SetRequestURI(uri)
		if err := c.DoTimeout(&req, &resp, time.Second); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
		}
		resultURI := string(resp.Body())
		if strings.HasPrefix(uri, "https") {
			resultURI = uri[:5] + resultURI[4:]
		}
		if resultURI != uri {
			t.Fatalf("unexpected uri %q. Expecting %q", resultURI, uri)
		}
	}
}

func testClientGetTimeoutSuccess(t *testing.T, c *Client, addr string, n int) {
	var buf []byte
	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("%s/foo/%d?bar=baz", addr, i)
		statusCode, body, err := c.GetTimeout(buf, uri, time.Second)
		buf = body
		if err != nil {
			t.Fatalf("unexpected error when doing http request: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
		}
		resultURI := string(body)
		if strings.HasPrefix(uri, "https") {
			resultURI = uri[:5] + resultURI[4:]
		}
		if resultURI != uri {
			t.Fatalf("unexpected uri %q. Expecting %q", resultURI, uri)
		}
	}
}

func testClientPost(t *testing.T, c clientPoster, addr string, n int) {
	var buf []byte
	var args Args
	for i := 0; i < n; i++ {
		uri := fmt.Sprintf("%s/foo/%d?bar=baz", addr, i)
		args.Set("xx", fmt.Sprintf("yy%d", i))
		args.Set("zzz", fmt.Sprintf("qwe_%d", i))
		argsS := args.String()
		statusCode, body, err := c.Post(buf, uri, &args)
		buf = body
		if err != nil {
			t.Fatalf("unexpected error when doing http request: %s", err)
		}
		if statusCode != StatusOK {
			t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusOK)
		}
		s := string(body)
		if s != argsS {
			t.Fatalf("unexpected response %q. Expecting %q", s, argsS)
		}
	}
}

func testHostClientGet(t *testing.T, c *HostClient, n int) {
	testClientGet(t, c, "http://google.com", n)
}

func testHostClientPost(t *testing.T, c *HostClient, n int) {
	testClientPost(t, c, "http://post-host.com", n)
}

type clientPoster interface {
	Post(dst []byte, uri string, postArgs *Args) (int, []byte, error)
}

type clientGetter interface {
	Get(dst []byte, uri string) (int, []byte, error)
}

func createEchoClient(t *testing.T, network, addr string) *HostClient {
	return &HostClient{
		Addr: addr,
		Dial: func(addr string) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
}

type testEchoServer struct {
	s  *Server
	ln net.Listener
	ch chan struct{}
	t  *testing.T
}

func (s *testEchoServer) Stop() {
	s.ln.Close()
	select {
	case <-s.ch:
	case <-time.After(time.Second):
		s.t.Fatalf("timeout when waiting for server close")
	}
}

func (s *testEchoServer) Addr() string {
	return s.ln.Addr().String()
}

func startEchoServerTLS(t *testing.T, network, addr string) *testEchoServer {
	return startEchoServerExt(t, network, addr, true)
}

func startEchoServer(t *testing.T, network, addr string) *testEchoServer {
	return startEchoServerExt(t, network, addr, false)
}

func startEchoServerExt(t *testing.T, network, addr string, isTLS bool) *testEchoServer {
	if network == "unix" {
		os.Remove(addr)
	}
	var ln net.Listener
	var err error
	if isTLS {
		certFile := "./ssl-cert-snakeoil.pem"
		keyFile := "./ssl-cert-snakeoil.key"
		cert, err1 := tls.LoadX509KeyPair(certFile, keyFile)
		if err1 != nil {
			t.Fatalf("Cannot load TLS certificate: %s", err1)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		ln, err = tls.Listen(network, addr, tlsConfig)
	} else {
		ln, err = net.Listen(network, addr)
	}
	if err != nil {
		t.Fatalf("cannot listen %q: %s", addr, err)
	}

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if ctx.IsGet() {
				ctx.Success("text/plain", ctx.URI().FullURI())
			} else if ctx.IsPost() {
				ctx.PostArgs().WriteTo(ctx)
			}
		},
		Logger: &customLogger{}, // Ignore log output.
	}
	ch := make(chan struct{})
	go func() {
		err := s.Serve(ln)
		if err != nil {
			t.Fatalf("unexpected error returned from Serve(): %s", err)
		}
		close(ch)
	}()
	return &testEchoServer{
		s:  s,
		ln: ln,
		ch: ch,
		t:  t,
	}
}
