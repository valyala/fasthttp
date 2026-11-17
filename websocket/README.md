# websocket

Package websocket implements the WebSocket protocol
([RFC 6455](https://www.rfc-editor.org/rfc/rfc6455)) for fasthttp servers and
clients, with optional permessage-deflate compression
([RFC 7692](https://www.rfc-editor.org/rfc/rfc7692)).

```go
import "github.com/valyala/fasthttp/websocket"
```

## Server

```go
func handler(ctx *fasthttp.RequestCtx) {
	err := websocket.Accept(ctx, nil, func(c *websocket.Conn) {
		for {
			typ, msg, err := c.Read(c.Context())
			if err != nil {
				return
			}
			if err := c.Write(c.Context(), typ, msg); err != nil {
				return
			}
		}
	})
	if err != nil {
		// Not a WebSocket handshake: Accept has written the error response.
		log.Printf("websocket: %v", err)
	}
}
```

The function given to `Accept` runs on the upgraded connection once the
request handler returns, and the connection is closed when it returns.

## Client

```go
c, err := websocket.Dial(ctx, "wss://example.com/echo", nil)
if err != nil {
	return err
}
defer c.CloseNow()

if err := c.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
	return err
}
_, msg, err := c.Read(ctx)
if err != nil {
	return err
}
fmt.Printf("%s\n", msg)

return c.Close(ctx, websocket.StatusNormalClosure, "")
```

## Usage notes

- **Messages.** `Read` and `Write` handle a whole message; `Reader` and
  `Writer` stream one. The bytes `Read` returns stay valid until the next
  `Read` or `Reader` call.
- **Contexts and timeouts.** Reads and writes take a context. One that is
  cancelled or times out during I/O closes the connection, since the frame
  stream cannot be resumed. `ReadTimeout` and `WriteTimeout` in the options
  bound each read and frame write without a timeout context for each call.
- **Closing.** `Close` performs the closing handshake; `CloseNow` closes at
  once. A closed connection returns the error that closed it:
  `CloseStatus(err)` gives the status code of a closing handshake or protocol
  violation.
- **Concurrency.** `Read`, `Reader` and `CloseRead` must not be called
  concurrently with each other, and a reader from `Reader` or a writer from
  `Writer` is for one goroutine at a time. All other `Conn` methods may be
  called concurrently.
- **Write-only connections.** `CloseRead` reads in the background, so that
  pings are answered and the peer's Close frame is seen. `Ping` checks that
  the peer still answers.
- **Origin.** A request with an `Origin` header is accepted only when its host
  and port equal the request's `Host`, unless `AcceptOptions.CheckOrigin`
  decides.
- **Compression.** Off by default; `CompressionMode` in the options enables
  it, with or without context takeover.
- **Limits.** `MaxMessageSize` bounds a received message, 4 MiB by default.
- **Server settings.** The timeouts and `Shutdown` of `fasthttp.Server` do not
  apply to an upgraded connection.

See the [package documentation](https://pkg.go.dev/github.com/valyala/fasthttp/websocket)
for the full API.
