package fasthttpadaptor

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
		if _, err := w.Write(body); err != nil {
			t.Fatalf("unexpected error when writing response body: %v", err)
		}
	}
	fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH))
	fasthttpH = setContextValueMiddleware(fasthttpH, expectedContextKey, expectedContextValue)

	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod(expectedMethod)
	req.SetRequestURI(expectedRequestURI)
	req.Header.SetHost(expectedHost)
	if _, err := req.BodyWriter().Write([]byte(expectedBody)); err != nil {
		t.Fatalf("unexpected error when writing request body: %v", err)
	}
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
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", "6")
			w.Header().Set("X-Foo", "bar")

			if _, err := w.Write([]byte("foo")); err != nil {
				t.Error(err)
			}

			f.Flush()

			time.Sleep(time.Millisecond * 500)

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

		if string(body) != "foobar" {
			t.Errorf("unexpected response body: %q. Expecting %q", body, "foobar")
		}

		close(clientCh)
	}()

	select {
	case <-clientCh:
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestFlushHandlerClosed(t *testing.T) {
	t.Parallel()

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); !ok {
			t.Errorf("expected http.ResponseWriter to implement http.Flusher")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", "6")
			w.Header().Set("X-Foo", "bar")

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
				// Flushing the ResponseWriter after Hijack must not block.
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}

				if _, err := rw.WriteString("bar"); err != nil {
					t.Error(err)
				}

				if err := rw.Flush(); err != nil {
					t.Error(err)
				}

				time.Sleep(time.Second)

				_ = c.Close()
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

func TestResourceRecyclingUnderLoadOneEndpoint(t *testing.T) {
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
		body, err := sendRequest(ln, req, responseTimeout)
		if err != nil {
			t.Errorf("[%d] unexpected error sending request: %v", reqID, err)
		}
		if string(body) != expectedBody {
			t.Errorf("[%d] unexpected response: %q. Expecting %q", reqID, body, expectedBody)
		}
	}
}

func TestResourceRecyclingUnderLoadMultipleEndpoints(t *testing.T) {
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
					if _, err := w.Write([]byte("foo")); err != nil {
						t.Error(err)
					}
					f.Flush()
					time.Sleep(250 * time.Millisecond)
					if _, err := w.Write([]byte("bar")); err != nil {
						t.Error(err)
					}
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
					if _, err := rw.WriteString("hijacked"); err != nil {
						t.Errorf("unexpected error: %v", err)
					}
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
			body, err := sendRequest(ln, req, responseTimeout)
			if err != nil {
				t.Errorf("[%s] unexpected error sending request: %v", handler.endpoint, err)
			}
			if string(body) != handler.expectedBody {
				t.Errorf("[%s] unexpected response: %q. Expecting %q", handler.endpoint, body, handler.expectedBody)
			}
		}
	}
}

func sendRequest(ln *fasthttputil.InmemoryListener, req *http.Request, responseTimeout time.Duration) ([]byte, error) {
	c, err := ln.Dial()
	if err != nil {
		return nil, err
	}

	if err := req.Write(c); err != nil {
		return nil, err
	}

	time.AfterFunc(responseTimeout, func() {
		c.Close()
	})
	response, err := io.ReadAll(c)
	if err != nil {
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(response)), nil)
	if err != nil {
		// Hijacked response, return the full response instead of the parsed body.
		return response, nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
}

func TestNewFastHTTPHandlerPanic(t *testing.T) {
	var ctx fasthttp.RequestCtx
	var req fasthttp.Request

	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetRequestURI("/")
	req.Header.SetHost("example.com")

	remoteAddr, err := net.ResolveTCPAddr("tcp", "1.2.3.4:6789")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx.Init(&req, remoteAddr, nil)

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}
	fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH))

	defer func() {
		recover() //nolint:errcheck
	}()

	fasthttpH(&ctx)

	t.Error("expected panic, but it didn't happen")
}

func TestWriterWriteRechecksStreamingReadyAfterLock(t *testing.T) {
	var ctx fasthttp.RequestCtx
	w := acquireWriter(&ctx)
	defer releaseWriter(w)

	w.mu.Lock()
	writeCh := make(chan error, 1)
	go func() {
		_, err := w.Write([]byte("late"))
		writeCh <- err
	}()

	time.Sleep(10 * time.Millisecond)
	// Flush builds the pipe before it makes streaming ready; do the same here.
	w.pr, w.pw = io.Pipe()
	close(w.streamReady)

	readCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len("late"))
		if _, err := io.ReadFull(w.pr, buf); err != nil {
			readCh <- nil
			return
		}
		readCh <- buf
	}()

	w.mu.Unlock()

	select {
	case body := <-readCh:
		if string(body) != "late" {
			t.Fatalf("unexpected streamed body %q. Expecting %q", body, "late")
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for streamed write")
	}

	select {
	case err := <-writeCh:
		if err != nil {
			t.Fatalf("unexpected write error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for Write")
	}
}

func TestNewFastHTTPHandlerPreservesPresetStatusCode(t *testing.T) {
	t.Parallel()

	// Verify that a status code set on ctx before calling NewFastHTTPHandler
	// is preserved when the net/http handler does not call WriteHeader.
	nethttpH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	fasthttpH := NewFastHTTPHandler(nethttpH)

	var ctx fasthttp.RequestCtx
	ctx.Request.SetRequestURI("/test")
	// Pre-set a status code that should survive through the adaptor.
	ctx.Response.SetStatusCode(fasthttp.StatusForbidden)

	fasthttpH(&ctx)

	if ctx.Response.StatusCode() != fasthttp.StatusForbidden {
		t.Fatalf("unexpected status code: %d. Expecting %d (pre-set code should be preserved)",
			ctx.Response.StatusCode(), fasthttp.StatusForbidden)
	}
	if string(ctx.Response.Body()) != "ok" {
		t.Fatalf("unexpected body: %q. Expecting %q", string(ctx.Response.Body()), "ok")
	}
}

func TestNewFastHTTPHandlerPresetStatusCodeOverriddenByHandler(t *testing.T) {
	t.Parallel()

	// If the net/http handler explicitly calls WriteHeader, that should take precedence
	// over any pre-set status code.
	nethttpH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	})

	fasthttpH := NewFastHTTPHandler(nethttpH)

	var ctx fasthttp.RequestCtx
	ctx.Request.SetRequestURI("/test")
	ctx.Response.SetStatusCode(fasthttp.StatusForbidden)

	fasthttpH(&ctx)

	// WriteHeader(404) should override the pre-set 403.
	if ctx.Response.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("unexpected status code: %d. Expecting %d (handler's WriteHeader should win)",
			ctx.Response.StatusCode(), fasthttp.StatusNotFound)
	}
}

// A client vanishing mid-stream must not race the request's teardown.
func TestHandlerStreamClientDisconnect(t *testing.T) {
	t.Parallel()

	s := &fasthttp.Server{Handler: NewFastHTTPHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("chunk"))
		w.(http.Flusher).Flush() //nolint:forcetypeassert
		for range 50 {
			if _, err := w.Write([]byte("more")); err != nil {
				return
			}
		}
	})}
	ln := fasthttputil.NewInmemoryListener()
	go s.Serve(ln) //nolint:errcheck
	defer ln.Close()

	for range 50 {
		c, err := ln.Dial()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		_, _ = c.Read(buf)
		_ = c.Close()
	}
}

// A HEAD response discards the stream while CompressHandler's goroutine may
// still be reading it; the pre-flush buffer must not be recycled under it.
func TestHandlerFlushedHeadWithCompression(t *testing.T) {
	t.Parallel()

	s := &fasthttp.Server{Handler: fasthttp.CompressHandler(NewFastHTTPHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 32*1024))
		w.(http.Flusher).Flush() //nolint:forcetypeassert
	}))}
	ln := fasthttputil.NewInmemoryListener()
	go s.Serve(ln) //nolint:errcheck
	defer ln.Close()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			c, err := ln.Dial()
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			br := bufio.NewReader(c)
			for range 100 {
				if _, err := c.Write([]byte("HEAD / HTTP/1.1\r\nHost: a\r\nAccept-Encoding: gzip\r\n\r\n")); err != nil {
					t.Error(err)
					return
				}
				var resp fasthttp.Response
				resp.SkipBody = true
				if err := resp.Read(br); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// panic(http.ErrAbortHandler) gives up on the response without crashing.
func TestHandlerAbortIsQuiet(t *testing.T) {
	t.Parallel()

	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(fasthttp.MethodGet)
	ctx.Request.SetRequestURI("/")
	ctx.Request.Header.SetHost("example.com")

	NewFastHTTPHandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})(ctx)
}

// An abort before the first write sends nothing, and the server tears the
// connection down exactly once — through the per-IP wrapper too.
func TestHandlerAbortWithPerIPLimit(t *testing.T) {
	t.Parallel()

	for _, keep := range []bool{false, true} {
		s := &fasthttp.Server{
			Handler: NewFastHTTPHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/abort" {
					panic(http.ErrAbortHandler)
				}
				_, _ = w.Write([]byte("ok"))
			}),
			MaxConnsPerIP:     1,
			KeepHijackedConns: keep,
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go s.Serve(ln) //nolint:errcheck

		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("GET /abort HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if b, err := io.ReadAll(c); err != nil || len(b) != 0 {
			t.Fatalf("keep=%v: aborted response carried %q (err=%v), want nothing", keep, b, err)
		}
		_ = c.Close()

		// The per-IP slot must free up and the server must still be serving.
		var follow string
		for range 50 {
			c2, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			_, _ = c2.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n"))
			_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
			b, _ := io.ReadAll(c2)
			_ = c2.Close()
			follow = string(b)
			if strings.Contains(follow, " 200 ") && strings.HasSuffix(follow, "ok") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(follow, " 200 ") || !strings.HasSuffix(follow, "ok") {
			t.Fatalf("keep=%v: follow-up response %q, want a 200 with body ok", keep, follow)
		}
		_ = ln.Close()
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) Printf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logRecorder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// A panic after a Flush must neither hang the client on a body that never
// completes nor let the connection serve a pipelined request, as in net/http;
// an abort stays quiet, any other panic is logged once. Under CompressHandler
// the response completes, but the log and the close remain.
func TestHandlerPanicAfterFlush(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		prefix     string
		panic      any
		compressed bool
		wantLog    string
	}{
		{"abort", "partial", http.ErrAbortHandler, false, ""},
		{"abort-headers-only", "", http.ErrAbortHandler, false, ""},
		{"panic", "partial", "boom", false, "panic in net/http handler: boom"},
		{"panic-eof", "partial", io.ErrUnexpectedEOF, false, "panic in net/http handler: unexpected EOF"},
		{"abort-compressed", "partial", http.ErrAbortHandler, true, ""},
		{"panic-compressed", "partial", "boom", true, "panic in net/http handler: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var secondServed atomic.Bool
			logs := &logRecorder{}
			closed := make(chan struct{}, 1)
			h := NewFastHTTPHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					secondServed.Store(true)
					return
				}
				_, _ = w.Write([]byte(tc.prefix))
				w.(http.Flusher).Flush() //nolint:forcetypeassert
				panic(tc.panic)
			})
			if tc.compressed {
				h = fasthttp.CompressHandler(h)
			}
			s := &fasthttp.Server{
				Handler: h,
				Logger:  logs,
				ConnState: func(_ net.Conn, st fasthttp.ConnState) {
					if st == fasthttp.StateClosed {
						select {
						case closed <- struct{}{}:
						default:
						}
					}
				},
			}
			ln := fasthttputil.NewInmemoryListener()
			go s.Serve(ln) //nolint:errcheck
			defer ln.Close()

			c, err := ln.Dial()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write([]byte(
				"GET / HTTP/1.1\r\nHost: a\r\nAccept-Encoding: gzip\r\n\r\nGET /second HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				br := bufio.NewReader(c)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					done <- fmt.Errorf("reading the flushed headers: %w", err)
					return
				}
				defer resp.Body.Close()
				first := make([]byte, len(tc.prefix))
				if _, err := io.ReadFull(resp.Body, first); err != nil {
					done <- err
					return
				}
				if _, err := io.ReadAll(resp.Body); err == nil && !tc.compressed {
					done <- errors.New("the aborted body ended in a clean EOF")
					return
				}
				if resp2, err := http.ReadResponse(br, nil); err == nil {
					_ = resp2.Body.Close()
					done <- errors.New("the connection served a second response after the abort")
					return
				}
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the body of an aborted flushed response never ended")
			}
			if secondServed.Load() {
				t.Fatal("the handler ran for a pipelined request after the abort")
			}
			// The server logs, if at all, before it reports the connection closed.
			<-closed
			if got := logs.String(); !strings.Contains(got, tc.wantLog) || (tc.wantLog == "" && got != "") {
				t.Fatalf("server log = %q, want %q", got, tc.wantLog)
			}
		})
	}
}

// Flush() without a body puts the headers on the wire, as in net/http.
func TestHandlerFlushSendsHeaders(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	s := &fasthttp.Server{Handler: NewFastHTTPHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Early", "1")
		w.(http.Flusher).Flush() //nolint:forcetypeassert
		<-release
	})}
	ln := fasthttputil.NewInmemoryListener()
	go s.Serve(ln) //nolint:errcheck
	defer ln.Close()
	defer close(release)

	c, err := ln.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	headers := make(chan error, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			headers <- err
			return
		}
		defer resp.Body.Close()
		if resp.Header.Get("X-Early") != "1" {
			headers <- fmt.Errorf("X-Early = %q, want 1", resp.Header.Get("X-Early"))
			return
		}
		headers <- nil
		// Let the handler finish so Close can drain the body.
		<-release
	}()
	select {
	case err := <-headers:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the flushed headers did not arrive while the handler was still running")
	}
}
