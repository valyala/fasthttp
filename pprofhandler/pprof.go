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

// PprofHandler serves server runtime profiling data in the format expected by the pprof visualization tool.
//
// See https://pkg.go.dev/net/http/pprof for details.
func PprofHandler(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Content-Type", "text/html")
	switch {
	case bytes.HasPrefix(ctx.Path(), []byte("/debug/pprof/cmdline")):
		cmdline(ctx)
	case bytes.HasPrefix(ctx.Path(), []byte("/debug/pprof/profile")):
		profile(ctx)
	case bytes.HasPrefix(ctx.Path(), []byte("/debug/pprof/symbol")):
		symbol(ctx)
	case bytes.HasPrefix(ctx.Path(), []byte("/debug/pprof/trace")):
		trace(ctx)
	default:
		for _, v := range rtp.Profiles() {
			ppName := v.Name()
			if bytes.HasPrefix(ctx.Path(), []byte("/debug/pprof/"+ppName)) {
				namedHandler := fasthttpadaptor.NewFastHTTPHandlerFunc(pprof.Handler(ppName).ServeHTTP)
				namedHandler(ctx)
				return
			}
		}
		index(ctx)
	}
}
