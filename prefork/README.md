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
