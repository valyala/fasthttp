# fasthttp
Fast HTTP implementation for Go.

Currently fasthttp is successfully used in a production serving up to 1M
concurrent keep-alive connections doing 100K qps from a single server.

[Documentation](https://godoc.org/github.com/valyala/fasthttp)

[Examples](https://godoc.org/github.com/valyala/fasthttp#pkg-examples)

# HTTP server performance comparison with [net/http](https://golang.org/pkg/net/http/)

In short, fasthttp is up to 10 times faster than net/http. Below are benchmark results.

GOMAXPROCS=1

net/http:
```
$ GOMAXPROCS=1 go test -bench=NetHTTPServerGet -benchmem
PASS
BenchmarkNetHTTPServerGet1ReqPerConn           	   50000	     21057 ns/op	    2409 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn           	  100000	     13772 ns/op	    2373 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn          	  200000	      8511 ns/op	    2102 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn         	  200000	      7501 ns/op	    2034 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn1KClients  	  100000	     22480 ns/op	    2734 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn1KClients  	  100000	     15380 ns/op	    2555 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn1KClients 	  100000	     11185 ns/op	    2759 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn1KClients	  200000	      7989 ns/op	    2034 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=1 go test -bench=kServerGet -benchmem
PASS
BenchmarkServerGet1ReqPerConn           	 1000000	      2395 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn           	 1000000	      1897 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn          	 1000000	      1285 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn         	 1000000	      1110 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn1KClients  	 1000000	      2186 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn1KClients  	 1000000	      1902 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn1KClients 	 1000000	      1343 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn1KClients	 1000000	      1124 ns/op	       1 B/op	       0 allocs/op
```

GOMAXPROCS=4

net/http:
```
$ GOMAXPROCS=4 go test -bench=NetHTTPServerGet -benchmem
PASS
BenchmarkNetHTTPServerGet1ReqPerConn-4           	  200000	      6207 ns/op	    2434 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn-4           	  300000	      4158 ns/op	    2398 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn-4          	  500000	      2603 ns/op	    2119 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn-4         	 1000000	      2225 ns/op	    2037 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn1KClients-4  	  200000	      5972 ns/op	    2496 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn1KClients-4  	  300000	      4309 ns/op	    2461 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn1KClients-4 	  500000	      3787 ns/op	    2533 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn1KClients-4	  500000	      2350 ns/op	    2037 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=4 go test -bench=kServerGet -benchmem
PASS
BenchmarkServerGet1ReqPerConn-4           	 1000000	      1038 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn-4           	 2000000	       764 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn-4          	 3000000	       388 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn-4         	 5000000	       334 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn1KClients-4  	 1000000	      1123 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn1KClients-4  	 2000000	       759 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn1KClients-4 	 3000000	       440 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn1KClients-4	 5000000	       342 ns/op	       0 B/op	       0 allocs/op
```

# Switching from [net/http](https://golang.org/pkg/net/http/) to fasthttp

Unfortunately, fasthttp doesn't provide API identical to net/http.
See the [FAQ](#faq) for details.

Important points:

* Fasthttp works with [RequestHandler functions](https://godoc.org/github.com/valyala/fasthttp#RequestHandler)
instead of objects implementing [Handler interface](https://golang.org/pkg/net/http/#Handler).
Fortunately, it is easy to pass bound struct methods to fasthttp:

```go
type MyHandler struct {
	foobar string
}

// request handler in net/http style, i.e. method bound to MyHandler struct.
func (h *MyHandler) HandleFastHTTP(ctx *fasthttp.RequestCtx) {
	// notice that we may access MyHandler properties here - see h.foobar.
	fmt.Fprintf(ctx, "Hello, world! Requested path is %q. Foobar is %q",
		ctx.Path(), h.foobar)
}

// request handler in fasthttp style, i.e. just plain function.
func fastHTTPHandler(ctx *fasthttp.RequestCtx) {
	fmt.Fprintf(ctx, "Hi there! RequestURI is %q", ctx.RequestURI())
}

// pass bound struct method to fasthttp
myHandler := &MyHandler{
	foobar: "foobar",
}
fasthttp.ListenAndServe(":8080", myHandler.HandleFastHTTP)

// pass plain function to fasthttp
fasthttp.ListenAndServe(":8081", fastHTTPHandler)
```

* The [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler)
accepts only one argument - [RequestCtx](https://godoc.org/github.com/valyala/fasthttp#RequestCtx).
It contains all the functionality required for http request processing
and response writing. Below is an example of a simple request handler conversion
from net/http to fasthttp.

```go
// net/http request handler
requestHandler := func(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/foo":
		fooHandler(w, r)
	case "/bar":
		barHandler(w, r)
	default:
		http.Error(w, "Unsupported path", http.StatusNotFound)
	}
}
```

```go
// the corresponding fasthttp request handler
requestHandler := func(ctx *fasthttp.RequestCtx) {
	path := ctx.Path()
	switch {
	case string(path) == "/foo":
		fooHandler(ctx)
	case string(path) == "/bar":
		barHandler(ctx)
	default:
		ctx.Error("Unsupported path", fasthttp.StatusNotFound)
	}
}
```

* net/http -> fasthttp conversion cheat sheet:

```go
// all the pseudocode below assumes w, r and ctx have these types
var (
	w http.ResponseWriter
	r *http.Request
	ctx *fasthttp.RequestCtx
)
```

  * r.Body -> [ctx.Request.Body()](https://godoc.org/github.com/valyala/fasthttp#Request.Body)
  * r.URL.Path -> [ctx.Path()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Path)
  * r.URL -> [ctx.URI()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.URI)
  * r.Method -> [ctx.Method()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Method)
  * r.Header -> [ctx.Request.Header](https://godoc.org/github.com/valyala/fasthttp#RequestHeader)
  * r.Host -> [ctx.Host()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Host)
  * r.Form -> [ctx.QueryArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.QueryArgs) +
  [ctx.PostArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostArgs)
  * r.PostForm -> [ctx.PostArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostArgs)
  * r.MultipartForm -> [ctx.MultipartForm()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.MultipartForm)
  * r.RemoteAddr -> [ctx.RemoteAddr()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.RemoteAddr)
  * r.RequestURI -> [ctx.RequestURI()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.RequestURI)
  * r.TLS -> [ctx.IsTLS()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.IsTLS)
  * r.Cookie() -> [ctx.Request.Header.Cookie()](https://godoc.org/github.com/valyala/fasthttp#RequestHeader.Cookie)
  * r.Referer() -> [ctx.Referer()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Referer)
  * r.UserAgent() -> [ctx.Request.Header.UserAgent()](https://godoc.org/github.com/valyala/fasthttp#RequestHeader.UserAgent)
  * w.Header() -> [ctx.Response.Header](https://godoc.org/github.com/valyala/fasthttp#ResponseHeader)
  * w.Write() -> [ctx.Write()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Write),
  [ctx.SetBody()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetBody),
  [ctx.SetBodyStream](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetBodyStream)
  * w.WriteHeader() -> [ctx.SetStatusCode()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetStatusCode) +
  [ctx.Response.Header](https://godoc.org/github.com/valyala/fasthttp#ResponseHeader) +
  [ctx.Write()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Write)
  * w.Hijack() -> [ctx.Hijack()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Hijack)
  * http.Error() -> [ctx.Error()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Error)
  * Fasthttp allows setting response headers and writing response body
  in arbitray order. There is no 'headers first, then body' restriction
  like in net/http.

* *VERY IMPORTANT NOTE* Fasthttp diallows holding references
to [RequestCtx](https://godoc.org/github.com/valyala/fasthttp#RequestCtx) or to its'
members after returning from [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler).
Otherwise [data races](http://blog.golang.org/race-detector) are unevitable.
Carefully inspect all the net/http request handlers converted to fasthttp whether
they retain references to RequestCtx or to its' members after returning.
RequestCtx provides the following _band aids_ for this case:

  * Wrap RequestHandler into [TimeoutHandler](https://godoc.org/github.com/valyala/fasthttp#TimeoutHandler).
  * Call [TimeoutError](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.TimeoutError)
  before returning from RequestHandler if there are references to RequestCtx or to its' members.
  See [the example](https://godoc.org/github.com/valyala/fasthttp#example-RequestCtx-TimeoutError)
  for more details.

Use brilliant tool - [race detector](http://blog.golang.org/race-detector) -
for detecting and eliminating data races in your program.


# Performance optimization tips for multi-core systems

* Use [reuseport](https://godoc.org/github.com/valyala/fasthttp/reuseport) listener.
* Run a separate server instance per CPU core with GOMAXPROCS=1.
* Pin each server instance to a separate CPU core using [taskset](http://linux.die.net/man/1/taskset).
* Ensure the interrupts of multiqueue network card are evenly distributed between CPU cores.
  See [this article](https://blog.cloudflare.com/how-to-achieve-low-latency/) for details.


# Fasthttp best practicies

* Do not allocate objects and `[]byte` buffers - just reuse them as much
  as possible. Fasthttp API design encourages this.
* [sync.Pool](https://golang.org/pkg/sync/#Pool) is your best friend.
* [Profile your program](http://blog.golang.org/profiling-go-programs)
  in production.
  `go tool pprof --alloc_objects your-program mem.pprof` usually gives better
  insights for optimization opportunities than `go tool pprof your-program cpu.pprof`.
* Write [tests and benchmarks](https://golang.org/pkg/testing/) for hot paths.
* Avoid conversion between `[]byte` and `string`, since this may result in memory
  allocation+copy. Fasthttp API provides functions for both `[]byte` and `string` -
  use these functions instead of converting manually between `[]byte` and `string`.
* Verify your tests and production code under
  [race detector](https://golang.org/doc/articles/race_detector.html) on a regular basis.


# FAQ

* *Why creating yet another http package instead of optimizing net/http?*

  Because net/http API limits many optimization opportunities.
  For example:
  * net/http Request object lifetime isn't limited by request handler execution
    time. So the server must create new request object per each request instead
    of reusing existing objects like fasthttp do.
  * net/http headers are stored in a `map[string][]string`. So the server
    must parse all the headers, convert them from `[]byte` to `string` and put
    them into the map before calling user-provided request handler.
    This all requires unnesessary memory allocations avoided by fasthttp.
  * net/http client API requires creating new response object per each request.

* *Why fasthttp API is incompatible with net/http?*

  Because net/http API limits many optimization opportunities. See the answer
  above for more details. Also certain net/http API parts are suboptimal
  for use:
  * Compare [net/http connection hijacking](https://golang.org/pkg/net/http/#Hijacker)
    to [fasthttp connection hijacking](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Hijack).
  * Compare [net/http Request.Body reading](https://golang.org/pkg/net/http/#Request)
    to [fasthttp request body reading](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostBody).

* *Why fasthttp doesn't support HTTP/2.0 and WebSockets?*

  There are [plans](TODO) for adding HTTP/2.0 and WebSockets support
  in the future.
  In the mean time, third parties may use [RequestCtx.Hijack](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Hijack)
  for implementing these goodies.

* *Are there known net/http advantages comparing to fasthttp?*

  Yes:
  * net/http supports [HTTP/2.0 starting from go1.6](https://http2.golang.org/).
  * net/http API is stable, while fasthttp API may change at any time.
  * net/http handles more HTTP corner cases.
  * net/http should contain less bugs, since it is used and tested by much
    wider user base.
  * Many existing web frameworks and request routers are built on top
    of net/http.
  * net/http works on go older than 1.5.

* *Which GO versions are supported by fasthttp?*

  Go1.5+. Older versions won't be supported, since their standard package
  [miss useful functions](https://github.com/valyala/fasthttp/issues/5).

* *Please provide real benchmark data and sever information*

  See [this issue](https://github.com/valyala/fasthttp/issues/4).

* *Are there plans to add request routing to fasthttp?*

  There are no plans to add request routing into fasthttp. I believe request
  routing must be implemented in a separate package(s) like
  [httprouter](https://github.com/julienschmidt/httprouter).
  See [this issue](https://github.com/valyala/fasthttp/issues/8) for more info.
