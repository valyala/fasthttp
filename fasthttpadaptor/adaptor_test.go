package fasthttpadaptor

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestNewFastHTTPHandler(t *testing.T) {
	t.Parallel()

	expectedMethod := fasthttp.MethodPost
	expectedProto := "HTTP/1.1"
	expectedProtoMajor := 1
	expectedProtoMinor := 1
	expectedRequestURI := "/foo/bar?baz=123"
	expectedBody := "<!doctype html><html>"
	expectedContentLength := len(expectedBody)
	expectedHost := "foobar.com"
	expectedRemoteAddr := "1.2.3.4:6789"
	expectedHeader := map[string]string{
		"Foo-Bar":         "baz",
		"Abc":             "defg",
		"XXX-Remote-Addr": "123.43.4543.345",
	}
	expectedURL, err := url.ParseRequestURI(expectedRequestURI)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedContextKey := "contextKey"
	expectedContextValue := "contextValue"
	expectedContentType := "text/html; charset=utf-8"

	callsCount := 0
	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		callsCount++
		if r.Method != expectedMethod {
			t.Fatalf("unexpected method %q. Expecting %q", r.Method, expectedMethod)
		}
		if r.Proto != expectedProto {
			t.Fatalf("unexpected proto %q. Expecting %q", r.Proto, expectedProto)
		}
		if r.ProtoMajor != expectedProtoMajor {
			t.Fatalf("unexpected protoMajor %d. Expecting %d", r.ProtoMajor, expectedProtoMajor)
		}
		if r.ProtoMinor != expectedProtoMinor {
			t.Fatalf("unexpected protoMinor %d. Expecting %d", r.ProtoMinor, expectedProtoMinor)
		}
		if r.RequestURI != expectedRequestURI {
			t.Fatalf("unexpected requestURI %q. Expecting %q", r.RequestURI, expectedRequestURI)
		}
		if r.ContentLength != int64(expectedContentLength) {
			t.Fatalf("unexpected contentLength %d. Expecting %d", r.ContentLength, expectedContentLength)
		}
		if len(r.TransferEncoding) != 0 {
			t.Fatalf("unexpected transferEncoding %q. Expecting []", r.TransferEncoding)
		}
		if r.Host != expectedHost {
			t.Fatalf("unexpected host %q. Expecting %q", r.Host, expectedHost)
		}
		if r.RemoteAddr != expectedRemoteAddr {
			t.Fatalf("unexpected remoteAddr %q. Expecting %q", r.RemoteAddr, expectedRemoteAddr)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatalf("unexpected error when reading request body: %v", err)
		}
		if string(body) != expectedBody {
			t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
		}
		if !reflect.DeepEqual(r.URL, expectedURL) {
			t.Fatalf("unexpected URL: %#v. Expecting %#v", r.URL, expectedURL)
		}
		if r.Context().Value(expectedContextKey) != expectedContextValue {
			t.Fatalf("unexpected context value for key %q. Expecting %q", expectedContextKey, expectedContextValue)
		}

		for k, expectedV := range expectedHeader {
			v := r.Header.Get(k)
			if v != expectedV {
				t.Fatalf("unexpected header value %q for key %q. Expecting %q", v, k, expectedV)
			}
		}

		w.Header().Set("Header1", "value1")
		w.Header().Set("Header2", "value2")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(body) //nolint:errcheck
	}
	fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH))
	fasthttpH = setContextValueMiddleware(fasthttpH, expectedContextKey, expectedContextValue)

	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod(expectedMethod)
	req.SetRequestURI(expectedRequestURI)
	req.Header.SetHost(expectedHost)
	req.BodyWriter().Write([]byte(expectedBody)) //nolint:errcheck
	for k, v := range expectedHeader {
		req.Header.Set(k, v)
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", expectedRemoteAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx.Init(&req, remoteAddr, nil)

	fasthttpH(&ctx)

	if callsCount != 1 {
		t.Fatalf("unexpected callsCount: %d. Expecting 1", callsCount)
	}

	resp := &ctx.Response
	if resp.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("unexpected statusCode: %d. Expecting %d", resp.StatusCode(), fasthttp.StatusBadRequest)
	}
	if string(resp.Header.Peek("Header1")) != "value1" {
		t.Fatalf("unexpected header value: %q. Expecting %q", resp.Header.Peek("Header1"), "value1")
	}
	if string(resp.Header.Peek("Header2")) != "value2" {
		t.Fatalf("unexpected header value: %q. Expecting %q", resp.Header.Peek("Header2"), "value2")
	}
	if string(resp.Body()) != expectedBody {
		t.Fatalf("unexpected response body %q. Expecting %q", resp.Body(), expectedBody)
	}
	if string(resp.Header.Peek("Content-Type")) != expectedContentType {
		t.Fatalf("unexpected response content-type %q. Expecting %q", string(resp.Header.Peek("Content-Type")), expectedContentType)
	}
}

func TestNewFastHTTPHandlerFunc(t *testing.T) {
	t.Parallel()

	expectedMethod := fasthttp.MethodPost
	expectedProto := "HTTP/1.1"
	expectedProtoMajor := 1
	expectedProtoMinor := 1
	expectedRequestURI := "/foo/bar?baz=123"
	expectedBody := "<!doctype html><html>"
	expectedContentLength := len(expectedBody)
	expectedHost := "foobar.com"
	expectedRemoteAddr := "1.2.3.4:6789"
	expectedHeader := map[string]string{
		"Foo-Bar":         "baz",
		"Abc":             "defg",
		"XXX-Remote-Addr": "123.43.4543.345",
	}
	expectedURL, err := url.ParseRequestURI(expectedRequestURI)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedContextKey := "contextKey"
	expectedContextValue := "contextValue"
	expectedContentType := "text/html; charset=utf-8"

	callsCount := 0
	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		callsCount++
		if r.Method != expectedMethod {
			t.Fatalf("unexpected method %q. Expecting %q", r.Method, expectedMethod)
		}
		if r.Proto != expectedProto {
			t.Fatalf("unexpected proto %q. Expecting %q", r.Proto, expectedProto)
		}
		if r.ProtoMajor != expectedProtoMajor {
			t.Fatalf("unexpected protoMajor %d. Expecting %d", r.ProtoMajor, expectedProtoMajor)
		}
		if r.ProtoMinor != expectedProtoMinor {
			t.Fatalf("unexpected protoMinor %d. Expecting %d", r.ProtoMinor, expectedProtoMinor)
		}
		if r.RequestURI != expectedRequestURI {
			t.Fatalf("unexpected requestURI %q. Expecting %q", r.RequestURI, expectedRequestURI)
		}
		if r.ContentLength != int64(expectedContentLength) {
			t.Fatalf("unexpected contentLength %d. Expecting %d", r.ContentLength, expectedContentLength)
		}
		if len(r.TransferEncoding) != 0 {
			t.Fatalf("unexpected transferEncoding %q. Expecting []", r.TransferEncoding)
		}
		if r.Host != expectedHost {
			t.Fatalf("unexpected host %q. Expecting %q", r.Host, expectedHost)
		}
		if r.RemoteAddr != expectedRemoteAddr {
			t.Fatalf("unexpected remoteAddr %q. Expecting %q", r.RemoteAddr, expectedRemoteAddr)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			t.Fatalf("unexpected error when reading request body: %v", err)
		}
		if string(body) != expectedBody {
			t.Fatalf("unexpected body %q. Expecting %q", body, expectedBody)
		}
		if !reflect.DeepEqual(r.URL, expectedURL) {
			t.Fatalf("unexpected URL: %#v. Expecting %#v", r.URL, expectedURL)
		}
		if r.Context().Value(expectedContextKey) != expectedContextValue {
			t.Fatalf("unexpected context value for key %q. Expecting %q", expectedContextKey, expectedContextValue)
		}

		for k, expectedV := range expectedHeader {
			v := r.Header.Get(k)
			if v != expectedV {
				t.Fatalf("unexpected header value %q for key %q. Expecting %q", v, k, expectedV)
			}
		}

		w.Header().Set("Header1", "value1")
		w.Header().Set("Header2", "value2")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(body) //nolint:errcheck
	}
	fasthttpH := NewFastHTTPHandlerFunc(nethttpH)
	fasthttpH = setContextValueMiddleware(fasthttpH, expectedContextKey, expectedContextValue)

	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod(expectedMethod)
	req.SetRequestURI(expectedRequestURI)
	req.Header.SetHost(expectedHost)
	req.BodyWriter().Write([]byte(expectedBody)) //nolint:errcheck
	for k, v := range expectedHeader {
		req.Header.Set(k, v)
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", expectedRemoteAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx.Init(&req, remoteAddr, nil)

	fasthttpH(&ctx)

	if callsCount != 1 {
		t.Fatalf("unexpected callsCount: %d. Expecting 1", callsCount)
	}

	resp := &ctx.Response
	if resp.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("unexpected statusCode: %d. Expecting %d", resp.StatusCode(), fasthttp.StatusBadRequest)
	}
	if string(resp.Header.Peek("Header1")) != "value1" {
		t.Fatalf("unexpected header value: %q. Expecting %q", resp.Header.Peek("Header1"), "value1")
	}
	if string(resp.Header.Peek("Header2")) != "value2" {
		t.Fatalf("unexpected header value: %q. Expecting %q", resp.Header.Peek("Header2"), "value2")
	}
	if string(resp.Body()) != expectedBody {
		t.Fatalf("unexpected response body %q. Expecting %q", resp.Body(), expectedBody)
	}
	if string(resp.Header.Peek("Content-Type")) != expectedContentType {
		t.Fatalf("unexpected response content-type %q. Expecting %q", string(resp.Header.Peek("Content-Type")), expectedContentType)
	}
}

func TestNewFastHTTPHandlerWithCookies(t *testing.T) {
	expectedMethod := fasthttp.MethodPost
	expectedRequestURI := "/foo/bar?baz=123"
	expectedHost := "foobar.com"
	expectedRemoteAddr := "1.2.3.4:6789"

	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod(expectedMethod)
	req.SetRequestURI(expectedRequestURI)
	req.Header.SetHost(expectedHost)
	req.Header.SetCookie("cookieOne", "valueCookieOne")
	req.Header.SetCookie("cookieTwo", "valueCookieTwo")

	remoteAddr, err := net.ResolveTCPAddr("tcp", expectedRemoteAddr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx.Init(&req, remoteAddr, nil)

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		// real handler warped by middleware, in this example do nothing
	}
	fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH))

	netMiddleware := func(_ http.ResponseWriter, r *http.Request) {
		// assume middleware do some change on r, such as reset header's host
		r.Header.Set("Host", "example.com")
		// convert ctx again in case request may modify by middleware
		ctx.Request.Header.Set("Host", r.Header.Get("Host"))
		// since cookies of r are not changed, expect "cookieOne=valueCookieOne"
		cookie, _ := r.Cookie("cookieOne")
		if err != nil {
			// will error, but if line 172 is commented, then no error will happen
			t.Errorf("should not error")
		}
		if cookie.Value != "valueCookieOne" {
			t.Errorf("cookie error, expect %s, find %s", "valueCookieOne", cookie.Value)
		}
		// instead of using responseWriter and r, use ctx again, like what have done in fiber
		fasthttpH(&ctx)
	}
	fastMiddleware := NewFastHTTPHandler(http.HandlerFunc(netMiddleware))
	fastMiddleware(&ctx)
}

func setContextValueMiddleware(next fasthttp.RequestHandler, key string, value any) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.SetUserValue(key, value)
		next(ctx)
	}
}

func TestHijack(t *testing.T) {
	t.Parallel()

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Hijacker); !ok {
			t.Errorf("expected http.ResponseWriter to implement http.Hijacker")
		} else {
			if _, err := w.Write([]byte("foo")); err != nil {
				t.Error(err)
			}

			if c, rw, err := f.Hijack(); err != nil {
				t.Error(err)
			} else {
				if _, err := rw.WriteString("bar"); err != nil {
					t.Error(err)
				}

				if err := rw.Flush(); err != nil {
					t.Error(err)
				}

				if err := c.Close(); err != nil {
					t.Error(err)
				}
			}
		}
	}

	s := &fasthttp.Server{
		Handler: NewFastHTTPHandler(http.HandlerFunc(nethttpH)),
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		buf, err := io.ReadAll(c)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if string(buf) != "foobar" {
			t.Errorf("unexpected response: %q. Expecting %q", buf, "foobar")
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestFlushHandler(t *testing.T) {
	t.Parallel()

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); !ok {
			t.Errorf("expected http.ResponseWriter to implement http.Flusher")
		} else {
			if _, err := w.Write([]byte("foo")); err != nil {
				t.Error(err)
			}

			f.Flush()

			time.Sleep(time.Second)

			if _, err := w.Write([]byte("bar")); err != nil {
				t.Error(err)
			}

			f.Flush()
		}
	}

	s := &fasthttp.Server{
		Handler: NewFastHTTPHandler(http.HandlerFunc(nethttpH)),
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		time.AfterFunc(500*time.Millisecond, func() {
			c.Close()
		})
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Errorf("unexpected error reading response: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Errorf("unexpected status code: %d. Expecting %d", resp.StatusCode, http.StatusOK)
		}

		if resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Errorf("unexpected Content-Type header: %q. Expecting %q", resp.Header.Get("Content-Type"), "text/plain; charset=utf-8")
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Errorf("unexpected error reading body: %v", err)
		}

		if string(body) != "foo" {
			t.Errorf("unexpected response body: %q. Expecting %q", body, "foo")
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestHijackFlush(t *testing.T) {
	t.Parallel()

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Hijacker); !ok {
			t.Errorf("expected http.ResponseWriter to implement http.Hijacker")
		} else {
			if _, err := w.Write([]byte("foo")); err != nil {
				t.Error(err)
			}

			if c, rw, err := f.Hijack(); err != nil {
				t.Error(err)
			} else {
				if _, err := rw.WriteString("bar"); err != nil {
					t.Error(err)
				}

				if err := rw.Flush(); err != nil {
					t.Error(err)
				}

				time.Sleep(time.Second)

				if _, err := rw.WriteString("bazz"); err != nil {
					t.Error(err)
				}

				if err := rw.Flush(); err != nil {
					t.Error(err)
				}

				if err := c.Close(); err != nil {
					t.Error(err)
				}
			}
		}
	}

	s := &fasthttp.Server{
		Handler: NewFastHTTPHandler(http.HandlerFunc(nethttpH)),
	}

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	clientCh := make(chan struct{})
	go func() {
		c, err := ln.Dial()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if _, err = c.Write([]byte("GET / HTTP/1.1\r\nHost: aa\r\n\r\n")); err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		time.AfterFunc(500*time.Millisecond, func() {
			c.Close()
		})
		buf, err := io.ReadAll(c)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		if string(buf) != "foobar" {
			t.Errorf("unexpected response: %q. Expecting %q", buf, "foobar")
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestResourceRecyclingUnderLoad_OneEndpoint(t *testing.T) {
	t.Parallel()

	handler := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello World!")
	}

	s := &fasthttp.Server{
		Handler: NewFastHTTPHandler(http.HandlerFunc(handler)),
	}

	requestCount := 10
	responseTimeout := 500 * time.Millisecond
	expectedBody := "Hello World!"

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	for reqID := 1; reqID <= requestCount; reqID++ {
		req := httptest.NewRequest("GET", "/", http.NoBody)
		resp, err := sendRequest(ln, req, responseTimeout)
		if err != nil {
			t.Errorf("[%d] unexpected error sending request: %v", reqID, err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Errorf("[%d] unexpected error reading body: %v", reqID, err)
		}

		if string(body) != expectedBody {
			t.Errorf("[%d] unexpected response: %q. Expecting %q", reqID, body, expectedBody)
		}
	}
}

func TestResourceRecyclingUnderLoad_MultipleEndpoints(t *testing.T) {
	t.Parallel()

	handlers := []struct {
		endpoint     string
		handler      fasthttp.RequestHandler
		expectedBody string
	}{
		{
			endpoint: "/done",
			handler: NewFastHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "Hello World!")
			})),
			expectedBody: "Hello World!",
		},
		{
			endpoint: "/flush",
			handler: NewFastHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if f, ok := w.(http.Flusher); ok {
					w.Write([]byte("foo")) //nolint:errcheck
					f.Flush()
					time.Sleep(250 * time.Millisecond)
					w.Write([]byte("bar")) //nolint:errcheck
					f.Flush()
				} else {
					http.Error(w, "Flusher not supported", http.StatusInternalServerError)
				}
			})),
			expectedBody: "foobar",
		},
		{
			endpoint: "/hijack",
			handler: NewFastHTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hj, ok := w.(http.Hijacker); ok {
					conn, rw, err := hj.Hijack()
					if err != nil {
						t.Errorf("unexpected error: %v", err)
						return
					}
					defer conn.Close()
					rw.WriteString("hijacked") //nolint:errcheck
					rw.Flush()
				} else {
					http.Error(w, "Hijacker not supported", http.StatusInternalServerError)
				}
			})),
			expectedBody: "hijacked",
		},
	}

	s := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			for _, h := range handlers {
				if string(ctx.Path()) == h.endpoint {
					h.handler(ctx)
					return
				}
			}
			ctx.Error("Not Found", fasthttp.StatusNotFound)
		},
	}

	repeatCount := 3
	responseTimeout := 500 * time.Millisecond

	ln := fasthttputil.NewInmemoryListener()

	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}()

	for range repeatCount {
		for _, handler := range handlers {
			req := httptest.NewRequest("GET", handler.endpoint, http.NoBody)
			resp, err := sendRequest(ln, req, responseTimeout)
			if err != nil {
				t.Errorf("[%s] unexpected error sending request: %v", handler.endpoint, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil && err != io.ErrUnexpectedEOF {
				t.Errorf("[%s] unexpected error reading body: %v", handler.endpoint, err)
			}
			if string(body) != handler.expectedBody {
				t.Errorf("[%s] unexpected response: %q. Expecting %q", handler.endpoint, body, handler.expectedBody)
			}
		}
	}
}

func sendRequest(ln *fasthttputil.InmemoryListener, req *http.Request, responseTimeout time.Duration) (*http.Response, error) {
	c, err := ln.Dial()
	if err != nil {
		return nil, err
	}

	dump, err := httputil.DumpRequest(req, true)
	if err != nil {
		return nil, err
	}

	if _, err := c.Write(dump); err != nil {
		return nil, err
	}

	time.AfterFunc(responseTimeout, func() {
		c.Close()
	})
	body, err := io.ReadAll(c)
	if err != nil {
		return nil, err
	}

	// Attempt to read as a response.
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(body)), nil)
	if err != nil {
		// Might be a hijacked connection. Read as a bare-bones body.
		resp = &http.Response{
			Body: io.NopCloser(bytes.NewReader(body)),
		}
	}
	return resp, nil
}
