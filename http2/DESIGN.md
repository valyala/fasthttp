# Native HTTP/2 design

The `http2` package is an opt-in protocol implementation over fasthttp's
request, response, and `RequestCtx` types. It does not convert through
`net/http`. Its exported API is experimental for at least two fasthttp minor
releases.

## Connection ownership

Each server connection has four execution roles:

1. A frame reader blocks in `ReadFrame` and sends bounded, immutable events.
2. The connection owner is the only goroutine that changes stream state,
   SETTINGS, HPACK state, flow-control windows, GOAWAY state, or the response
   schedule.
3. Each accepted stream runs the fasthttp handler independently, so a slow
   handler does not stop frame processing for other streams.
4. Control frames, header blocks, and flow-controlled DATA are serialized by
   one bounded writer path. RFC 9218 urgency and incremental scheduling are
   applied there; the obsolete RFC 7540 dependency tree is not maintained.

The client uses the same ownership rule for reads and HPACK state. Request
goroutines reserve stream IDs and slots, then use a serialized writer. A
request timeout resets only its stream and never changes the physical TCP
deadline shared by other streams.

## Codec boundary and provenance

The implementation uses the MIT-compatible `golang.org/x/net/http2.Framer`
and `hpack` packages for wire framing and compression primitives. A private
codec decodes HEADERS and CONTINUATION blocks directly into bounded, pooled
event storage. This avoids `net/http.Request`, header maps, and x/net's
allocation-heavy `MetaHeadersFrame` while retaining its frame validation.

The connection state machines, fasthttp mappings, flow control, scheduling,
push, and stream APIs are original code in this repository. No code was copied
from `dgrr/http2` or other third-party fasthttp HTTP/2 implementations.

## Negotiation

- TLS uses ALPN `h2`. `PreferHTTP2` advertises `h2` and `http/1.1`; when the
  peer selects HTTP/1.1, that same negotiated connection is handed to the
  built-in fasthttp HTTP/1 transport.
- `RequireHTTP2` returns `ErrHTTP2Required` when ALPN does not select `h2`.
- Cleartext HTTP/2 requires explicit `PriorKnowledge`. A configured server can
  distinguish the 24-byte HTTP/2 preface from HTTP/1 on the same listener and
  replays the first mismatching byte to the HTTP/1 parser.
- The obsolete HTTP/1 `Upgrade: h2c` mechanism is intentionally unsupported.

## HTTP semantics and extensions

Pseudo-headers are validated and mapped directly to fasthttp headers. The
implementation rejects connection-specific fields, invalid `TE`, malformed
content lengths, invalid trailer fields, invalid CONNECT combinations, and
body lengths that disagree with their headers. Header names on the wire are
lowercase. Sensitive request and response fields are encoded as never-indexed.

Server push is implemented but disabled by default. Push is same-origin,
limited to GET and HEAD, bounded by depth and concurrent promise limits, and
requires the client to install a `PushHandler`.

Extended CONNECT is also disabled by default. The client waits for the peer's
`SETTINGS_ENABLE_CONNECT_PROTOCOL=1` before sending `:protocol`.
`RequestCtx.AcceptStream` and `HostClient.OpenStream` expose DATA as a virtual
`fasthttp.StreamConn`; close, half-close, deadlines, and cancellation are
stream-scoped.

## Resource and shutdown boundaries

Concurrent streams, handler admission, event and command queues, frame size,
HPACK tables, decompressed header lists, cached header strings, push depth,
pending priority updates, closed-stream tombstones, and connection/stream
receive windows all have explicit bounds. A rapid-reset rate limit terminates
abusive connections with `ENHANCE_YOUR_CALM`.

Server shutdown sends an initial GOAWAY, uses a PING barrier to identify the
last accepted stream, then sends the final GOAWAY and drains accepted work. If
`ShutdownWithContext` expires, registered protocol connections are closed and
remaining streams are canceled.

## HTTP/3 boundary

The root package abstracts only request lifecycle, cancellation, push,
informational responses, and bidirectional stream semantics. TCP, HTTP/2
frames, HPACK, and HTTP/2 flow control remain private to this package.

A future HTTP/3 package can therefore use QUIC connections, control streams,
QPACK, and QUIC flow control directly under RFC 9114 and RFC 9204. It will not
need to emulate an HTTP/2 connection or add a closed HTTP-version enum to the
root package.

## Verification

The test suite includes native/native and Go x/net interoperability, TLS ALPN,
HTTP/1 fallback without a second dial, prior knowledge, same-port dispatch,
streaming bodies, trailers, 103 responses, push, extended CONNECT, half-close,
GOAWAY, flow control, rapid reset, and deterministic multiplexing tests.

CI runs all applicable h2spec 2.6.0 cases in strict mode in addition to Go
unit, race, shuffle, and fuzz targets. h2spec primarily tests RFC 7540/7541, so
RFC 9113, RFC 9218, rapid-reset, and RFC 8441 behavior is covered by native
tests rather than inferred from the h2spec score.
