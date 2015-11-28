# fasthttp
Fast HTTP implementation for Go.

Currently fasthttp is successfully used in a production serving up to 1M
concurrent keep-alive connections doing 100K qps from a single server.

[Documentation](https://godoc.org/github.com/valyala/fasthttp)

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

* *Which GO versions are supported by fasthttp?*

  Go1.5+. Older versions won't be supported, since their standard package
  [miss useful functions](https://github.com/valyala/fasthttp/issues/5).

* *Please provide real benchmark data and sever information*

  See [this issue](https://github.com/valyala/fasthttp/issues/4).
