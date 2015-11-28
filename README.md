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
BenchmarkNetHTTPServerGet1ReqPerConn           	  100000	     21211 ns/op	    2407 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn           	  100000	     15682 ns/op	    2373 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn          	  200000	      9957 ns/op	    2103 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10000ReqPerConn       	  200000	      8243 ns/op	    2034 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn1KClients  	   50000	     23474 ns/op	    2704 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn1KClients  	  100000	     18124 ns/op	    2539 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn1KClients 	  100000	     11815 ns/op	    2689 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn1KClients	  200000	      9106 ns/op	    2034 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=1 go test -bench=kServerGet -benchmem
PASS
BenchmarkServerGet1ReqPerConn                  	  500000	      2495 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn                  	 1000000	      1925 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn                 	 1000000	      1300 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn                	 1000000	      1140 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn1KClients         	  500000	      2460 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn1KClients         	 1000000	      1962 ns/op	       1 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn1KClients        	 1000000	      1340 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn1KClients       	 1000000	      1180 ns/op	       0 B/op	       0 allocs/op
```

GOMAXPROCS=4

net/http:
```
$ GOMAXPROCS=4 go test -bench=NetHTTPServerGet -benchmem
PASS
BenchmarkNetHTTPServerGet1ReqPerConn-4           	  200000	      5929 ns/op	    2434 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn-4           	  300000	      4153 ns/op	    2399 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn-4          	  500000	      2751 ns/op	    2118 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10000ReqPerConn-4       	  500000	      2398 ns/op	    2037 B/op	      18 allocs/op
BenchmarkNetHTTPServerGet1ReqPerConn1KClients-4  	  200000	      5979 ns/op	    2494 B/op	      30 allocs/op
BenchmarkNetHTTPServerGet2ReqPerConn1KClients-4  	  300000	      4582 ns/op	    2457 B/op	      24 allocs/op
BenchmarkNetHTTPServerGet10ReqPerConn1KClients-4 	  300000	      3589 ns/op	    2537 B/op	      19 allocs/op
BenchmarkNetHTTPServerGet10KReqPerConn1KClients-4	  500000	      2465 ns/op	    2036 B/op	      18 allocs/op
```

fasthttp:
```
$ GOMAXPROCS=4 go test -bench=kServerGet -benchmem
PASS
BenchmarkServerGet1ReqPerConn-4                  	 2000000	      1094 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn-4                  	 2000000	       707 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn-4                 	 3000000	       417 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn-4                	 5000000	       351 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet1ReqPerConn1KClients-4         	 2000000	       916 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet2ReqPerConn1KClients-4         	 2000000	       655 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10ReqPerConn1KClients-4        	 3000000	       404 ns/op	       0 B/op	       0 allocs/op
BenchmarkServerGet10KReqPerConn1KClients-4       	 5000000	       359 ns/op	       0 B/op	       0 allocs/op
```

# Performance optimization tips for multi-core systems

* Use [reuseport](https://godoc.org/github.com/valyala/fasthttp/reuseport) listener.
* Run a separate server instance per CPU core with GOMAXPROCS=1.
* Pin each server instance to a separate CPU core using [taskset](http://linux.die.net/man/1/taskset).
* Ensure the interrupts of multiqueue network card are evenly distributed between CPU cores.
  See [this article](https://blog.cloudflare.com/how-to-achieve-low-latency/) for details.


# Fasthttp best practicies

* Do not allocate objects and buffers - just reuse them as much as possible.
  Fasthttp API design encourages this.
* [sync.Pool](https://golang.org/pkg/sync/#Pool) is your best friend.
* [Profile your program](http://blog.golang.org/profiling-go-programs)
  in production.
  `go tool pprof --alloc_objects your-program mem.pprof` usually gives better
  insights for optimization than `go tool pprof your-program cpu.pprof`.
* Write [tests and benchmarks](https://golang.org/pkg/testing/) for hot paths.
* Avoid conversion between []byte and string, since this may result in memory
  allocation+copy. Fasthttp API provides functions for both []byte and string -
  use these functions instead of converting manually between []byte and string.
* Verify your tests and production code under
  [race detector](https://golang.org/doc/articles/race_detector.html) on a regular basis.


# FAQ

* *Why creating yet another http package instead of optimizing net/http?*
  Because net/http API limits many optimization opportunities.
  For example:
  * net/http request lifetime isn't limited by request handler execution
    time. So the server creates new request object per each request instead
    of reusing existing object like fasthttp do.
  * net/http headers are stored in a `map[string][]string`. So the server
    must parse all the headers, convert them from `[]byte` to `string` and put
    them into the map before calling user-provided request handler.
    This all requires unnesessary memory allocations avoided by fasthttp.
  * net/http client API requires creating new response object for each request.

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
