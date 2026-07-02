package pprofhandler

import (
	"net"
	"testing"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func TestPprofHandler_PathMatching(t *testing.T) {
	ln := fasthttputil.NewInmemoryListener()
	defer ln.Close()

	s := &fasthttp.Server{
		Handler: PprofHandler,
	}
	go func() {
		_ = s.Serve(ln)
	}()

	client := &fasthttp.Client{
		Dial: func(addr string) (net.Conn, error) {
			return ln.Dial()
		},
	}

	doReq := func(path string) (int, []byte) {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		req.SetRequestURI("http://localhost" + path)
		if err := client.Do(req, resp); err != nil {
			t.Fatalf("request to %s failed: %v", path, err)
		}
		return resp.StatusCode(), resp.Body()
	}

	// Exact paths should work
	tests := []struct {
		path       string
		wantStatus int
	}{
		{"/debug/pprof/", fasthttp.StatusOK},
		{"/debug/pprof/cmdline", fasthttp.StatusOK},
		{"/debug/pprof/symbol", fasthttp.StatusOK},
		{"/debug/pprof/heap", fasthttp.StatusOK},
	}
	for _, tt := range tests {
		status, _ := doReq(tt.path)
		if status != tt.wantStatus {
			t.Errorf("path %s: got status %d, want %d", tt.path, status, tt.wantStatus)
		}
	}

	// Malformed paths with extra suffixes should NOT return cmdline/profile/symbol/trace data.
	// The key test: compare response body of exact path vs. prefixed path.
	// If prefix matching were used, they'd return the same content.
	_, exactBody := doReq("/debug/pprof/cmdline")
	_, prefixBody := doReq("/debug/pprof/cmdlineEvil")
	if string(exactBody) == string(prefixBody) && len(exactBody) > 0 {
		t.Error("/debug/pprof/cmdlineEvil should not return the same content as /debug/pprof/cmdline")
	}

	_, exactBody = doReq("/debug/pprof/symbol")
	_, prefixBody = doReq("/debug/pprof/symbolEvil")
	if string(exactBody) == string(prefixBody) && len(exactBody) > 0 {
		t.Error("/debug/pprof/symbolEvil should not return the same content as /debug/pprof/symbol")
	}
}
