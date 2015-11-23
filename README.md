# fasthttp
Fast HTTP implementation for Go.

Currently fasthttp is successfully used in a production serving up to 1M
concurrent keep-alive connections doing 50K qps from a single server.

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
* Either do not keep references to RequestCtx members after returning
  from RequestHandler or call RequestCtx.TimeoutError() before returning
  from RequestHandler.
* [Profile your program](http://blog.golang.org/profiling-go-programs)
  in production.
  `go tool pprof --alloc_objects your-program mem.pprof` usually gives better
  insights for optimization than `go tool pprof your-program cpu.pprof`.
* Write [tests and benchmarks](https://golang.org/pkg/testing/) for hot paths.
