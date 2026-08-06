# Native HTTP/2 design

This package speaks HTTP/2 directly to fasthttp's `Request`, `Response`, and
`RequestCtx`. There is no `net/http` conversion layer. The exported API is
experimental and may change for two more minor releases.

## One owner per connection

Everything else follows from this rule: exactly one goroutine mutates a
connection's state. Stream table, SETTINGS, HPACK tables, flow-control
windows, GOAWAY state, response schedule — all of it belongs to that goroutine
alone, so none of it needs a lock.

Three other roles exist around it:

- A reader parked in `ReadFrame`, which turns frames into bounded events.
- A writer that owns `net.Conn.Write` and nothing else. It receives byte
  batches that are already ordered and already accounted for; it knows nothing
  about streams, HPACK, or credit. A batch it accepts is in flight — credit is
  never re-reserved or refunded behind its back.
- One goroutine per stream running the handler, so a slow handler stalls its
  own stream and no others.

Response scheduling uses RFC 9218 urgency. The RFC 7540 dependency tree is not
maintained — RFC 9113 deprecated it. The `incremental` parameter is parsed and
validated but the scheduler ignores it today.

### The client's stream-ID rule

A client stream gets its ID inside the connection write slot, immediately
before its HEADERS reach the writer — not when the request is admitted. That
one placement buys three properties for free:

- Wire order of request HEADERS equals ID order, by construction.
- A stream cancelled while waiting for the slot never consumes an ID and never
  puts a byte on the wire.
- RST_STREAM queues behind the same slot, so it cannot overtake the HEADERS it
  is cancelling.

An earlier version assigned IDs at admission and needed roughly a hundred
lines of claim, skip, and deferred-reset machinery to get the same ordering
back.

A request timeout resets its own stream and never touches the shared TCP
deadline. There is one deliberate exception: if a deadline expires after part
of a frame has been copied into a batch, that batch cannot be handed to
another writer, so the connection dies with it. Requests without a deadline
hit the same rule via `WriteByteTimeout`, because a producer parked on a full
queue cannot notice its own stream being cancelled.

## What we borrowed

Wire framing and HPACK primitives come from `golang.org/x/net/http2`'s
`Framer` and `hpack` (MIT, already a fasthttp dependency). We keep x/net's
frame validation and skip its `MetaHeadersFrame`: a private codec decodes
HEADERS and CONTINUATION straight into pooled event storage, which avoids the
header maps and per-message allocations that path costs.

The state machines, fasthttp mapping, flow control, scheduling, push, and
stream APIs are written for this repository. Nothing was copied from
`dgrr/http2`.

## Negotiation

TLS uses ALPN. `PreferHTTP2` offers `h2` and `http/1.1`; if the peer picks
HTTP/1.1, that same connection is handed to fasthttp's HTTP/1 transport rather
than dialed again. `RequireHTTP2` fails with `ErrHTTP2Required` instead.

Cleartext requires explicit `PriorKnowledge`. A server can tell the 24-byte
preface from an HTTP/1 request line on one listener; a non-matching prefix is
pushed back into the HTTP/1 reader without swapping the `net.Conn`.

`Upgrade: h2c` is not supported and will not be. RFC 9113 dropped it.

## Semantics

Pseudo-headers map straight onto fasthttp headers. Connection-specific fields,
bad `TE`, malformed content lengths, invalid trailers, malformed CONNECT, and
bodies that disagree with their declared length are all rejected. Wire header
names are lowercase; sensitive fields encode as never-indexed.

Push and extended CONNECT are both off by default. Push is same-origin, GET
and HEAD only, and bounded by depth and promise count. Extended CONNECT waits
for the peer's `SETTINGS_ENABLE_CONNECT_PROTOCOL=1` before sending
`:protocol`; `RequestCtx.AcceptStream` and `HostClient.OpenStream` then expose
DATA as a `fasthttp.StreamConn` whose close, half-close, and deadlines are
scoped to that stream.

## Limits, and the two we are least sure about

Concurrency, queue depths, frame size, HPACK tables, header list size, cached
strings, push depth, priority updates, and closed-stream tombstones are all
bounded. Rapid resets past 1000/second kill the connection with
`ENHANCE_YOUR_CALM`, and a CONTINUATION run past 64 frames does the same.

Two defaults deserve a second opinion:

**A 16 MiB connection receive window** (1 MiB per stream). x/net's server uses
1 MiB; fasthttp's HTTP/1 bound is `MaxRequestBodySize`, 4 MiB by default. So
this is 4x what an HTTP/1 connection can buffer and 16x what x/net allows,
traded for throughput on a single fast stream. On the client side the same
16 MiB is conservative — x/net's transport uses 1 GiB.

**A 128 MiB per-connection `RequestCtx` cache** (256 entries max). Released
contexts are reset and kept on the connection before falling back to the
Server pool, which keeps large body buffers attached to a busy multiplexed
connection instead of losing them to the next GC. It measurably helps: 1 MiB
uploads went from ~1.66 MiB/op to ~1.74 KiB/op. The cost is that unlike
`Server.ctxPool`, this memory is not GC-reclaimable — it is freed when the
connection closes, and nowhere else. The ceiling is not configurable today.

## Stream and connection teardown

Every stream ends at one finalizer owned by the connection: normal EOF, reset,
response-pump completion, extended CONNECT completion, and forced shutdown all
land there. It pools a stream only once the handler and the response pump are
done and the wire state is terminal. User-visible body objects keep stable
stream IDs rather than becoming a second release path.

Shutdown sends a GOAWAY, uses a PING barrier to pin down the last stream it
actually accepted, sends the final GOAWAY, then drains. If
`ShutdownWithContext` runs out of time, connections are closed and the
remaining streams cancelled.

## Room for HTTP/3

The root package's protocol hooks cover request lifecycle, cancellation, push,
informational responses, and bidirectional streams. `ProtocolRegistration` is
deliberately a connection-oriented bridge for TCP protocols — it is not a
QUIC abstraction, and no HTTP/2 frame, HPACK table, or HTTP/2 window escapes
this package.

An HTTP/3 package can therefore use QUIC streams, QPACK, and QUIC flow control
as RFC 9114 and RFC 9204 describe, without emulating an HTTP/2 connection or
teaching the root package a closed set of HTTP versions.

## Testing

Interoperability runs both ways against `golang.org/x/net/http2`, plus TLS
ALPN, HTTP/1 fallback without a second dial, prior knowledge, same-port
dispatch, streaming bodies, trailers, 103, push, extended CONNECT, half-close,
GOAWAY, flow control, and rapid reset.

h2spec 2.6.0 in strict mode passes, but it mostly covers RFC 7540/7541 — RFC
9113 specifics, RFC 9218, RFC 8441, and rapid-reset handling are covered by
tests here rather than inferred from the h2spec score. One h2spec case
(`http2/6.9.2/2`) is skipped because it needs the server to still be sending
when a shrunken window lands; `TestSettingsDecreaseDrivesSendWindowNegative`
pins that behavior deterministically instead.

Fuzz targets are run during release validation. The repository's CIFuzz
workflow does not build them, so it is not coverage for this package.
