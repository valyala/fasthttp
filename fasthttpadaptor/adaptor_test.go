package fasthttpadaptor

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/valyala/fasthttp"
	"golang.org/x/sync/errgroup"
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
		t.Fatalf("unexpected response content-type %q. Expecting %q", string(resp.Header.Peek("Content-Type")), expectedBody)
	}
}

func setContextValueMiddleware(next fasthttp.RequestHandler, key string, value interface{}) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.SetUserValue(key, value)
		next(ctx)
	}
}

func TestHijackInterface1(t *testing.T) {
	g := errgroup.Group{}

	var (
		reqCtx fasthttp.RequestCtx
		req    fasthttp.Request
	)

	client, server := net.Pipe()
	testmsgc2s := "hello from a client"
	testmsgs2c := "hello from a hijacked request"

	reqCtx.Init2(server, nil, true)
	req.CopyTo(&reqCtx.Request)

	nethttpH := func(w http.ResponseWriter, r *http.Request) {
		if h, ok := w.(http.Hijacker); !ok {
			t.Fatalf("response writer do not support hijack interface")
		} else if netConn, _, err := h.Hijack(); err != nil {
			t.Fatalf("invoking Hijack failed: %s", err)
		} else if netConn == nil {
			t.Fatalf("invalid conn handler for hijack invokation")
		} else {
			readMsg := make([]byte, len(testmsgc2s))
			n, err := io.ReadAtLeast(netConn, readMsg, len(testmsgc2s))
			if err != nil {
				t.Fatalf("server: error on read from conn: %s", err)
			}
			if n != len(testmsgc2s) || testmsgc2s != string(readMsg) {
				t.Fatalf("server: mismatch on message recieved: expected: (%d)<%s>, actual: (%d)<%s>\n", len(testmsgc2s), testmsgc2s, n, string(readMsg))
			}
			n, err = io.WriteString(netConn, testmsgs2c)
			if err != nil {
				t.Fatalf("server: error on write to conn: %s", err)
			}
			if n != len(testmsgs2c) {
				t.Fatalf("server: mismatch on message sent size: expected: (%d), actual: (%d)\n", len(testmsgc2s), n)
			}
			netConn.Close()
		}
	}

	g.Go(func() error {
		n, err := io.WriteString(client, testmsgc2s)
		if err != nil {
			return fmt.Errorf("client: error on write to conn: %s\n", err)
		}
		if n != len(testmsgc2s) {
			return fmt.Errorf("client: mismatch on send all: expected: %d, actual: %d\n", len(testmsgc2s), n)
		}
		readMsg := make([]byte, len(testmsgs2c))
		n, err = io.ReadAtLeast(client, readMsg, len(testmsgs2c))
		if err != nil {
			return fmt.Errorf("client: error on read from conn: %s", err)
		}
		if n != len(testmsgs2c) || testmsgs2c != string(readMsg) {
			return fmt.Errorf("client: mismatch on message recieved: expected: (%d)<%s>, actual: (%d)<%s>\n", len(testmsgs2c), testmsgs2c, n, string(readMsg))
		}
		return nil
	})

	g.Go(func() error {
		fasthttpH := NewFastHTTPHandler(http.HandlerFunc(nethttpH), func(c net.Conn) {})
		fasthttpH(&reqCtx)
		if !reqCtx.Hijacked() {
			t.Fatal("request was not hijacked")
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}
