# HTTP/2 design notes

The `http2` package implements HTTP/2 directly on fasthttp's `Request`,
`Response` and `RequestCtx`. It does not go through `net/http`.

## Concurrency model

One goroutine (the "owner") holds all connection state: stream table,
settings, HPACK tables, flow control windows, GOAWAY state and the response
schedule. There are no locks around this state.

The other goroutines per connection are:

- a reader, blocked in `ReadFrame`, which sends frames to the owner over a
  bounded channel
- a writer, which only calls `net.Conn.Write` on byte batches prepared by the
  owner
- one worker goroutine per active stream to run the handler; workers are
  reused between streams

Response scheduling uses RFC 9218 urgency. Within the same urgency,
non-incremental responses are sent one at a time in stream ID order and
incremental responses are interleaved. The RFC 7540 priority tree is not
implemented (deprecated by RFC 9113).

### Write batching

Design goal: one read syscall per incoming burst, one write syscall per
outgoing burst.

- The reader uses a buffered reader and marks each event if the next frame is
  already in the buffer, so the owner knows more input is coming.
- The owner counts started handlers and delays the flush while their
  completions keep arriving. The wait is bounded by 1ms of silence. Streams
  written off by the timeout are tagged with a generation counter so their
  late completions don't extend the next batch's wait.
- A batch is capped at 64 events. Larger batches benchmarked slower.
- The write loop collects all batches queued during the previous syscall and
  writes them with a single syscall.
- The flusher iterates a queue of streams with pending output, not the whole
  stream table.

### Client stream IDs

The client assigns a stream ID right before writing HEADERS, while holding
the connection write slot. Results:

- HEADERS wire order always matches stream ID order.
- A request cancelled before reaching the write slot never consumes an ID.
- RST_STREAM goes through the same slot, so it cannot arrive before its own
  HEADERS.

Request timeouts reset only their own stream. Exception: if a deadline
expires after part of a frame was already copied into a batch, the
connection is closed, because the remaining bytes can't be written by anyone
else. `WriteByteTimeout` covers the same case for requests without a
deadline.

## Dependencies

Frame encoding/decoding and HPACK come from `golang.org/x/net/http2`
(already a fasthttp dependency). `MetaHeadersFrame` is not used: HEADERS and
CONTINUATION payloads are decoded into pooled buffers by this package,
because the x/net path allocates per message and its frame cache only covers
DATA frames.

## Negotiation

- TLS: ALPN. `PreferHTTP2` offers `h2` and `http/1.1`; if the peer picks
  HTTP/1.1 the connection is handed to the HTTP/1 client without redialing.
  `RequireHTTP2` returns `ErrHTTP2Required` instead.
- Cleartext server: prior knowledge (preface detection on the same listener
  as HTTP/1) and the `Upgrade: h2c` handshake. The upgraded request becomes
  stream 1. The upgrade is declined for TLS connections, streamed request
  bodies and pipelined requests; those requests are served as HTTP/1.
- Cleartext client: `PriorKnowledge` only.

## Request/response semantics

Pseudo-headers map to fasthttp header fields. Rejected: connection-specific
headers, `TE` other than `trailers`, malformed content lengths, invalid
trailers, malformed CONNECT, bodies that don't match their content length.
Header names go on the wire lowercase. Sensitive fields are never-indexed.
Fields declared as trailers are excluded from the initial header block.

Push and extended CONNECT exist but are off by default. Push is same-origin
GET/HEAD, limited by depth and promise count. Extended CONNECT requires the
peer's `SETTINGS_ENABLE_CONNECT_PROTOCOL=1`. `RequestCtx.AcceptStream` and
`HostClient.OpenStream` expose the stream as a `fasthttp.StreamConn`.

gRPC works without dedicated API:

- unary: normal handler + `AddTrailer("Grpc-Status")`
- streaming: `Server.StreamRequestBody` + `ctx.RequestBodyStream()` +
  `SetBodyStreamWriter`. Every `Flush` produces a DATA frame boundary.
  Trailers are encoded after the stream writer returns, so the writer may
  still set trailer values. This ordering is guaranteed.
- errors without a body: set `grpc-status` as a normal header and don't
  declare trailers (gRPC "Trailers-Only"). nginx drops trailers on bodyless
  responses otherwise.

See `examples/grpcserver`.

## Limits

Bounded: concurrent streams, event/command queues, frame size, HPACK tables,
header list size, cached header strings, push depth, pending priority
updates, closed-stream tombstones. More than 1000 RST_STREAM/s from a peer or
a CONTINUATION run longer than 64 frames closes the connection with
`ENHANCE_YOUR_CALM`.

Two defaults were chosen by measurement:

**Connection receive window: 4 MiB** (1 MiB per stream), same as HTTP/1's
default `MaxRequestBodySize`. Configurable via
`MaxUploadBufferPerConnection`.

| | 16 MiB | 4 MiB | 1 MiB (x/net default) |
| --- | ---: | ---: | ---: |
| streamed 1 MiB upload | 309 us | 327 us | 394 us |
| buffered 1 MiB POST | 191 us | 170 us | 205 us |
| small GET, 100 streams | 5.84 us | 5.28 us | 5.16 us |
| small GET, 1000 streams | 5.77 us | 5.49 us | 5.45 us |

16 MiB doesn't help anything. 1 MiB makes streamed uploads 27% slower. So
4 MiB.

**Per-connection RequestCtx cache: 128 MiB** (max 256 entries). Released
contexts are cached on the connection before falling back to the shared
server pool, so a busy connection keeps its body buffers. Measured with 1 MiB
POSTs: 86 B/op at 128 MiB, 368 KB/op at 32 MiB, 2.8 MB/op at 8 MiB. Downside:
this memory is only freed when the connection closes, not by GC.
`Server.MaxProtocolRequestCtxCacheBytes` changes the limit, negative disables
the cache. x/net buffers bodies in fixed 16 KiB pools instead, which is why
it allocates on every request.

## Teardown

All stream ends (EOF, reset, response pump done, extended CONNECT done,
forced shutdown) go through one finalizer owned by the connection. A stream
returns to the pool only after the handler and the response pump have
finished and the wire state is terminal.

Shutdown: send GOAWAY, PING barrier to fix the last accepted stream ID, final
GOAWAY, drain. If the shutdown context expires first, connections are closed
and remaining streams cancelled.

## HTTP/3

The protocol hooks in the root package (request lifecycle, cancellation,
push, informational responses, bidirectional streams) don't expose any
HTTP/2 types. An HTTP/3 package can implement them with QUIC streams and
QPACK directly.

## Testing

Interop tests run both directions against `golang.org/x/net/http2`, plus:
TLS ALPN, HTTP/1 fallback, prior knowledge, h2c upgrade, same-port dispatch,
streaming, trailers, 103, push, extended CONNECT, GOAWAY, flow control,
rapid reset. The grpc-go interop client passes directly and through nginx
`grpc_pass`.

h2spec 2.6.0 strict passes. h2spec mostly covers RFC 7540/7541; RFC 9113
details, RFC 9218, RFC 8441 and rapid reset are covered by tests in this
package. One h2spec case (`http2/6.9.2/2`) depends on catching the server
mid-send and is skipped; `TestSettingsDecreaseDrivesSendWindowNegative`
tests the same behavior deterministically.

Fuzz targets exist but the repo's CIFuzz workflow doesn't build them; they
are run manually.
