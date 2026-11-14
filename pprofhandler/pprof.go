// Package pprofhandler provides a fasthttp handler similar to net/http/pprof.
package pprofhandler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/pprof"
	rtp "runtime/pprof"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

var (
	cmdline = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Cmdline)
	profile = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Profile)
	symbol  = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Symbol)
	trace   = fasthttpadaptor.NewFastHTTPHandlerFunc(streamTrace)
	index   = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Index)
)

var (
	cmdlinePath = []byte("/debug/pprof/cmdline")
	profilePath = []byte("/debug/pprof/profile")
	symbolPath  = []byte("/debug/pprof/symbol")
	tracePath   = []byte("/debug/pprof/trace")
	indexPath   = []byte("/debug/pprof/")
)

// The trace writer runs on a runtime goroutine. Flush before writing to
// switch the adaptor to streaming, and wake pprof.Trace if the pipe closes.
type traceWriter struct {
	http.ResponseWriter

	cancel context.CancelFunc
}

func (w *traceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *traceWriter) Write(p []byte) (int, error) {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.cancel()
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err
}

func streamTrace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	pprof.Trace(&traceWriter{w, cancel}, r.WithContext(ctx))
}

// matchPprofPath checks whether path exactly matches a pprof endpoint.
// It only accepts the exact path (e.g. /debug/pprof/heap) and rejects
// paths with a trailing slash, matching the behaviour of net/http/pprof
// which returns 404 for paths ending in /.
func matchPprofPath(path, endpoint []byte) bool {
	return bytes.Equal(path, endpoint)
}

// PprofHandler serves server runtime profiling data in the format expected by the pprof visualization tool.
//
// See https://pkg.go.dev/net/http/pprof for details.
//
// Execution traces are streamed as they are recorded, so the server write
// timeout must allow for the requested trace duration.
//
// Paths ending in a trailing slash (e.g. /debug/pprof/heap/) return 404,
// matching the behaviour of net/http/pprof's DefaultServeMux which does
// not register handlers for paths ending in /. The index at /debug/pprof/
// is served, as in net/http/pprof.
func PprofHandler(ctx *fasthttp.RequestCtx) {
	path := ctx.Path()

	// Reject paths ending in / — net/http/pprof returns 404 for these.
	if len(path) > 1 && path[len(path)-1] == '/' && !bytes.Equal(path, indexPath) {
		ctx.SetStatusCode(404)
		return
	}

	ctx.Response.Header.Set("Content-Type", "text/html")
	switch {
	case matchPprofPath(path, cmdlinePath):
		cmdline(ctx)
	case matchPprofPath(path, profilePath):
		profile(ctx)
	case matchPprofPath(path, symbolPath):
		symbol(ctx)
	case matchPprofPath(path, tracePath):
		trace(ctx)
	default:
		for _, v := range rtp.Profiles() {
			ppName := v.Name()
			endpoint := []byte("/debug/pprof/" + ppName)
			if matchPprofPath(path, endpoint) {
				namedHandler := fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Handler(ppName).ServeHTTP)
				namedHandler(ctx)
				return
			}
		}
		index(ctx)
	}
}
