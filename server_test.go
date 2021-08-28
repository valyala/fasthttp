package fasthttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp/fasthttputil"
)

// Make sure RequestCtx implements context.Context
var _ context.Context = &RequestCtx{}

func TestServerCRNLAfterPost_Pipeline(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
		},
		Logger: &testLogger{},
	}

	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer c.Close()
	if _, err = c.Write([]byte("POST / HTTP/1.1\r\nHost: golang.org\r\nContent-Length: 3\r\n\r\nABC" +
		"\r\n\r\n" + // <-- this stuff is bogus, but we'll ignore it
		"GET / HTTP/1.1\r\nHost: golang.org\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
}

func TestServerCRNLAfterPost(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
		},
		Logger:      &testLogger{},
		ReadTimeout: time.Millisecond * 100,
	}

	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	defer c.Close()
	if _, err = c.Write([]byte("POST / HTTP/1.1\r\nHost: golang.org\r\nContent-Length: 3\r\n\r\nABC" +
		"\r\n\r\n", // <-- this stuff is bogus, but we'll ignore it
	)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
	if err := resp.Read(br); err == nil {
		t.Fatal("expected error") // We didn't send a request so we should get an error here.
	}
}

func TestServerPipelineFlush(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
		},
	}
	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if _, err = c.Write([]byte("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	// Write a partial request.
	if _, err = c.Write([]byte("GET /foo1 HTTP/1.1\r\nHost: ")); err != nil {
		t.Fatal(err)
	}
	go func() {
		// Wait for 200ms to finish the request
		time.Sleep(time.Millisecond * 200)

		if _, err = c.Write([]byte("google.com\r\n\r\n")); err != nil {
			t.Error(err)
		}
	}()

	start := time.Now()
	br := bufio.NewReader(c)
	var resp Response

	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	// Since the second request takes 200ms to finish we expect the first one to be flushed earlier.
	d := time.Since(start)
	if d > time.Millisecond*100 {
		t.Fatalf("had to wait for %v", d)
	}

	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}
}

func TestServerInvalidHeader(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if ctx.Request.Header.Peek("Foo") != nil || ctx.Request.Header.Peek("Foo ") != nil {
				t.Error("expected Foo header")
			}
		},
		Logger: &testLogger{},
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if _, err = c.Write([]byte("POST /foo HTTP/1.1\r\nHost: gle.com\r\nFoo : bar\r\nContent-Length: 5\r\n\r\n12345")); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(c)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusBadRequest {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusBadRequest)
	}

	c, err = ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if _, err = c.Write([]byte("GET /foo HTTP/1.1\r\nHost: gle.com\r\nFoo : bar\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	br = bufio.NewReader(c)
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if resp.StatusCode() != StatusBadRequest {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusBadRequest)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestServerConnState(t *testing.T) {
	t.Parallel()

	states := make([]string, 0)
	s := &Server{
		Handler: func(ctx *RequestCtx) {},
		ConnState: func(conn net.Conn, state ConnState) {
			states = append(states, state.String())
		},
	}

	ln := fasthttputil.NewInmemoryListener()

	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c)
		// Send 2 requests on the same connection.
		for i := 0; i < 2; i++ {
			if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			var resp Response
			if err := resp.Read(br); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			if resp.StatusCode() != StatusOK {
				t.Errorf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
			}
		}
		if err := c.Close(); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		// Give the server a little bit of time to transition the connection to the close state.
		time.Sleep(time.Millisecond * 100)
		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	// 2 requests so we go to active and idle twice.
	expected := []string{"new", "active", "idle", "active", "idle", "closed"}

	if !reflect.DeepEqual(expected, states) {
		t.Fatalf("wrong state, expected %s, got %s", expected, states)
	}
}

func TestSaveMultipartFile(t *testing.T) {
	t.Parallel()

	filea := "This is a test file."
	fileb := strings.Repeat("test", 64)

	mr := multipart.NewReader(strings.NewReader(""+
		"--foo\r\n"+
		"Content-Disposition: form-data; name=\"filea\"; filename=\"filea.txt\"\r\n"+
		"Content-Type: text/plain\r\n"+
		"\r\n"+
		filea+"\r\n"+
		"--foo\r\n"+
		"Content-Disposition: form-data; name=\"fileb\"; filename=\"fileb.txt\"\r\n"+
		"Content-Type: text/plain\r\n"+
		"\r\n"+
		fileb+"\r\n"+
		"--foo--\r\n",
	), "foo")

	f, err := mr.ReadForm(64)
	if err != nil {
		t.Fatal(err)
	}

	if err := SaveMultipartFile(f.File["filea"][0], "filea.txt"); err != nil {
		t.Fatal(err)
	}
	defer os.Remove("filea.txt")

	if c, err := ioutil.ReadFile("filea.txt"); err != nil {
		t.Fatal(err)
	} else if string(c) != filea {
		t.Fatalf("filea changed expected %q got %q", filea, c)
	}

	// Make sure fileb was saved to a file.
	if ff, err := f.File["fileb"][0].Open(); err != nil {
		t.Fatal("expected FileHeader.Open to work")
	} else if _, ok := ff.(*os.File); !ok {
		t.Fatal("expected fileb to be an os.File")
	} else {
		ff.Close()
	}

	if err := SaveMultipartFile(f.File["fileb"][0], "fileb.txt"); err != nil {
		t.Fatal(err)
	}
	defer os.Remove("fileb.txt")

	if c, err := ioutil.ReadFile("fileb.txt"); err != nil {
		t.Fatal(err)
	} else if string(c) != fileb {
		t.Fatalf("fileb changed expected %q got %q", fileb, c)
	}
}

func TestServerName(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
		},
	}

	getReponse := func() []byte {
		rw := &readWriter{}
		rw.r.WriteString("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")

		if err := s.ServeConn(rw); err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}

		resp, err := ioutil.ReadAll(&rw.w)
		if err != nil {
			t.Fatalf("Unexpected error from ReadAll: %s", err)
		}

		return resp
	}

	resp := getReponse()
	if !bytes.Contains(resp, []byte("\r\nServer: "+string(defaultServerName)+"\r\n")) {
		t.Fatalf("Unexpected response %q expected Server: "+string(defaultServerName), resp)
	}

	// We can't just overwrite s.Name as fasthttp caches the name in an atomic.Value
	s = &Server{
		Handler: func(ctx *RequestCtx) {
		},
		Name: "foobar",
	}

	resp = getReponse()
	if !bytes.Contains(resp, []byte("\r\nServer: foobar\r\n")) {
		t.Fatalf("Unexpected response %q expected Server: foobar", resp)
	}

	s = &Server{
		Handler: func(ctx *RequestCtx) {
		},
		NoDefaultServerHeader: true,
		NoDefaultContentType:  true,
		NoDefaultDate:         true,
	}

	resp = getReponse()
	if bytes.Contains(resp, []byte("\r\nServer: ")) {
		t.Fatalf("Unexpected response %q expected no Server header", resp)
	}

	if bytes.Contains(resp, []byte("\r\nContent-Type: ")) {
		t.Fatalf("Unexpected response %q expected no Content-Type header", resp)
	}

	if bytes.Contains(resp, []byte("\r\nDate: ")) {
		t.Fatalf("Unexpected response %q expected no Date header", resp)
	}
}

func TestRequestCtxString(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx

	s := ctx.String()
	expectedS := "#0000000000000000 - 0.0.0.0:0<->0.0.0.0:0 - GET http:///"
	if s != expectedS {
		t.Fatalf("unexpected ctx.String: %q. Expecting %q", s, expectedS)
	}

	ctx.Request.SetRequestURI("https://foobar.com/aaa?bb=c")
	s = ctx.String()
	expectedS = "#0000000000000000 - 0.0.0.0:0<->0.0.0.0:0 - GET https://foobar.com/aaa?bb=c"
	if s != expectedS {
		t.Fatalf("unexpected ctx.String: %q. Expecting %q", s, expectedS)
	}
}

func TestServerErrSmallBuffer(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("shouldn't be never called") //nolint:errcheck
		},
		ReadBufferSize: 20,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET / HTTP/1.1\r\nHost: aabb.com\r\nVERY-long-Header: sdfdfsd dsf dsaf dsf df fsd\r\n\r\n")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	var serverErr error
	select {
	case serverErr = <-ch:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}

	if serverErr == nil {
		t.Fatal("expected error")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	statusCode := resp.StatusCode()
	if statusCode != StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusRequestHeaderFieldsTooLarge)
	}
	if !resp.ConnectionClose() {
		t.Fatal("missing 'Connection: close' response header")
	}

	expectedErr := errSmallBuffer.Error()
	if !strings.Contains(serverErr.Error(), expectedErr) {
		t.Fatalf("unexpected log output: %v. Expecting %q", serverErr, expectedErr)
	}
}

func TestRequestCtxIsTLS(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx

	// tls.Conn
	ctx.c = &tls.Conn{}
	if !ctx.IsTLS() {
		t.Fatal("IsTLS must return true")
	}

	// non-tls.Conn
	ctx.c = &readWriter{}
	if ctx.IsTLS() {
		t.Fatal("IsTLS must return false")
	}

	// overridden tls.Conn
	ctx.c = &struct {
		*tls.Conn
		fooBar bool
	}{}
	if !ctx.IsTLS() {
		t.Fatal("IsTLS must return true")
	}

	ctx.c = &perIPConn{Conn: &tls.Conn{}}
	if !ctx.IsTLS() {
		t.Fatal("IsTLS must return true")
	}
}

func TestRequestCtxRedirectHTTPSSchemeless(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx

	s := "GET /foo/bar?baz HTTP/1.1\nHost: aaa.com\n\n"
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := ctx.Request.Read(br); err != nil {
		t.Fatalf("cannot read request: %s", err)
	}
	ctx.Request.isTLS = true

	ctx.Redirect("//foobar.com/aa/bbb", StatusFound)
	location := ctx.Response.Header.Peek(HeaderLocation)
	expectedLocation := "https://foobar.com/aa/bbb"
	if string(location) != expectedLocation {
		t.Fatalf("Unexpected location: %q. Expecting %q", location, expectedLocation)
	}
}

func TestRequestCtxRedirect(t *testing.T) {
	t.Parallel()

	testRequestCtxRedirect(t, "http://qqq/", "", "http://qqq/")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "", "http://qqq/foo/bar?baz=111")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "#aaa", "http://qqq/foo/bar?baz=111#aaa")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "?abc=de&f", "http://qqq/foo/bar?abc=de&f")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "?abc=de&f#sf", "http://qqq/foo/bar?abc=de&f#sf")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "x.html", "http://qqq/foo/x.html")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "x.html?a=1", "http://qqq/foo/x.html?a=1")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "x.html#aaa=bbb&cc=ddd", "http://qqq/foo/x.html#aaa=bbb&cc=ddd")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "x.html?b=1#aaa=bbb&cc=ddd", "http://qqq/foo/x.html?b=1#aaa=bbb&cc=ddd")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "/x.html", "http://qqq/x.html")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "/x.html#aaa=bbb&cc=ddd", "http://qqq/x.html#aaa=bbb&cc=ddd")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "../x.html", "http://qqq/x.html")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "../../x.html", "http://qqq/x.html")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "./.././../x.html", "http://qqq/x.html")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "http://foo.bar/baz", "http://foo.bar/baz")
	testRequestCtxRedirect(t, "http://qqq/foo/bar?baz=111", "https://foo.bar/baz", "https://foo.bar/baz")
	testRequestCtxRedirect(t, "https://foo.com/bar?aaa", "//google.com/aaa?bb", "https://google.com/aaa?bb")
}

func testRequestCtxRedirect(t *testing.T, origURL, redirectURL, expectedURL string) {
	var ctx RequestCtx
	var req Request
	req.SetRequestURI(origURL)
	ctx.Init(&req, nil, nil)

	ctx.Redirect(redirectURL, StatusFound)
	loc := ctx.Response.Header.Peek(HeaderLocation)
	if string(loc) != expectedURL {
		t.Fatalf("unexpected redirect url %q. Expecting %q. origURL=%q, redirectURL=%q", loc, expectedURL, origURL, redirectURL)
	}
}

func TestServerResponseServerHeader(t *testing.T) {
	t.Parallel()

	serverName := "foobar serv"

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			name := ctx.Response.Header.Server()
			if string(name) != serverName {
				fmt.Fprintf(ctx, "unexpected server name: %q. Expecting %q", name, serverName)
			} else {
				ctx.WriteString("OK") //nolint:errcheck
			}

			// make sure the server name is sent to the client after ctx.Response.Reset()
			ctx.NotFound()
		},
		Name: serverName,
	}

	ln := fasthttputil.NewInmemoryListener()

	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c)
		var resp Response
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}

		if resp.StatusCode() != StatusNotFound {
			t.Errorf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusNotFound)
		}
		if string(resp.Body()) != "404 Page not found" {
			t.Errorf("unexpected body: %q. Expecting %q", resp.Body(), "404 Page not found")
		}
		if string(resp.Header.Server()) != serverName {
			t.Errorf("unexpected server header: %q. Expecting %q", resp.Header.Server(), serverName)
		}
		if err = c.Close(); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerResponseBodyStream(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	readyCh := make(chan struct{})
	h := func(ctx *RequestCtx) {
		ctx.SetConnectionClose()
		if ctx.IsBodyStream() {
			t.Fatal("IsBodyStream must return false")
		}
		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			fmt.Fprintf(w, "first")
			if err := w.Flush(); err != nil {
				return
			}
			<-readyCh
			fmt.Fprintf(w, "second")
			// there is no need to flush w here, since it will
			// be flushed automatically after returning from StreamWriter.
		})
		if !ctx.IsBodyStream() {
			t.Fatal("IsBodyStream must return true")
		}
	}

	serverCh := make(chan struct{})
	go func() {
		if err := Serve(ln, h); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c)
		var respH ResponseHeader
		if err = respH.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if respH.StatusCode() != StatusOK {
			t.Errorf("unexpected status code: %d. Expecting %d", respH.StatusCode(), StatusOK)
		}

		buf := make([]byte, 1024)
		n, err := br.Read(buf)
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		b := buf[:n]
		if string(b) != "5\r\nfirst\r\n" {
			t.Errorf("unexpected result %q. Expecting %q", b, "5\r\nfirst\r\n")
		}
		close(readyCh)

		tail, err := ioutil.ReadAll(br)
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if string(tail) != "6\r\nsecond\r\n0\r\n\r\n" {
			t.Errorf("unexpected tail %q. Expecting %q", tail, "6\r\nsecond\r\n0\r\n\r\n")
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerDisableKeepalive(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("OK") //nolint:errcheck
		},
		DisableKeepalive: true,
	}

	ln := fasthttputil.NewInmemoryListener()

	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c)
		var resp Response
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Errorf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
		}
		if !resp.ConnectionClose() {
			t.Error("expecting 'Connection: close' response header")
		}
		if string(resp.Body()) != "OK" {
			t.Errorf("unexpected body: %q. Expecting %q", resp.Body(), "OK")
		}

		// make sure the connection is closed
		data, err := ioutil.ReadAll(br)
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if len(data) > 0 {
			t.Errorf("unexpected data read from the connection: %q. Expecting empty data", data)
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerMaxConnsPerIPLimit(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("OK") //nolint:errcheck
		},
		MaxConnsPerIP: 1,
		Logger:        &testLogger{},
	}

	ln := fasthttputil.NewInmemoryListener()

	serverCh := make(chan struct{})
	go func() {
		fakeLN := &fakeIPListener{
			Listener: ln,
		}
		if err := s.Serve(fakeLN); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c1, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		c2, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c2)
		var resp Response
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusTooManyRequests {
			t.Errorf("unexpected status code for the second connection: %d. Expecting %d",
				resp.StatusCode(), StatusTooManyRequests)
		}

		if _, err = c1.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error when writing to the first connection: %s", err)
		}
		br = bufio.NewReader(c1)
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Errorf("unexpected status code for the first connection: %d. Expecting %d",
				resp.StatusCode(), StatusOK)
		}
		if string(resp.Body()) != "OK" {
			t.Errorf("unexpected body for the first connection: %q. Expecting %q", resp.Body(), "OK")
		}
		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

type fakeIPListener struct {
	net.Listener
}

func (ln *fakeIPListener) Accept() (net.Conn, error) {
	conn, err := ln.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &fakeIPConn{
		Conn: conn,
	}, nil
}

type fakeIPConn struct {
	net.Conn
}

func (conn *fakeIPConn) RemoteAddr() net.Addr {
	addr, err := net.ResolveTCPAddr("tcp4", "1.2.3.4:5789")
	if err != nil {
		panic(fmt.Sprintf("BUG: unexpected error: %s", err))
	}
	return addr
}

func TestServerConcurrencyLimit(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.WriteString("OK") //nolint:errcheck
		},
		Concurrency: 1,
		Logger:      &testLogger{},
	}

	ln := fasthttputil.NewInmemoryListener()

	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(serverCh)
	}()

	clientCh := make(chan struct{})
	go func() {
		c1, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		c2, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(c2)
		var resp Response
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusServiceUnavailable {
			t.Errorf("unexpected status code for the second connection: %d. Expecting %d",
				resp.StatusCode(), StatusServiceUnavailable)
		}

		if _, err = c1.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error when writing to the first connection: %s", err)
		}
		br = bufio.NewReader(c1)
		if err = resp.Read(br); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		if resp.StatusCode() != StatusOK {
			t.Errorf("unexpected status code for the first connection: %d. Expecting %d",
				resp.StatusCode(), StatusOK)
		}
		if string(resp.Body()) != "OK" {
			t.Errorf("unexpected body for the first connection: %q. Expecting %q", resp.Body(), "OK")
		}
		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerWriteFastError(t *testing.T) {
	t.Parallel()

	s := &Server{
		Name: "foobar",
	}
	var buf bytes.Buffer
	expectedBody := "access denied"
	s.writeFastError(&buf, StatusForbidden, expectedBody)

	br := bufio.NewReader(&buf)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if resp.StatusCode() != StatusForbidden {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusForbidden)
	}
	body := resp.Body()
	if string(body) != expectedBody {
		t.Fatalf("unexpected body: %q. Expecting %q", body, expectedBody)
	}
	server := string(resp.Header.Server())
	if server != s.Name {
		t.Fatalf("unexpected server: %q. Expecting %q", server, s.Name)
	}
	contentType := string(resp.Header.ContentType())
	if contentType != "text/plain" {
		t.Fatalf("unexpected content-type: %q. Expecting %q", contentType, "text/plain")
	}
	if !resp.Header.ConnectionClose() {
		t.Fatal("expecting 'Connection: close' response header")
	}
}

func TestServerTLS(t *testing.T) {
	t.Parallel()

	text := []byte("Make fasthttp great again")
	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Write(text) //nolint:errcheck
		},
	}

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}

	err = s.AppendCertEmbed(certData, keyData)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		err = s.ServeTLS(ln, "", "")
		if err != nil {
			t.Error(err)
		}
	}()

	c := &Client{
		ReadTimeout: time.Second * 2,
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	req, res := AcquireRequest(), AcquireResponse()
	req.SetRequestURI("https://some.url")

	err = c.Do(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(text, res.Body()) {
		t.Fatal("error transmitting information")
	}
}

func TestServerTLSReadTimeout(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		ReadTimeout: time.Millisecond * 500,
		Logger:      &testLogger{}, // Ignore log output.
		Handler: func(ctx *RequestCtx) {
		},
	}

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}

	err = s.AppendCertEmbed(certData, keyData)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		err = s.ServeTLS(ln, "", "")
		if err != nil {
			t.Error(err)
		}
	}()

	c, err := ln.Dial()
	if err != nil {
		t.Error(err)
	}

	r := make(chan error)

	go func() {
		b := make([]byte, 1)
		_, err := c.Read(b)
		c.Close()
		r <- err
	}()

	select {
	case err = <-r:
	case <-time.After(time.Second):
	}

	if err == nil {
		t.Error("server didn't close connection after timeout")
	}
}

func TestServerServeTLSEmbed(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	certData, keyData, err := GenerateTestCertificate("localhost")
	if err != nil {
		t.Fatal(err)
	}

	// start the server
	ch := make(chan struct{})
	go func() {
		err := ServeTLSEmbed(ln, certData, keyData, func(ctx *RequestCtx) {
			if !ctx.IsTLS() {
				ctx.Error("expecting tls", StatusBadRequest)
				return
			}
			scheme := ctx.URI().Scheme()
			if string(scheme) != "https" {
				ctx.Error(fmt.Sprintf("unexpected scheme=%q. Expecting %q", scheme, "https"), StatusBadRequest)
				return
			}
			ctx.WriteString("success") //nolint:errcheck
		})
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(ch)
	}()

	// establish connection to the server
	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
	})

	// send request
	if _, err = tlsConn.Write([]byte("GET / HTTP/1.1\r\nHost: aaa\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// read response
	respCh := make(chan struct{})
	go func() {
		br := bufio.NewReader(tlsConn)
		var resp Response
		if err := resp.Read(br); err != nil {
			t.Error("unexpected error")
		}
		body := resp.Body()
		if string(body) != "success" {
			t.Errorf("unexpected response body %q. Expecting %q", body, "success")
		}
		close(respCh)
	}()
	select {
	case <-respCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	// close the server
	if err = ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerMultipartFormDataRequest(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		StreamRequestBody            bool
		DisablePreParseMultipartForm bool
	}{
		{false, false},
		{false, true},
		{true, false},
		{true, true},
	} {
		reqS := `POST /upload HTTP/1.1
Host: qwerty.com
Content-Length: 521
Content-Type: multipart/form-data; boundary=----WebKitFormBoundaryJwfATyF8tmxSJnLg

------WebKitFormBoundaryJwfATyF8tmxSJnLg
Content-Disposition: form-data; name="f1"

value1
------WebKitFormBoundaryJwfATyF8tmxSJnLg
Content-Disposition: form-data; name="fileaaa"; filename="TODO"
Content-Type: application/octet-stream

- SessionClient with referer and cookies support.
- Client with requests' pipelining support.
- ProxyHandler similar to FSHandler.
- WebSockets. See https://tools.ietf.org/html/rfc6455 .
- HTTP/2.0. See https://tools.ietf.org/html/rfc7540 .

------WebKitFormBoundaryJwfATyF8tmxSJnLg--

GET / HTTP/1.1
Host: asbd
Connection: close

`
		ln := fasthttputil.NewInmemoryListener()

		s := &Server{
			StreamRequestBody:            test.StreamRequestBody,
			DisablePreParseMultipartForm: test.DisablePreParseMultipartForm,
			Handler: func(ctx *RequestCtx) {
				switch string(ctx.Path()) {
				case "/upload":
					f, err := ctx.MultipartForm()
					if err != nil {
						t.Errorf("unexpected error: %s", err)
					}
					if len(f.Value) != 1 {
						t.Errorf("unexpected values %d. Expecting %d", len(f.Value), 1)
					}
					if len(f.File) != 1 {
						t.Errorf("unexpected file values %d. Expecting %d", len(f.File), 1)
					}
					fv := ctx.FormValue("f1")
					if string(fv) != "value1" {
						t.Errorf("unexpected form value: %q. Expecting %q", fv, "value1")
					}
					ctx.Redirect("/", StatusSeeOther)
				default:
					ctx.WriteString("non-upload") //nolint:errcheck
				}
			},
		}

		ch := make(chan struct{})
		go func() {
			if err := s.Serve(ln); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			close(ch)
		}()

		conn, err := ln.Dial()
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if _, err = conn.Write([]byte(reqS)); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}

		var resp Response
		br := bufio.NewReader(conn)
		respCh := make(chan struct{})
		go func() {
			if err := resp.Read(br); err != nil {
				t.Errorf("error when reading response: %s", err)
			}
			if resp.StatusCode() != StatusSeeOther {
				t.Errorf("unexpected status code %d. Expecting %d", resp.StatusCode(), StatusSeeOther)
			}
			loc := resp.Header.Peek(HeaderLocation)
			if string(loc) != "http://qwerty.com/" {
				t.Errorf("unexpected location %q. Expecting %q", loc, "http://qwerty.com/")
			}

			if err := resp.Read(br); err != nil {
				t.Errorf("error when reading the second response: %s", err)
			}
			if resp.StatusCode() != StatusOK {
				t.Errorf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
			}
			body := resp.Body()
			if string(body) != "non-upload" {
				t.Errorf("unexpected body %q. Expecting %q", body, "non-upload")
			}
			close(respCh)
		}()

		select {
		case <-respCh:
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}

		if err := ln.Close(); err != nil {
			t.Fatalf("error when closing listener: %s", err)
		}

		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timeout when waiting for the server to stop")
		}
	}
}

func TestServerGetWithContent(t *testing.T) {
	t.Parallel()

	h := func(ctx *RequestCtx) {
		ctx.Success("foo/bar", []byte("success"))
	}
	s := &Server{
		Handler: h,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET / HTTP/1.1\r\nHost: mm.com\r\nContent-Length: 5\r\n\r\nabcde")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	resp := rw.w.String()
	if !strings.HasSuffix(resp, "success") {
		t.Fatalf("unexpected response %s.", resp)
	}
}

func TestServerDisableHeaderNamesNormalizing(t *testing.T) {
	t.Parallel()

	headerName := "CASE-senSITive-HEAder-NAME"
	headerNameLower := strings.ToLower(headerName)
	headerValue := "foobar baz"
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			hv := ctx.Request.Header.Peek(headerName)
			if string(hv) != headerValue {
				t.Errorf("unexpected header value for %q: %q. Expecting %q", headerName, hv, headerValue)
			}
			hv = ctx.Request.Header.Peek(headerNameLower)
			if len(hv) > 0 {
				t.Errorf("unexpected header value for %q: %q. Expecting empty value", headerNameLower, hv)
			}
			ctx.Response.Header.Set(headerName, headerValue)
			ctx.WriteString("ok") //nolint:errcheck
			ctx.SetContentType("aaa")
		},
		DisableHeaderNamesNormalizing: true,
	}

	rw := &readWriter{}
	rw.r.WriteString(fmt.Sprintf("GET / HTTP/1.1\r\n%s: %s\r\nHost: google.com\r\n\r\n", headerName, headerValue))

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	resp.Header.DisableNormalizing()
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	hv := resp.Header.Peek(headerName)
	if string(hv) != headerValue {
		t.Fatalf("unexpected header value for %q: %q. Expecting %q", headerName, hv, headerValue)
	}
	hv = resp.Header.Peek(headerNameLower)
	if len(hv) > 0 {
		t.Fatalf("unexpected header value for %q: %q. Expecting empty value", headerNameLower, hv)
	}
}

func TestServerReduceMemoryUsageSerial(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler:           func(ctx *RequestCtx) {},
		ReduceMemoryUsage: true,
	}

	ch := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(ch)
	}()

	testServerRequests(t, ln)

	if err := ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %s", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func TestServerReduceMemoryUsageConcurrent(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	s := &Server{
		Handler:           func(ctx *RequestCtx) {},
		ReduceMemoryUsage: true,
	}

	ch := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(ch)
	}()

	gCh := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			testServerRequests(t, ln)
			gCh <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		select {
		case <-gCh:
		case <-time.After(time.Second):
			t.Fatalf("timeout on goroutine %d", i)
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %s", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func testServerRequests(t *testing.T, ln *fasthttputil.InmemoryListener) {
	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	br := bufio.NewReader(conn)
	var resp Response
	for i := 0; i < 10; i++ {
		if _, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: aaa\r\n\r\n"); err != nil {
			t.Fatalf("unexpected error on iteration %d: %s", i, err)
		}

		respCh := make(chan struct{})
		go func() {
			if err = resp.Read(br); err != nil {
				t.Errorf("unexpected error when reading response on iteration %d: %s", i, err)
			}
			close(respCh)
		}()
		select {
		case <-respCh:
		case <-time.After(time.Second):
			t.Fatalf("timeout on iteration %d", i)
		}
	}

	if err = conn.Close(); err != nil {
		t.Fatalf("error when closing the connection: %s", err)
	}
}

func TestServerHTTP10ConnectionKeepAlive(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	ch := make(chan struct{})
	go func() {
		err := Serve(ln, func(ctx *RequestCtx) {
			if string(ctx.Path()) == "/close" {
				ctx.SetConnectionClose()
			}
		})
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(ch)
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	_, err = fmt.Fprintf(conn, "%s", "GET / HTTP/1.0\r\nHost: aaa\r\nConnection: keep-alive\r\n\r\n")
	if err != nil {
		t.Fatalf("error when writing request: %s", err)
	}
	_, err = fmt.Fprintf(conn, "%s", "GET /close HTTP/1.0\r\nHost: aaa\r\nConnection: keep-alive\r\n\r\n")
	if err != nil {
		t.Fatalf("error when writing request: %s", err)
	}

	br := bufio.NewReader(conn)
	var resp Response
	if err = resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.ConnectionClose() {
		t.Fatal("response mustn't have 'Connection: close' header")
	}
	if err = resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatal("response must have 'Connection: close' header")
	}

	tailCh := make(chan struct{})
	go func() {
		tail, err := ioutil.ReadAll(br)
		if err != nil {
			t.Errorf("error when reading tail: %s", err)
		}
		if len(tail) > 0 {
			t.Errorf("unexpected non-zero tail %q", tail)
		}
		close(tailCh)
	}()

	select {
	case <-tailCh:
	case <-time.After(time.Second):
		t.Fatal("timeout when reading tail")
	}

	if err = conn.Close(); err != nil {
		t.Fatalf("error when closing the connection: %s", err)
	}

	if err = ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %s", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func TestServerHTTP10ConnectionClose(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()

	ch := make(chan struct{})
	go func() {
		err := Serve(ln, func(ctx *RequestCtx) {
			// The server must close the connection irregardless
			// of request and response state set inside request
			// handler, since the HTTP/1.0 request
			// had no 'Connection: keep-alive' header.
			ctx.Request.Header.ResetConnectionClose()
			ctx.Request.Header.Set(HeaderConnection, "keep-alive")
			ctx.Response.Header.ResetConnectionClose()
			ctx.Response.Header.Set(HeaderConnection, "keep-alive")
		})
		if err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		close(ch)
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	_, err = fmt.Fprintf(conn, "%s", "GET / HTTP/1.0\r\nHost: aaa\r\n\r\n")
	if err != nil {
		t.Fatalf("error when writing request: %s", err)
	}

	br := bufio.NewReader(conn)
	var resp Response
	if err = resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}

	if !resp.ConnectionClose() {
		t.Fatal("HTTP1.0 response must have 'Connection: close' header")
	}

	tailCh := make(chan struct{})
	go func() {
		tail, err := ioutil.ReadAll(br)
		if err != nil {
			t.Errorf("error when reading tail: %s", err)
		}
		if len(tail) > 0 {
			t.Errorf("unexpected non-zero tail %q", tail)
		}
		close(tailCh)
	}()

	select {
	case <-tailCh:
	case <-time.After(time.Second):
		t.Fatal("timeout when reading tail")
	}

	if err = conn.Close(); err != nil {
		t.Fatalf("error when closing the connection: %s", err)
	}

	if err = ln.Close(); err != nil {
		t.Fatalf("error when closing listener: %s", err)
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout when waiting for the server to stop")
	}
}

func TestRequestCtxFormValue(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	req.SetRequestURI("/foo/bar?baz=123&aaa=bbb")
	req.SetBodyString("qqq=port&mmm=sddd")
	req.Header.SetContentType("application/x-www-form-urlencoded")

	ctx.Init(&req, nil, nil)

	v := ctx.FormValue("baz")
	if string(v) != "123" {
		t.Fatalf("unexpected value %q. Expecting %q", v, "123")
	}
	v = ctx.FormValue("mmm")
	if string(v) != "sddd" {
		t.Fatalf("unexpected value %q. Expecting %q", v, "sddd")
	}
	v = ctx.FormValue("aaaasdfsdf")
	if len(v) > 0 {
		t.Fatalf("unexpected value for unknown key %q", v)
	}
}

func TestRequestCtxUserValue(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx

	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("key-%d", i)
		ctx.SetUserValue(k, i)
	}
	for i := 5; i < 10; i++ {
		k := fmt.Sprintf("key-%d", i)
		ctx.SetUserValueBytes([]byte(k), i)
	}

	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key-%d", i)
		v := ctx.UserValue(k)
		n, ok := v.(int)
		if !ok || n != i {
			t.Fatalf("unexpected value obtained for key %q: %v. Expecting %d", k, v, i)
		}
	}
	vlen := 0
	ctx.VisitUserValues(func(key []byte, value interface{}) {
		vlen++
		v := ctx.UserValueBytes(key)
		if v != value {
			t.Fatalf("unexpected value obtained from VisitUserValues for key: %q, expecting: %#v but got: %#v", key, v, value)
		}
	})
	if len(ctx.userValues) != vlen {
		t.Fatalf("the length of user values returned from VisitUserValues is not equal to the length of the userValues, expecting: %d but got: %d", len(ctx.userValues), vlen)
	}

	ctx.ResetUserValues()
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("key-%d", i)
		v := ctx.UserValue(k)
		if v != nil {
			t.Fatalf("unexpected value obtained for key %q: %v. Expecting nil", k, v)
		}
	}
}

func TestServerHeadRequest(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			fmt.Fprintf(ctx, "Request method is %q", ctx.Method())
			ctx.SetContentType("aaa/bbb")
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("HEAD /foobar HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	resp.SkipBody = true
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if resp.Header.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.Header.StatusCode(), StatusOK)
	}
	if len(resp.Body()) > 0 {
		t.Fatalf("Unexpected non-zero body %q", resp.Body())
	}
	if resp.Header.ContentLength() != 24 {
		t.Fatalf("unexpected content-length %d. Expecting %d", resp.Header.ContentLength(), 24)
	}
	if string(resp.Header.ContentType()) != "aaa/bbb" {
		t.Fatalf("unexpected content-type %q. Expecting %q", resp.Header.ContentType(), "aaa/bbb")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) > 0 {
		t.Fatalf("unexpected remaining data %q", data)
	}
}

func TestServerExpect100Continue(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if !ctx.IsPost() {
				t.Errorf("unexpected method %q. Expecting POST", ctx.Method())
			}
			if string(ctx.Path()) != "/foo" {
				t.Errorf("unexpected path %q. Expecting %q", ctx.Path(), "/foo")
			}
			ct := ctx.Request.Header.ContentType()
			if string(ct) != "a/b" {
				t.Errorf("unexpectected content-type: %q. Expecting %q", ct, "a/b")
			}
			if string(ctx.PostBody()) != "12345" {
				t.Errorf("unexpected body: %q. Expecting %q", ctx.PostBody(), "12345")
			}
			ctx.WriteString("foobar") //nolint:errcheck
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: gle.com\r\nExpect: 100-continue\r\nContent-Length: 5\r\nContent-Type: a/b\r\n\r\n12345")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusOK, string(defaultContentType), "foobar")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) > 0 {
		t.Fatalf("unexpected remaining data %q", data)
	}
}

func TestServerContinueHandler(t *testing.T) {
	t.Parallel()

	acceptContentLength := 5
	s := &Server{
		ContinueHandler: func(headers *RequestHeader) bool {
			if !headers.IsPost() {
				t.Errorf("unexpected method %q. Expecting POST", headers.Method())
			}

			ct := headers.ContentType()
			if string(ct) != "a/b" {
				t.Errorf("unexpectected content-type: %q. Expecting %q", ct, "a/b")
			}

			// Pass on any request that isn't the accepted content length
			return headers.contentLength == acceptContentLength
		},
		Handler: func(ctx *RequestCtx) {
			if ctx.Request.Header.contentLength != acceptContentLength {
				t.Errorf("all requests with content-length: other than %d, should be denied", acceptContentLength)
			}
			if !ctx.IsPost() {
				t.Errorf("unexpected method %q. Expecting POST", ctx.Method())
			}
			if string(ctx.Path()) != "/foo" {
				t.Errorf("unexpected path %q. Expecting %q", ctx.Path(), "/foo")
			}
			ct := ctx.Request.Header.ContentType()
			if string(ct) != "a/b" {
				t.Errorf("unexpectected content-type: %q. Expecting %q", ct, "a/b")
			}
			if string(ctx.PostBody()) != "12345" {
				t.Errorf("unexpected body: %q. Expecting %q", ctx.PostBody(), "12345")
			}
			ctx.WriteString("foobar") //nolint:errcheck
		},
	}

	sendRequest := func(rw *readWriter, expectedStatusCode int, expectedResponse string) {
		if err := s.ServeConn(rw); err != nil {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}

		br := bufio.NewReader(&rw.w)
		verifyResponse(t, br, expectedStatusCode, string(defaultContentType), expectedResponse)

		data, err := ioutil.ReadAll(br)
		if err != nil {
			t.Fatalf("Unexpected error when reading remaining data: %s", err)
		}
		if len(data) > 0 {
			t.Fatalf("unexpected remaining data %q", data)
		}
	}

	// The same server should not fail when handling the three different types of requests
	// Regular requests
	// Expect 100 continue accepted
	// Exepect 100 continue denied
	rw := &readWriter{}
	for i := 0; i < 25; i++ {

		// Regular requests without Expect 100 continue header
		rw.r.Reset()
		rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: gle.com\r\nContent-Length: 5\r\nContent-Type: a/b\r\n\r\n12345")
		sendRequest(rw, StatusOK, "foobar")

		// Regular Expect 100 continue reqeuests that are accepted
		rw.r.Reset()
		rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: gle.com\r\nExpect: 100-continue\r\nContent-Length: 5\r\nContent-Type: a/b\r\n\r\n12345")
		sendRequest(rw, StatusOK, "foobar")

		// Requests being denied
		rw.r.Reset()
		rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: gle.com\r\nExpect: 100-continue\r\nContent-Length: 6\r\nContent-Type: a/b\r\n\r\n123456")
		sendRequest(rw, StatusExpectationFailed, "")
	}
}

func TestCompressHandler(t *testing.T) {
	t.Parallel()

	expectedBody := string(createFixedBody(2e4))
	h := CompressHandler(func(ctx *RequestCtx) {
		ctx.Write([]byte(expectedBody)) //nolint:errcheck
	})

	var ctx RequestCtx
	var resp Response

	// verify uncompressed response
	h(&ctx)
	s := ctx.Response.String()
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce := resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "")
	}
	body := resp.Body()
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify gzip-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, deflate, sdhc")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "gzip")
	}
	body, err := resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// an attempt to compress already compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set("Accept-Encoding", "gzip, deflate, sdhc")
	hh := CompressHandler(h)
	hh(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "gzip" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "gzip")
	}
	body, err = resp.BodyGunzip()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}

	// verify deflate-compressed response
	ctx.Request.Reset()
	ctx.Response.Reset()
	ctx.Request.Header.Set(HeaderAcceptEncoding, "foobar, deflate, sdhc")

	h(&ctx)
	s = ctx.Response.String()
	br = bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ce = resp.Header.Peek(HeaderContentEncoding)
	if string(ce) != "deflate" {
		t.Fatalf("unexpected Content-Encoding: %q. Expecting %q", ce, "deflate")
	}
	body, err = resp.BodyInflate()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if string(body) != expectedBody {
		t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
	}
}

func TestRequestCtxWriteString(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	n, err := ctx.WriteString("foo")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != 3 {
		t.Fatalf("unexpected n %d. Expecting 3", n)
	}
	n, err = ctx.WriteString("привет")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if n != 12 {
		t.Fatalf("unexpected n=%d. Expecting 12", n)
	}

	s := ctx.Response.Body()
	if string(s) != "fooпривет" {
		t.Fatalf("unexpected response body %q. Expecting %q", s, "fooпривет")
	}
}

func TestServeConnNonHTTP11KeepAlive(t *testing.T) {
	t.Parallel()

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.0\r\nConnection: keep-alive\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.0\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /must/be/ignored HTTP/1.0\r\nHost: google.com\r\n\r\n")

	requestsServed := 0

	ch := make(chan struct{})
	go func() {
		err := ServeConn(rw, func(ctx *RequestCtx) {
			requestsServed++
			ctx.SuccessString("aaa/bbb", "foobar")
		})
		if err != nil {
			t.Errorf("unexpected error in ServeConn: %s", err)
		}
		close(ch)
	}()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	br := bufio.NewReader(&rw.w)

	var resp Response

	// verify the first response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if string(resp.Header.Peek(HeaderConnection)) != "keep-alive" {
		t.Fatalf("unexpected Connection header %q. Expecting %q", resp.Header.Peek(HeaderConnection), "keep-alive")
	}
	if resp.Header.ConnectionClose() {
		t.Fatal("unexpected Connection: close")
	}

	// verify the second response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if string(resp.Header.Peek(HeaderConnection)) != "close" {
		t.Fatalf("unexpected Connection header %q. Expecting %q", resp.Header.Peek(HeaderConnection), "close")
	}
	if !resp.Header.ConnectionClose() {
		t.Fatal("expecting Connection: close")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after responses %q", data)
	}

	if requestsServed != 2 {
		t.Fatalf("unexpected number of requests served: %d. Expecting 2", requestsServed)
	}
}

func TestRequestCtxSetBodyStreamWriter(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	if ctx.IsBodyStream() {
		t.Fatal("IsBodyStream must return false")
	}
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		fmt.Fprintf(w, "body writer line 1\n")
		if err := w.Flush(); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		fmt.Fprintf(w, "body writer line 2\n")
	})
	if !ctx.IsBodyStream() {
		t.Fatal("IsBodyStream must return true")
	}

	s := ctx.Response.String()

	br := bufio.NewReader(bytes.NewBufferString(s))
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Error when reading response: %s", err)
	}

	body := string(resp.Body())
	expectedBody := "body writer line 1\nbody writer line 2\n"
	if body != expectedBody {
		t.Fatalf("unexpected body: %q. Expecting %q", body, expectedBody)
	}
}

func TestRequestCtxIfModifiedSince(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	lastModified := time.Now().Add(-time.Hour)

	if !ctx.IfModifiedSince(lastModified) {
		t.Fatal("IfModifiedSince must return true for non-existing If-Modified-Since header")
	}

	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	if ctx.IfModifiedSince(lastModified) {
		t.Fatal("If-Modified-Since current time must return false")
	}

	past := lastModified.Add(-time.Hour)
	if ctx.IfModifiedSince(past) {
		t.Fatal("If-Modified-Since past time must return false")
	}

	future := lastModified.Add(time.Hour)
	if !ctx.IfModifiedSince(future) {
		t.Fatal("If-Modified-Since future time must return true")
	}
}

func TestRequestCtxSendFileNotModified(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	lastModified, err := FileLastModified(filePath)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	ctx.SendFile(filePath)

	s := ctx.Response.String()

	var resp Response
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusNotModified {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusNotModified)
	}
	if len(resp.Body()) > 0 {
		t.Fatalf("unexpected non-zero response body: %q", resp.Body())
	}
}

func TestRequestCtxSendFileModified(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	lastModified, err := FileLastModified(filePath)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	lastModified = lastModified.Add(-time.Hour)
	ctx.Request.Header.Set("If-Modified-Since", string(AppendHTTPDate(nil, lastModified)))

	ctx.SendFile(filePath)

	s := ctx.Response.String()

	var resp Response
	br := bufio.NewReader(bytes.NewBufferString(s))
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("cannot open file: %s", err)
	}
	body, err := ioutil.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("error when reading file: %s", err)
	}

	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("unexpected response body: %q. Expecting %q", resp.Body(), body)
	}
}

func TestRequestCtxSendFile(t *testing.T) {
	t.Parallel()

	var ctx RequestCtx
	var req Request
	ctx.Init(&req, nil, defaultLogger)

	filePath := "./server_test.go"
	ctx.SendFile(filePath)

	w := &bytes.Buffer{}
	bw := bufio.NewWriter(w)
	if err := ctx.Response.Write(bw); err != nil {
		t.Fatalf("error when writing response: %s", err)
	}
	if err := bw.Flush(); err != nil {
		t.Fatalf("error when flushing response: %s", err)
	}

	var resp Response
	br := bufio.NewReader(w)
	if err := resp.Read(br); err != nil {
		t.Fatalf("error when reading response: %s", err)
	}
	if resp.StatusCode() != StatusOK {
		t.Fatalf("unexpected status code: %d. Expecting %d", resp.StatusCode(), StatusOK)
	}

	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("cannot open file: %s", err)
	}
	body, err := ioutil.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("error when reading file: %s", err)
	}

	if !bytes.Equal(resp.Body(), body) {
		t.Fatalf("unexpected response body: %q. Expecting %q", resp.Body(), body)
	}
}

func TestRequestCtxHijack(t *testing.T) {
	t.Parallel()

	hijackStartCh := make(chan struct{})
	hijackStopCh := make(chan struct{})
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			if ctx.Hijacked() {
				t.Error("connection mustn't be hijacked")
			}
			ctx.Hijack(func(c net.Conn) {
				<-hijackStartCh

				b := make([]byte, 1)
				// ping-pong echo via hijacked conn
				for {
					n, err := c.Read(b)
					if n != 1 {
						if err == io.EOF {
							close(hijackStopCh)
							return
						}
						if err != nil {
							t.Errorf("unexpected error: %s", err)
						}
						t.Errorf("unexpected number of bytes read: %d. Expecting 1", n)
					}
					if _, err = c.Write(b); err != nil {
						t.Errorf("unexpected error when writing data: %s", err)
					}
				}
			})
			if !ctx.Hijacked() {
				t.Error("connection must be hijacked")
			}
			ctx.Success("foo/bar", []byte("hijack it!"))
		},
	}

	hijackedString := "foobar baz hijacked!!!"
	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString(hijackedString)

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusOK, "foo/bar", "hijack it!")

	close(hijackStartCh)
	select {
	case <-hijackStopCh:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if string(data) != hijackedString {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, hijackedString)
	}
}

func TestRequestCtxHijackNoResponse(t *testing.T) {
	t.Parallel()

	hijackDone := make(chan error)
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Hijack(func(c net.Conn) {
				_, err := c.Write([]byte("test"))
				hijackDone <- err
			})
			ctx.HijackSetNoResponse(true)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\nContent-Length: 0\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	select {
	case err := <-hijackDone:
		if err != nil {
			t.Fatalf("Unexpected error from hijack: %s", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}

	if got := rw.w.String(); got != "test" {
		t.Errorf(`expected "test", got %q`, got)
	}
}

func TestRequestCtxNoHijackNoResponse(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			io.WriteString(ctx, "test") //nolint:errcheck
			ctx.HijackSetNoResponse(true)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\nContent-Length: 0\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	bf := bufio.NewReader(
		strings.NewReader(rw.w.String()),
	)
	resp := AcquireResponse()
	resp.Read(bf) //nolint:errcheck
	if got := string(resp.Body()); got != "test" {
		t.Errorf(`expected "test", got %q`, got)
	}
}

func TestRequestCtxInit(t *testing.T) {
	// This test can't run parallel as it modifies globalConnID.

	var ctx RequestCtx
	var logger testLogger
	globalConnID = 0x123456
	ctx.Init(&ctx.Request, zeroTCPAddr, &logger)
	ip := ctx.RemoteIP()
	if !ip.IsUnspecified() {
		t.Fatalf("unexpected ip for bare RequestCtx: %q. Expected 0.0.0.0", ip)
	}
	ctx.Logger().Printf("foo bar %d", 10)

	expectedLog := "#0012345700000000 - 0.0.0.0:0<->0.0.0.0:0 - GET http:/// - foo bar 10\n"
	if logger.out != expectedLog {
		t.Fatalf("Unexpected log output: %q. Expected %q", logger.out, expectedLog)
	}
}

func TestTimeoutHandlerSuccess(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	h := func(ctx *RequestCtx) {
		if string(ctx.Path()) == "/" {
			ctx.Success("aaa/bbb", []byte("real response"))
		}
	}
	s := &Server{
		Handler: TimeoutHandler(h, 10*time.Second, "timeout!!!"),
	}
	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		close(serverCh)
	}()

	concurrency := 20
	clientCh := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			conn, err := ln.Dial()
			if err != nil {
				t.Errorf("unexepcted error: %s", err)
			}
			if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			br := bufio.NewReader(conn)
			verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
			clientCh <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-clientCh:
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestTimeoutHandlerTimeout(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	readyCh := make(chan struct{})
	doneCh := make(chan struct{})
	h := func(ctx *RequestCtx) {
		ctx.Success("aaa/bbb", []byte("real response"))
		<-readyCh
		doneCh <- struct{}{}
	}
	s := &Server{
		Handler: TimeoutHandler(h, 20*time.Millisecond, "timeout!!!"),
	}
	serverCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		close(serverCh)
	}()

	concurrency := 20
	clientCh := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			conn, err := ln.Dial()
			if err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
			br := bufio.NewReader(conn)
			verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "timeout!!!")
			clientCh <- struct{}{}
		}()
	}

	for i := 0; i < concurrency; i++ {
		select {
		case <-clientCh:
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}

	close(readyCh)
	for i := 0; i < concurrency; i++ {
		select {
		case <-doneCh:
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	select {
	case <-serverCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestTimeoutHandlerTimeoutReuse(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	h := func(ctx *RequestCtx) {
		if string(ctx.Path()) == "/timeout" {
			time.Sleep(time.Second)
		}
		ctx.SetBodyString("ok")
	}
	s := &Server{
		Handler: TimeoutHandler(h, 500*time.Millisecond, "timeout!!!"),
	}
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()

	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	br := bufio.NewReader(conn)
	if _, err = conn.Write([]byte("GET /timeout HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "timeout!!!")

	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	verifyResponse(t, br, StatusOK, string(defaultContentType), "ok")

	if err := ln.Close(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestServerGetOnly(t *testing.T) {
	t.Parallel()

	h := func(ctx *RequestCtx) {
		if !ctx.IsGet() {
			t.Errorf("non-get request: %q", ctx.Method())
		}
		ctx.Success("foo/bar", []byte("success"))
	}
	s := &Server{
		Handler: h,
		GetOnly: true,
	}

	rw := &readWriter{}
	rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: google.com\r\nContent-Length: 5\r\nContent-Type: aaa\r\n\r\n12345")

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()

	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("expecting error")
		}
		if err != ErrGetOnly {
			t.Fatalf("Unexpected error from serveConn: %s. Expecting %s", err, ErrGetOnly)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	statusCode := resp.StatusCode()
	if statusCode != StatusBadRequest {
		t.Fatalf("unexpected status code: %d. Expecting %d", statusCode, StatusBadRequest)
	}
	if !resp.ConnectionClose() {
		t.Fatal("missing 'Connection: close' response header")
	}
}

func TestServerTimeoutErrorWithResponse(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()

			var resp Response

			resp.SetStatusCode(123)
			resp.SetBodyString("foobar. Should be ignored")
			ctx.TimeoutErrorWithResponse(&resp)

			resp.SetStatusCode(456)
			resp.ResetBody()
			fmt.Fprintf(resp.BodyWriter(), "path=%s", ctx.Path())
			resp.Header.SetContentType("foo/bar")
			ctx.TimeoutErrorWithResponse(&resp)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 456, "foo/bar", "path=/foo")
	verifyResponse(t, br, 456, "foo/bar", "path=/bar")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerTimeoutErrorWithCode(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()
			ctx.TimeoutErrorWithCode("should be ignored", 234)
			ctx.TimeoutErrorWithCode("stolen ctx", StatusBadRequest)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusBadRequest, string(defaultContentType), "stolen ctx")
	verifyResponse(t, br, StatusBadRequest, string(defaultContentType), "stolen ctx")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerTimeoutError(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			go func() {
				ctx.Success("aaa/bbb", []byte("xxxyyy"))
			}()
			ctx.TimeoutError("should be ignored")
			ctx.TimeoutError("stolen ctx")
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /foo HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "stolen ctx")
	verifyResponse(t, br, StatusRequestTimeout, string(defaultContentType), "stolen ctx")

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerMaxRequestsPerConn(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler:            func(ctx *RequestCtx) {},
		MaxRequestsPerConn: 1,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatal("Response must have 'connection: close' header")
	}
	verifyResponseHeader(t, &resp.Header, 200, 0, string(defaultContentType))

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerConnectionClose(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetConnectionClose()
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /must/be/ignored HTTP/1.1\r\nHost: aaa.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	var resp Response

	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}
	if !resp.ConnectionClose() {
		t.Fatal("expecting Connection: close header")
	}

	data, err := ioutil.ReadAll(br)
	if err != nil {
		t.Fatalf("Unexpected error when reading remaining data: %s", err)
	}
	if len(data) != 0 {
		t.Fatalf("Unexpected data read after the first response %q. Expecting %q", data, "")
	}
}

func TestServerRequestNumAndTime(t *testing.T) {
	t.Parallel()

	n := uint64(0)
	var connT time.Time
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			n++
			if ctx.ConnRequestNum() != n {
				t.Errorf("unexpected request number: %d. Expecting %d", ctx.ConnRequestNum(), n)
			}
			if connT.IsZero() {
				connT = ctx.ConnTime()
			}
			if ctx.ConnTime() != connT {
				t.Errorf("unexpected serve conn time: %s. Expecting %s", ctx.ConnTime(), connT)
			}
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /bar HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("GET /baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	if n != 3 {
		t.Fatalf("unexpected number of requests served: %d. Expecting %d", n, 3)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, string(defaultContentType), "")
}

func TestServerEmptyResponse(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			// do nothing :)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, string(defaultContentType), "")
}

func TestServerLogger(t *testing.T) {
	// This test can't run parallel as it modifies globalConnID.

	cl := &testLogger{}
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			logger := ctx.Logger()
			h := &ctx.Request.Header
			logger.Printf("begin")
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, body=%q, remoteAddr=%s",
				h.RequestURI(), ctx.Request.Body(), ctx.RemoteAddr())))
			logger.Printf("end")
		},
		Logger: cl,
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")
	rw.r.WriteString("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: 5\r\nContent-Type: aa\r\n\r\nabcde")

	rwx := &readWriterRemoteAddr{
		rw: rw,
		addr: &net.TCPAddr{
			IP:   []byte{1, 2, 3, 4},
			Port: 8765,
		},
	}

	globalConnID = 0

	if err := s.ServeConn(rwx); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo1, body=\"\", remoteAddr=1.2.3.4:8765")
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo2, body=\"abcde\", remoteAddr=1.2.3.4:8765")

	expectedLogOut := `#0000000100000001 - 1.2.3.4:8765<->1.2.3.4:8765 - GET http://google.com/foo1 - begin
#0000000100000001 - 1.2.3.4:8765<->1.2.3.4:8765 - GET http://google.com/foo1 - end
#0000000100000002 - 1.2.3.4:8765<->1.2.3.4:8765 - POST http://aaa.com/foo2 - begin
#0000000100000002 - 1.2.3.4:8765<->1.2.3.4:8765 - POST http://aaa.com/foo2 - end
`
	if cl.out != expectedLogOut {
		t.Fatalf("Unexpected logger output: %q. Expected %q", cl.out, expectedLogOut)
	}
}

func TestServerRemoteAddr(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, remoteAddr=%s, remoteIP=%s",
				h.RequestURI(), ctx.RemoteAddr(), ctx.RemoteIP())))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")

	rwx := &readWriterRemoteAddr{
		rw: rw,
		addr: &net.TCPAddr{
			IP:   []byte{1, 2, 3, 4},
			Port: 8765,
		},
	}

	if err := s.ServeConn(rwx); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo1, remoteAddr=1.2.3.4:8765, remoteIP=1.2.3.4")
}

func TestServerCustomRemoteAddr(t *testing.T) {
	t.Parallel()

	customRemoteAddrHandler := func(h RequestHandler) RequestHandler {
		return func(ctx *RequestCtx) {
			ctx.SetRemoteAddr(&net.TCPAddr{
				IP:   []byte{1, 2, 3, 5},
				Port: 0,
			})
			h(ctx)
		}
	}

	s := &Server{
		Handler: customRemoteAddrHandler(func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("text/html", []byte(fmt.Sprintf("requestURI=%s, remoteAddr=%s, remoteIP=%s",
				h.RequestURI(), ctx.RemoteAddr(), ctx.RemoteIP())))
		}),
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo1 HTTP/1.1\r\nHost: google.com\r\n\r\n")

	rwx := &readWriterRemoteAddr{
		rw: rw,
		addr: &net.TCPAddr{
			IP:   []byte{1, 2, 3, 4},
			Port: 8765,
		},
	}

	if err := s.ServeConn(rwx); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "text/html", "requestURI=/foo1, remoteAddr=1.2.3.5:0, remoteIP=1.2.3.5")
}

type readWriterRemoteAddr struct {
	net.Conn
	rw   io.ReadWriteCloser
	addr net.Addr
}

func (rw *readWriterRemoteAddr) Close() error {
	return rw.rw.Close()
}

func (rw *readWriterRemoteAddr) Read(b []byte) (int, error) {
	return rw.rw.Read(b)
}

func (rw *readWriterRemoteAddr) Write(b []byte) (int, error) {
	return rw.rw.Write(b)
}

func (rw *readWriterRemoteAddr) RemoteAddr() net.Addr {
	return rw.addr
}

func (rw *readWriterRemoteAddr) LocalAddr() net.Addr {
	return rw.addr
}

func TestServerConnError(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Error("foobar", 423)
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	var resp Response
	if err := resp.Read(br); err != nil {
		t.Fatalf("Unexpected error when reading response: %s", err)
	}
	if resp.Header.StatusCode() != 423 {
		t.Fatalf("Unexpected status code %d. Expected %d", resp.Header.StatusCode(), 423)
	}
	if resp.Header.ContentLength() != 6 {
		t.Fatalf("Unexpected Content-Length %d. Expected %d", resp.Header.ContentLength(), 6)
	}
	if !bytes.Equal(resp.Header.Peek(HeaderContentType), defaultContentType) {
		t.Fatalf("Unexpected Content-Type %q. Expected %q", resp.Header.Peek(HeaderContentType), defaultContentType)
	}
	if !bytes.Equal(resp.Body(), []byte("foobar")) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body(), "foobar")
	}
}

func TestServeConnSingleRequest(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI(), h.Peek(HeaderHost))))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "aaa", "requestURI=/foo/bar?baz, host=google.com")
}

func TestServeConnMultiRequests(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			h := &ctx.Request.Header
			ctx.Success("aaa", []byte(fmt.Sprintf("requestURI=%s, host=%s", h.RequestURI(), h.Peek(HeaderHost))))
		},
	}

	rw := &readWriter{}
	rw.r.WriteString("GET /foo/bar?baz HTTP/1.1\r\nHost: google.com\r\n\r\nGET /abc HTTP/1.1\r\nHost: foobar.com\r\n\r\n")

	if err := s.ServeConn(rw); err != nil {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}

	br := bufio.NewReader(&rw.w)
	verifyResponse(t, br, 200, "aaa", "requestURI=/foo/bar?baz, host=google.com")
	verifyResponse(t, br, 200, "aaa", "requestURI=/abc, host=foobar.com")
}

func TestShutdown(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			time.Sleep(time.Millisecond * 500)
			ctx.Success("aaa/bbb", []byte("real response"))
		},
	}
	serveCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		_, err := ln.Dial()
		if err == nil {
			t.Error("server is still listening")
		}
		serveCh <- struct{}{}
	}()
	clientCh := make(chan struct{})
	go func() {
		conn, err := ln.Dial()
		if err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(conn)
		resp := verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
		verifyResponseHeaderConnection(t, &resp.Header, "")
		clientCh <- struct{}{}
	}()
	time.Sleep(time.Millisecond * 100)
	shutdownCh := make(chan struct{})
	go func() {
		if err := s.Shutdown(); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		shutdownCh <- struct{}{}
	}()
	done := 0
	for {
		select {
		case <-time.After(time.Second * 2):
			t.Fatal("shutdown took too long")
		case <-serveCh:
			done++
		case <-clientCh:
			done++
		case <-shutdownCh:
			done++
		}
		if done == 3 {
			return
		}
	}
}

func TestCloseOnShutdown(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			time.Sleep(time.Millisecond * 500)
			ctx.Success("aaa/bbb", []byte("real response"))
		},
		CloseOnShutdown: true,
	}
	serveCh := make(chan struct{})
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		_, err := ln.Dial()
		if err == nil {
			t.Error("server is still listening")
		}
		serveCh <- struct{}{}
	}()
	clientCh := make(chan struct{})
	go func() {
		conn, err := ln.Dial()
		if err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %s", err)
		}
		br := bufio.NewReader(conn)
		resp := verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
		verifyResponseHeaderConnection(t, &resp.Header, "close")
		clientCh <- struct{}{}
	}()
	time.Sleep(time.Millisecond * 100)
	shutdownCh := make(chan struct{})
	go func() {
		if err := s.Shutdown(); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
		shutdownCh <- struct{}{}
	}()
	done := 0
	for {
		select {
		case <-time.After(time.Second):
			t.Fatal("shutdown took too long")
		case <-serveCh:
			done++
		case <-clientCh:
			done++
		case <-shutdownCh:
			done++
		}
		if done == 3 {
			return
		}
	}
}

func TestShutdownReuse(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Success("aaa/bbb", []byte("real response"))
		},
		ReadTimeout: time.Millisecond * 100,
		Logger:      &testLogger{}, // Ignore log output.
	}
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	br := bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
	if err := s.Shutdown(); err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	ln = fasthttputil.NewInmemoryListener()
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	conn, err = ln.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	br = bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
	if err := s.Shutdown(); err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
}

func TestShutdownDone(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			<-ctx.Done()
			ctx.Success("aaa/bbb", []byte("real response"))
		},
	}
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	go func() {
		// Shutdown won't return if the connection doesn't close,
		// which doesn't happen until we read the response.
		if err := s.Shutdown(); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	// We can only reach this point and get a valid response
	// if reading from ctx.Done() returned.
	br := bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
}

func TestShutdownErr(t *testing.T) {
	t.Parallel()

	ln := fasthttputil.NewInmemoryListener()
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			// This will panic, but I was not able to intercept with recover()
			c, cancel := context.WithCancel(ctx)
			defer cancel()
			<-c.Done()
			ctx.Success("aaa/bbb", []byte("real response"))
		},
	}

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	conn, err := ln.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	go func() {
		// Shutdown won't return if the connection doesn't close,
		// which doesn't happen until we read the response.
		if err := s.Shutdown(); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	// We can only reach this point and get a valid response
	// if reading from ctx.Done() returned.
	br := bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
}

func TestMultipleServe(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.Success("aaa/bbb", []byte("real response"))
		},
	}

	ln1 := fasthttputil.NewInmemoryListener()
	ln2 := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln1); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()
	go func() {
		if err := s.Serve(ln2); err != nil {
			t.Errorf("unexepcted error: %s", err)
		}
	}()

	conn, err := ln1.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	br := bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")

	conn, err = ln2.Dial()
	if err != nil {
		t.Fatalf("unexepcted error: %s", err)
	}
	if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: google.com\r\n\r\n")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	br = bufio.NewReader(conn)
	verifyResponse(t, br, StatusOK, "aaa/bbb", "real response")
}

func TestMaxBodySizePerRequest(t *testing.T) {
	t.Parallel()

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			// do nothing :)
		},
		HeaderReceived: func(header *RequestHeader) RequestConfig {
			return RequestConfig{
				MaxRequestBodySize: 5 << 10,
			}
		},
		ReadTimeout:        time.Second * 5,
		WriteTimeout:       time.Second * 5,
		MaxRequestBodySize: 1 << 20,
	}

	rw := &readWriter{}
	rw.r.WriteString(fmt.Sprintf("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: %d\r\nContent-Type: aa\r\n\r\n%s", (5<<10)+1, strings.Repeat("a", (5<<10)+1)))

	if err := s.ServeConn(rw); err != ErrBodyTooLarge {
		t.Fatalf("Unexpected error from serveConn: %s", err)
	}
}

func TestStreamRequestBody(t *testing.T) {
	t.Parallel()

	part1 := strings.Repeat("1", 1<<15)
	part2 := strings.Repeat("2", 1<<16)
	contentLength := len(part1) + len(part2)
	next := make(chan struct{})

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			checkReader(t, ctx.RequestBodyStream(), part1)
			close(next)
			checkReader(t, ctx.RequestBodyStream(), part2)
		},
		StreamRequestBody: true,
	}

	pipe := fasthttputil.NewPipeConns()
	cc, sc := pipe.Conn1(), pipe.Conn2()
	//write headers and part1 body
	if _, err := cc.Write([]byte(fmt.Sprintf("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: %d\r\nContent-Type: aa\r\n\r\n", contentLength))); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.Write([]byte(part1)); err != nil {
		t.Fatal(err)
	}

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(sc)
	}()

	select {
	case <-next:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("part1 timeout")
	}

	if _, err := cc.Write([]byte(part2)); err != nil {
		t.Fatal(err)
	}
	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-ch:
		if err == nil || err.Error() != "connection closed" { // fasthttputil.errConnectionClosed is private so do a string match.
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("part2 timeout")
	}
}

func TestStreamRequestBodyExceedMaxSize(t *testing.T) {
	t.Parallel()

	part1 := strings.Repeat("1", 1<<18)
	part2 := strings.Repeat("2", 1<<20-1<<18)
	contentLength := len(part1) + len(part2)
	next := make(chan struct{})

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			checkReader(t, ctx.RequestBodyStream(), part1)
			close(next)
			checkReader(t, ctx.RequestBodyStream(), part2)
		},
		DisableKeepalive:   true,
		StreamRequestBody:  true,
		MaxRequestBodySize: 1,
	}

	pipe := fasthttputil.NewPipeConns()
	cc, sc := pipe.Conn1(), pipe.Conn2()
	//write headers and part1 body
	if _, err := cc.Write([]byte(fmt.Sprintf("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: %d\r\nContent-Type: aa\r\n\r\n%s", contentLength, part1))); err != nil {
		t.Error(err)
	}

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(sc)
	}()

	select {
	case <-next:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("part1 timeout")
	}

	if _, err := cc.Write([]byte(part2)); err != nil {
		t.Error(err)
	}

	select {
	case err := <-ch:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("part2 timeout")
	}
}

func TestStreamBodyReqestContentLength(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("1", 1<<15) // 32K
	contentLength := len(content)

	s := &Server{
		Handler: func(ctx *RequestCtx) {
			realContentLength := ctx.Request.Header.ContentLength()
			if realContentLength != contentLength {
				t.Fatal("incorrect content length")
			}
		},
		MaxRequestBodySize: 1 * 1024 * 1024, // 1M
		StreamRequestBody:  true,
	}

	pipe := fasthttputil.NewPipeConns()
	cc, sc := pipe.Conn1(), pipe.Conn2()
	if _, err := cc.Write([]byte(fmt.Sprintf("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: %d\r\nContent-Type: aa\r\n\r\n%s", contentLength, content))); err != nil {
		t.Fatal(err)
	}

	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(sc)
	}()

	if err := sc.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-ch:
		if err == nil || err.Error() != "connection closed" { // fasthttputil.errConnectionClosed is private so do a string match.
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatal("test timeout")
	}
}

func checkReader(t *testing.T, r io.Reader, expected string) {
	b := make([]byte, len(expected))
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("Unexpected error from reader: %s", err)
	}
	if string(b) != expected {
		t.Fatal("incorrect request body")
	}
}

func TestMaxReadTimeoutPerRequest(t *testing.T) {
	t.Parallel()

	headers := []byte(fmt.Sprintf("POST /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Length: %d\r\nContent-Type: aa\r\n\r\n", 5*1024))
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			t.Error("shouldn't reach handler")
		},
		HeaderReceived: func(header *RequestHeader) RequestConfig {
			return RequestConfig{
				ReadTimeout: time.Millisecond,
			}
		},
		ReadBufferSize: len(headers),
		ReadTimeout:    time.Second * 5,
		WriteTimeout:   time.Second * 5,
	}

	pipe := fasthttputil.NewPipeConns()
	cc, sc := pipe.Conn1(), pipe.Conn2()
	go func() {
		//write headers
		_, err := cc.Write(headers)
		if err != nil {
			t.Error(err)
		}
		//write body
		for i := 0; i < 5*1024; i++ {
			time.Sleep(time.Millisecond)
			cc.Write([]byte{'a'}) //nolint:errcheck
		}
	}()
	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(sc)
	}()

	select {
	case err := <-ch:
		if err == nil || err != nil && !strings.EqualFold(err.Error(), "timeout") {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatal("test timeout")
	}
}

func TestMaxWriteTimeoutPerRequest(t *testing.T) {
	t.Parallel()

	headers := []byte("GET /foo2 HTTP/1.1\r\nHost: aaa.com\r\nContent-Type: aa\r\n\r\n")
	s := &Server{
		Handler: func(ctx *RequestCtx) {
			ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
				var buf [192]byte
				for {
					w.Write(buf[:]) //nolint:errcheck
				}
			})
		},
		HeaderReceived: func(header *RequestHeader) RequestConfig {
			return RequestConfig{
				WriteTimeout: time.Millisecond,
			}
		},
		ReadBufferSize: 192,
		ReadTimeout:    time.Second * 5,
		WriteTimeout:   time.Second * 5,
	}

	pipe := fasthttputil.NewPipeConns()
	cc, sc := pipe.Conn1(), pipe.Conn2()

	var resp Response
	go func() {
		//write headers
		_, err := cc.Write(headers)
		if err != nil {
			t.Error(err)
		}
		br := bufio.NewReaderSize(cc, 192)
		err = resp.Header.Read(br)
		if err != nil {
			t.Error(err)
		}

		var chunk [192]byte
		for {
			time.Sleep(time.Millisecond)
			br.Read(chunk[:]) //nolint:errcheck
		}
	}()
	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(sc)
	}()

	select {
	case err := <-ch:
		if err == nil || err != nil && !strings.EqualFold(err.Error(), "timeout") {
			t.Fatalf("Unexpected error from serveConn: %s", err)
		}
	case <-time.After(time.Second):
		t.Fatal("test timeout")
	}
}

func TestIncompleteBodyReturnsUnexpectedEOF(t *testing.T) {
	t.Parallel()

	rw := &readWriter{}
	rw.r.WriteString("POST /foo HTTP/1.1\r\nHost: google.com\r\nContent-Length: 5\r\n\r\n123")
	s := &Server{
		Handler: func(ctx *RequestCtx) {},
	}
	ch := make(chan error)
	go func() {
		ch <- s.ServeConn(rw)
	}()
	if err := <-ch; err == nil || err.Error() != "unexpected EOF" {
		t.Fatal(err)
	}
}

func verifyResponse(t *testing.T, r *bufio.Reader, expectedStatusCode int, expectedContentType, expectedBody string) *Response {
	var resp Response
	if err := resp.Read(r); err != nil {
		t.Fatalf("Unexpected error when parsing response: %s", err)
	}

	if !bytes.Equal(resp.Body(), []byte(expectedBody)) {
		t.Fatalf("Unexpected body %q. Expected %q", resp.Body(), []byte(expectedBody))
	}
	verifyResponseHeader(t, &resp.Header, expectedStatusCode, len(resp.Body()), expectedContentType)
	return &resp
}

type readWriter struct {
	net.Conn
	r bytes.Buffer
	w bytes.Buffer
}

func (rw *readWriter) Close() error {
	return nil
}

func (rw *readWriter) Read(b []byte) (int, error) {
	return rw.r.Read(b)
}

func (rw *readWriter) Write(b []byte) (int, error) {
	return rw.w.Write(b)
}

func (rw *readWriter) RemoteAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) LocalAddr() net.Addr {
	return zeroTCPAddr
}

func (rw *readWriter) SetDeadline(t time.Time) error {
	return nil
}

func (rw *readWriter) SetReadDeadline(t time.Time) error {
	return nil
}

func (rw *readWriter) SetWriteDeadline(t time.Time) error {
	return nil
}

type testLogger struct {
	lock sync.Mutex
	out  string
}

func (cl *testLogger) Printf(format string, args ...interface{}) {
	cl.lock.Lock()
	cl.out += fmt.Sprintf(format, args...)[6:] + "\n"
	cl.lock.Unlock()
}
