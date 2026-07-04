// Package pprofhandler provides a fasthttp handler similar to net/http/pprof.
package pprofhandler

import (
	"bytes"
	"net/http/pprof"
	rtp "runtime/pprof"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

var (
	cmdline = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Cmdline)
	profile = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Profile)
	symbol  = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Symbol)
	trace   = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Trace)
	index   = fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Index)
)

var (
	cmdlinePath = []byte("/debug/pprof/cmdline")
	profilePath = []byte("/debug/pprof/profile")
	symbolPath  = []byte("/debug/pprof/symbol")
	tracePath   = []byte("/debug/pprof/trace")
)

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
// Paths ending in a trailing slash (e.g. /debug/pprof/heap/) return 404,
// matching the behaviour of net/http/pprof's DefaultServeMux which does
// not register handlers for paths ending in /.
func PprofHandler(ctx *fasthttp.RequestCtx) {
	path := ctx.Path()

	// Reject paths ending in / — net/http/pprof returns 404 for these.
	if len(path) > 1 && path[len(path)-1] == '/' {
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
