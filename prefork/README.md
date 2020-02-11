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
    // Simulates some hard work
    time.Sleep(100 * time.Millisecond)
}
```

Test command:

```bash
$ wrk -H 'Host: localhost' -H 'Accept: text/plain,text/html;q=0.9,application/xhtml+xml;q=0.9,application/xml;q=0.8,*/*;q=0.7' -H 'Connection: keep-alive' --latency -d 15 -c 512 --timeout 8 -t 4 http://localhost:8080
```

Results:

- prefork

```bash
Running 15s test @ http://localhost:8080
  4 threads and 512 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     4.75ms    4.27ms 126.24ms   97.45%
    Req/Sec    26.46k     4.16k   71.18k    88.72%
  Latency Distribution
     50%    4.55ms
     75%    4.82ms
     90%    5.46ms
     99%   15.49ms
  1581916 requests in 15.09s, 140.30MB read
  Socket errors: connect 0, read 318, write 0, timeout 0
Requests/sec: 104861.58
Transfer/sec:      9.30MB
```

- **non**-prefork

```bash
Running 15s test @ http://localhost:8080
  4 threads and 512 connections
  Thread Stats   Avg      Stdev     Max   +/- Stdev
    Latency     6.42ms   11.83ms 177.19ms   96.42%
    Req/Sec    24.96k     5.83k   56.83k    82.93%
  Latency Distribution
     50%    4.53ms
     75%    4.93ms
     90%    6.94ms
     99%   74.54ms
  1472441 requests in 15.09s, 130.59MB read
  Socket errors: connect 0, read 265, write 0, timeout 0
Requests/sec:  97553.34
Transfer/sec:      8.65MB
```
