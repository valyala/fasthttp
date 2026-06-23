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

// pprofPrefix is the common prefix for all pprof paths.
var pprofPrefix = []byte("/debug/pprof/")

// matchPprofPath checks whether path exactly matches a pprof endpoint.
// It accepts the exact path (e.g. /debug/pprof/heap) or the path with a
// trailing slash (e.g. /debug/pprof/heap/), matching the behaviour of
// net/http/pprof's DefaultServeMux registrations.
func matchPprofPath(path, endpoint []byte) bool {
	if bytes.Equal(path, endpoint) {
		return true
	}
	// Allow trailing slash: /debug/pprof/heap/ matches /debug/pprof/heap
	if len(path) == len(endpoint)+1 && path[len(path)-1] == '/' && bytes.Equal(path[:len(path)-1], endpoint) {
		return true
	}
	return false
}

// PprofHandler serves server runtime profiling data in the format expected by the pprof visualization tool.
//
// See https://pkg.go.dev/net/http/pprof for details.
func PprofHandler(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Type", "text/html")
	switch {
	case matchPprofPath(ctx.Path(), cmdlinePath):
		cmdline(ctx)
	case matchPprofPath(ctx.Path(), profilePath):
		profile(ctx)
	case matchPprofPath(ctx.Path(), symbolPath):
		symbol(ctx)
	case matchPprofPath(ctx.Path(), tracePath):
		trace(ctx)
	default:
		for _, v := range rtp.Profiles() {
			ppName := v.Name()
			endpoint := []byte("/debug/pprof/" + ppName)
			if matchPprofPath(ctx.Path(), endpoint) {
				namedHandler := fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Handler(ppName).ServeHTTP)
				namedHandler(ctx)
				return
			}
		}
		index(ctx)
	}
}