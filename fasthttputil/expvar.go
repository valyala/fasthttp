package fasthttputil

import (
	"expvar"
	"fmt"

	"github.com/valyala/fasthttp"
)

// ExpvarHandler dumps json representation of expvars to http response.
//
// See https://golang.org/pkg/expvar/ for details.
func ExpvarHandler(ctx *fasthttp.RequestCtx) {
	ctx.Response.Reset()

	fmt.Fprintf(ctx, "{\n")
	first := true
	expvar.Do(func(kv expvar.KeyValue) {
		if !first {
			fmt.Fprintf(ctx, ",\n")
		}
		first = false
		fmt.Fprintf(ctx, "%q: %s", kv.Key, kv.Value)
	})
	fmt.Fprintf(ctx, "\n}\n")

	ctx.SetContentType("application/json; charset=utf-8")
}
