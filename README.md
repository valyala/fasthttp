# fasthttp
Fast HTTP implementation for Go.

Currently fasthttp is successfully used in a production serving 100K rps from 1M
concurrent keep-alive connections on a single server.

[![Build Status](https://travis-ci.org/valyala/fasthttp.svg)](https://travis-ci.org/valyala/fasthttp)

[Documentation](https://godoc.org/github.com/valyala/fasthttp)

[Examples from docs](https://godoc.org/github.com/valyala/fasthttp#pkg-examples)

[Code examples](examples)

[Switching from net/http to fasthttp](#switching-from-nethttp-to-fasthttp)

[Fasthttp best practices](#fasthttp-best-practices)

[Tricks with byte buffers](#tricks-with-byte-buffers)

[FAQ](#faq)

# HTTP server performance comparison with [net/http](https://golang.org/pkg/net/http/)

In short, fasthttp server is up to 10 times faster than net/http. Below are benchmark results.

GOMAXPROCS=1

net/http:
```
$ GOMAXPROCS=1 go test -bench=NetHTTPServerGet -benchmem -benchtime=5s
PASS
BenchmarkNetHTTPServerGet1ReqPerConn            	  300000	     21236 ns/op	    2404 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn            	  500000	     14634 ns/op	    2371 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn           	 1000000	      9447 ns/op	    2101 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn          	 1000000	      7939 ns/op	    2033 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn10KClients  	  300000	     30291 ns/op	    4589 B/op	      31 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn10KClients  	  500000	     23199 ns/op	    3581 B/op	      25 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn10KClients 	  500000	     13270 ns/op	    2621 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet100ReqPerConn10KClients	  500000	     11412 ns/op	    2119 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=1 go test -bench=kServerGet -benchmem -benchtime=5s
PASS
BenchmarkServerGet1ReqPerConn            	 3000000	      2341 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn            	 5000000	      1799 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn           	 5000000	      1239 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn          	10000000	      1090 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn10KClients  	 3000000	      2860 ns/op	       4 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn10KClients  	 3000000	      1992 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn10KClients 	 5000000	      1297 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet100ReqPerConn10KClients	10000000	      1264 ns/op	       9 B/op	       0 allocs/op
```

GOMAXPROCS=4

net/http:
```
$ GOMAXPROCS=4 go test -bench=NetHTTPServerGet -benchmem -benchtime=5s
PASS
BenchmarkNetHTTPServerGet1ReqPerConn-4            	 1000000	      5545 ns/op	    2433 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn-4            	 2000000	      4147 ns/op	    2398 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn-4           	 3000000	      2628 ns/op	    2118 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn-4          	 3000000	      2304 ns/op	    2037 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn10KClients-4  	 1000000	      7327 ns/op	    3561 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn10KClients-4  	 1000000	      5952 ns/op	    3073 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn10KClients-4 	 2000000	      4345 ns/op	    2530 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet100ReqPerConn10KClients-4	 2000000	      3866 ns/op	    2132 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=4 go test -bench=kServerGet -benchmem -benchtime=5s
PASS
BenchmarkServerGet1ReqPerConn-4            	10000000	      1053 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn-4            	10000000	       685 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn-4           	20000000	       393 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn-4          	20000000	       338 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn10KClients-4  	10000000	      1033 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn10KClients-4  	10000000	       668 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn10KClients-4 	20000000	       393 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet100ReqPerConn10KClients-4	20000000	       384 ns/op	       4 B/op	       0 allocs/op
```

# HTTP client comparison with net/http

In short, fasthttp client is up to 10 times faster than net/http. Below are benchmark results.

GOMAXPROCS=1

net/http:
```
$ GOMAXPROCS=1 go test -bench='HTTPClient(Do|GetEndToEnd)' -benchmem -benchtime=5s
PASS
BenchmarkNetHTTPClientDoFastServer	  500000	     17535 ns/op	    2624 B/op	      38 allocs/op
BenchmarkNetHTTPClientGetEndToEnd 	  200000	     56593 ns/op	    5012 B/op	      59 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=1 go test -bench='kClient(Do|GetEndToEnd)' -benchmem -benchtime=5s
PASS
BenchmarkClientDoFastServer	 5000000	      1420 ns/op	       0 B/op	       0 allocs/op
BenchmarkClientGetEndToEnd 	  500000	     17912 ns/op	       0 B/op	       0 allocs/op
```

GOMAXPROCS=4

net/http:
```
$ GOMAXPROCS=4 go test -bench='HTTPClient(Do|GetEndToEnd)' -benchmem -benchtime=5s
PASS
BenchmarkNetHTTPClientDoFastServer-4	 1000000	      5795 ns/op	    2626 B/op	      38 allocs/op
BenchmarkNetHTTPClientGetEndToEnd-4 	  500000	     19304 ns/op	    5953 B/op	      62 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=4 go test -bench='kClient(Do|GetEndToEnd)' -benchmem -benchtime=5s
PASS
BenchmarkClientDoFastServer-4	20000000	       443 ns/op	       0 B/op	       0 allocs/op
BenchmarkClientGetEndToEnd-4 	 1000000	      5954 ns/op	       0 B/op	       0 allocs/op
```

# Switching from net/http to fasthttp

Unfortunately, fasthttp doesn't provide API identical to net/http.
See the [FAQ](#faq) for details.
There is [net/http -> fasthttp handler converter](https://godoc.org/github.com/valyala/fasthttp/fasthttpadaptor),
but it is advisable writing fasthttp request handlers by hands for gaining
all the fasthttp advantages (especially high performance :) ).

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
	switch string(ctx.Path()) {
	case "/foo":
		fooHandler(ctx)
	case "/bar":
		barHandler(ctx)
	default:
		ctx.Error("Unsupported path", fasthttp.StatusNotFound)
	}
}
```

* Fasthttp allows setting response headers and writing response body
in arbitrary order. There is no 'headers first, then body' restriction
like in net/http. The following code is valid for fasthttp:
```go
requestHandler := func(ctx *fasthttp.RequestCtx) {
	// set some headers and status code first
	ctx.SetContentType("foo/bar")
	ctx.SetStatusCode(fasthttp.StatusOK)

	// then write the first part of body
	fmt.Fprintf(ctx, "this is the first part of body\n")

	// then set more headers
	ctx.Response.Header.Set("Foo-Bar", "baz")

	// then write more body
	fmt.Fprintf(ctx, "this is the second part of body\n")

	// then override already written body
	ctx.SetBody([]byte("this is completely new body contents"))

	// then update status code
	ctx.SetStatusCode(fasthttp.StatusNotFound)

	// basically, anything may be updated many times before
	// returning from RequestHandler.
	//
	// Unlike net/http fasthttp doesn't put response to the wire until
	// returning from RequestHandler.
}
```

* Fasthttp doesn't provide [ServeMux](https://golang.org/pkg/net/http/#ServeMux),
since I believe third-party request routers like
[fasthttprouter](https://github.com/buaazp/fasthttprouter) must be used instead,
Net/http code with simple ServeMux is trivially converted
to fasthttp code:

```go
// net/http code

m := &http.ServeMux{}
m.HandleFunc("/foo", fooHandlerFunc)
m.HandleFunc("/bar", barHandlerFunc)
m.Handle("/baz", bazHandler)

http.ListenAndServe(":80", m)
```

```go
// the corresponding fasthttp code
m := func(ctx *fasthttp.RequestCtx) {
	switch string(ctx.Path()) {
	case "/foo":
		fooHandlerFunc(ctx)
	case "/bar":
		barHandlerFunc(ctx)
	case "/baz":
		bazHandler.HandlerFunc(ctx)
	default:
		ctx.Error("not found", fasthttp.StatusNotFound)
	}
}

fastttp.ListenAndServe(":80", m)
```

* net/http -> fasthttp conversion table:

  * All the pseudocode below assumes w, r and ctx have these types:
  ```go
	var (
		w http.ResponseWriter
		r *http.Request
		ctx *fasthttp.RequestCtx
	)
  ```
  * r.Body -> [ctx.PostBody()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostBody)
  * r.URL.Path -> [ctx.Path()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Path)
  * r.URL -> [ctx.URI()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.URI)
  * r.Method -> [ctx.Method()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Method)
  * r.Header -> [ctx.Request.Header](https://godoc.org/github.com/valyala/fasthttp#RequestHeader)
  * r.Header.Get() -> [ctx.Request.Header.Peek()](https://godoc.org/github.com/valyala/fasthttp#RequestHeader.Peek)
  * r.Host -> [ctx.Host()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Host)
  * r.Form -> [ctx.QueryArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.QueryArgs) +
  [ctx.PostArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostArgs)
  * r.PostForm -> [ctx.PostArgs()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.PostArgs)
  * r.FormValue() -> [ctx.FormValue()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.FormValue)
  * r.FormFile() -> [ctx.FormFile()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.FormFile)
  * r.MultipartForm -> [ctx.MultipartForm()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.MultipartForm)
  * r.RemoteAddr -> [ctx.RemoteAddr()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.RemoteAddr)
  * r.RequestURI -> [ctx.RequestURI()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.RequestURI)
  * r.TLS -> [ctx.IsTLS()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.IsTLS)
  * r.Cookie() -> [ctx.Request.Header.Cookie()](https://godoc.org/github.com/valyala/fasthttp#RequestHeader.Cookie)
  * r.Referer() -> [ctx.Referer()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Referer)
  * r.UserAgent() -> [ctx.UserAgent()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.UserAgent)
  * w.Header() -> [ctx.Response.Header](https://godoc.org/github.com/valyala/fasthttp#ResponseHeader)
  * w.Header().Set() -> [ctx.Response.Header.Set()](https://godoc.org/github.com/valyala/fasthttp#ResponseHeader.Set)
  * w.Header().Set("Content-Type") -> [ctx.SetContentType()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetContentType)
  * w.Header().Set("Set-Cookie") -> [ctx.Response.Header.SetCookie()](https://godoc.org/github.com/valyala/fasthttp#ResponseHeader.SetCookie)
  * w.Write() -> [ctx.Write()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Write),
  [ctx.SetBody()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetBody),
  [ctx.SetBodyStream()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetBodyStream),
  [ctx.SetBodyStreamWriter()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetBodyStreamWriter)
  * w.WriteHeader() -> [ctx.SetStatusCode()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SetStatusCode)
  * w.(http.Hijacker).Hijack() -> [ctx.Hijack()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Hijack)
  * http.Error() -> [ctx.Error()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Error)
  * http.FileServer() -> [fasthttp.FSHandler()](https://godoc.org/github.com/valyala/fasthttp#FSHandler),
  [fasthttp.FS](https://godoc.org/github.com/valyala/fasthttp#FS)
  * http.ServeFile() -> [ctx.SendFile()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.SendFile)
  * http.Redirect() -> [ctx.Redirect()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.Redirect)
  * http.NotFound() -> [ctx.NotFound()](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.NotFound)
  * http.StripPrefix() -> [fasthttp.PathRewriteFunc](https://godoc.org/github.com/valyala/fasthttp#PathRewriteFunc)

* *VERY IMPORTANT!* Fasthttp disallows holding references
to [RequestCtx](https://godoc.org/github.com/valyala/fasthttp#RequestCtx) or to its'
members after returning from [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler).
Otherwise [data races](http://blog.golang.org/race-detector) are inevitable.
Carefully inspect all the net/http request handlers converted to fasthttp whether
they retain references to RequestCtx or to its' members after returning.
RequestCtx provides the following _band aids_ for this case:

  * Wrap RequestHandler into [TimeoutHandler](https://godoc.org/github.com/valyala/fasthttp#TimeoutHandler).
  * Call [TimeoutError](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.TimeoutError)
  before returning from RequestHandler if there are references to RequestCtx or to its' members.
  See [the example](https://godoc.org/github.com/valyala/fasthttp#example-RequestCtx-TimeoutError)
  for more details.

Use brilliant tool - [race detector](http://blog.golang.org/race-detector) -
for detecting and eliminating data races in your program. If you detected
data race related to fasthttp in your program, then there is high probability
you forgot calling [TimeoutError](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.TimeoutError)
before returning from [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler).

* Blind switching from net/http to fasthttp won't give you performance boost.
While fasthttp is optimized for speed, its' performance may be easily saturated
by slow [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler).
So [profile](http://blog.golang.org/profiling-go-programs) and optimize your
code after switching to fasthttp.


# Performance optimization tips for multi-core systems

* Use [reuseport](https://godoc.org/github.com/valyala/fasthttp/reuseport) listener.
* Run a separate server instance per CPU core with GOMAXPROCS=1.
* Pin each server instance to a separate CPU core using [taskset](http://linux.die.net/man/1/taskset).
* Ensure the interrupts of multiqueue network card are evenly distributed between CPU cores.
  See [this article](https://blog.cloudflare.com/how-to-achieve-low-latency/) for details.


# Fasthttp best practices

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


# Tricks with `[]byte` buffers

The following tricks are used by fasthttp. Use them in your code too.

* Standard Go functions accept nil buffers
```go
var (
	// both buffers are uninitialized
	dst []byte
	src []byte
)
dst = append(dst, src...)  // is legal if dst is nil and/or src is nil
copy(dst, src)  // is legal if dst is nil and/or src is nil
(string(src) == "")  // is true if src is nil
(len(src) == 0)  // is true if src is nil
src = src[:0]  // works like a charm with nil src

// this for loop doesn't panic if src is nil
for i, ch := range src {
	doSomething(i, ch)
}
```

So throw away nil checks for `[]byte` buffers from you code. For example,
```go
srcLen := 0
if src != nil {
	srcLen = len(src)
}
```

becomes

```go
srcLen := len(src)
```

* String may be appended to `[]byte` buffer with `append`
```go
dst = append(dst, "foobar"...)
```

* `[]byte` buffer may be extended to its' capacity.
```go
buf := make([]byte, 100)
a := buf[:10]  // len(a) == 10, cap(a) == 100.
b := a[:100]  // is valid, since cap(a) == 100.
```

* All fasthttp functions accept nil `[]byte` buffer
```go
statusCode, body, err := fasthttp.Get(nil, "http://google.com/")
uintBuf := fasthttp.AppendUint(nil, 1234)
```

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
  for implementing these goodies. See [the first third-party websocket implementation on the top of fasthttp](https://github.com/leavengood/websocket).

* *Are there known net/http advantages comparing to fasthttp?*

  Yes:
  * net/http supports [HTTP/2.0 starting from go1.6](https://http2.golang.org/).
  * net/http API is stable, while fasthttp API constantly evolves.
  * net/http handles more HTTP corner cases.
  * net/http should contain less bugs, since it is used and tested by much
    wider audience.
  * Many existing web frameworks and request routers are built on top
    of net/http.
  * net/http works on Go older than 1.5.

* *Which GO versions are supported by fasthttp?*

  Go1.5+. Older versions won't be supported, since their standard package
  [miss useful functions](https://github.com/valyala/fasthttp/issues/5).

* *Please provide real benchmark data and sever information*

  See [this issue](https://github.com/valyala/fasthttp/issues/4).

* *Are there plans to add request routing to fasthttp?*

  There are no plans to add request routing into fasthttp. I believe request
  routing must be implemented in a separate package(s) like
  [httprouter](https://github.com/julienschmidt/httprouter).
  Try [fasthttprouter](https://github.com/buaazp/fasthttprouter),
  httprouter fork for fasthttp.
  See also [this issue](https://github.com/valyala/fasthttp/issues/8) for more info.

* *I detected data race in fasthttp!*

  Cool! [File a bug](https://github.com/valyala/fasthttp/issues/new). But before
  doing this check the following in your code:

  * Make sure there are no references to [RequestCtx](https://godoc.org/github.com/valyala/fasthttp#RequestCtx)
  or to its' members after returning from [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler).
  * Make sure you call [TimeoutError](https://godoc.org/github.com/valyala/fasthttp#RequestCtx.TimeoutError)
  before returning from [RequestHandler](https://godoc.org/github.com/valyala/fasthttp#RequestHandler)
  if there are references to [RequestCtx](https://godoc.org/github.com/valyala/fasthttp#RequestCtx)
  or to its' members, which may be accessed by other goroutines.
