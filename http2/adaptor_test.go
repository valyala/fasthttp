package http2

import (
	"errors"
	"io"
	stdhttp "net/http"
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// A net/http handler can't hijack a multiplexed connection; the adaptor
// reports that instead of handing out the shared connection.
func TestAdaptorHijackOverHTTP2(t *testing.T) {
	handler := fasthttpadaptor.NewFastHTTPHandler(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		hijacker, ok := w.(stdhttp.Hijacker)
		if !ok {
			stdhttp.Error(w, "not a Hijacker", stdhttp.StatusInternalServerError)
			return
		}
		conn, _, err := hijacker.Hijack()
		if conn != nil {
			_ = conn.Close()
			stdhttp.Error(w, "hijack handed out a connection", stdhttp.StatusInternalServerError)
			return
		}
		if !errors.Is(err, stdhttp.ErrNotSupported) {
			stdhttp.Error(w, "unexpected error: "+err.Error(), stdhttp.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "not hijacked")
	}))
	s := newTestServer(t, &fasthttp.Server{Handler: handler}, ServerConfig{})
	for range 2 {
		resp, err := s.client.Get(s.URL("/"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "not hijacked" {
			t.Fatalf("status %d body %q", resp.StatusCode, body)
		}
	}
}
