# Prefork

Server prefork implementation.

Preforks master process between several child processes increases performance, because Go doesn't have to share and manage memory between cores.

**WARNING: using prefork prevents the use of any global state!. Things like in-memory caches won't work.**

- How it works:

```go
import (
    "github.com/valyala/fasthttp"
    "github.com/valyala/fasthttp/prefork"
)

server := &fasthttp.Server{
    // Your configuration
}

// Wraps the server with prefork
preforkServer := prefork.New(server)

if err := preforkServer.ListenAndServe(":8080"); err != nil {
    panic(err)
}
```

## Benchmarks

Environment:

- Machine: MacBook Pro 13-inch, 2017
- OS: MacOS 10.15.3
- Go: go1.13.6 darwin/amd64

Handler code:

```go
func requestHandler(ctx *fasthttp.RequestCtx) {
    // Simulate some hard work
	time.Sleep(100 * time.Millisecond)
}
```

Results:

- **WITH** prefork

```bash
$ wrk -c 1000 -t 4 -d 30s http://localhost:8080
Running 30s test @ http://localhost:8080
  4 threads and 1000 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency   104.35ms    1.81ms 114.01ms   73.68%
    Req/Sec     2.40k   198.27     2.55k    94.19%
  286846 requests in 30.03s, 25.44MB read
  Socket errors: connect 0, read 62, write 0, timeout 0
Requests/sec:   9551.60
Transfer/sec:    867.48KB
```

- **WITHOUT** prefork

```bash
$ wrk -c 1000 -t 4 -d 30s http://localhost:8080
Running 30s test @ http://localhost:8080
  4 threads and 1000 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency   104.39ms    2.62ms 119.24ms   66.75%
    Req/Sec     2.13k   581.16     2.65k    83.23%
  253666 requests in 30.06s, 22.50MB read
  Socket errors: connect 0, read 903, write 88, timeout 0
Requests/sec:   8439.21
Transfer/sec:    766.45KB
```
