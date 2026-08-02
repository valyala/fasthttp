# HTTP/2 performance snapshot

This is a local development snapshot, not a universal performance claim. The
benchmarks run both endpoints in one process over loopback TCP, prewarm the
connections, use one physical connection, and report ten samples through
`benchstat`. TLS results use the same certificate and TLS implementation for
both clients.

Machine:

- Apple M4, 10 logical CPUs, 32 GiB RAM
- macOS 15.7.8 (24G824), arm64
- Go 1.26.5

Commands:

```sh
go test -run '^$' \
  -bench '^(BenchmarkEndToEnd|BenchmarkClientsAgainstGoServer|BenchmarkServersWithFasthttpClient|BenchmarkHeaderCodec|BenchmarkTLSGET)$' \
  -benchmem -count=10 ./http2

go test -run '^$' -bench '^BenchmarkBodies$' \
  -benchtime=500ms -benchmem -count=10 ./http2
```

## Small GET throughput

Median nanoseconds per operation are shown below. “Go” means
`golang.org/x/net/http2`/`net/http`.

| Scenario | Streams | fasthttp | Go | Throughput advantage |
| --- | ---: | ---: | ---: | ---: |
| Native client + native server, cleartext | 1 | 23,890 | 27,090 | 1.13x |
| Native client + native server, cleartext | 10 | 5,799 | 7,409 | 1.28x |
| Native client + native server, cleartext | 100 | 3,989 | 6,675 | 1.67x |
| Native client + native server, cleartext | 1,000 | 6,844 | 9,460 | 1.38x |
| Clients against the same Go server | 100 | 7,156 | 8,878 | 1.24x |
| Servers behind the same fasthttp client | 100 | 3,585 | 6,534 | 1.82x |
| Servers behind the same fasthttp client | 1,000 | 3,715 | 6,568 | 1.77x |
| TLS client + native TLS server | 100 | 2,746 | 5,798 | 2.11x |
| HEADERS/HPACK codec | 1 block | 169.3 | 315.9 | 1.87x |

The 100- and 1,000-stream end-to-end samples have more scheduler variance
than the isolated server samples. The individual raw benchmark output should
be retained in the Draft PR when performance decisions are made.

## Small GET allocation profile

| Scenario | fasthttp | Go | Reduction |
| --- | ---: | ---: | ---: |
| Cleartext, 100 streams | 4 allocs / 163 B | 32 allocs / 3.51 KiB | 8x allocations, about 22x bytes |
| Server comparison, 100 streams | 4 allocs / 160 B | 28 allocs / 2.48 KiB | 7x allocations, about 16x bytes |
| TLS, 100 streams | 4 allocs / 163 B | 32 allocs / 3.47 KiB | 8x allocations, about 22x bytes |
| HEADERS/HPACK codec | 2 allocs / 64 B | 10 allocs / 480 B | 5x allocations, 7.5x bytes |

The private header codec, stream/context pooling, bounded string interning, and
targeted writer wakeups account for most of this reduction. The two remaining
codec allocations are `HeadersFrame` objects created inside x/net's low-level
Framer; the implementation deliberately keeps that dependency boundary.

## Body throughput

| Scenario | fasthttp | Go | Throughput advantage |
| --- | ---: | ---: | ---: |
| 4 KiB GET | 4.66 µs | 9.88 µs | 2.12x |
| 1 MiB GET, fasthttp buffered | 357 µs | 497 µs | 1.39x |
| 4 KiB POST | 7.18 µs | 9.27 µs | 1.29x |
| 1 MiB POST | 325 µs | 367 µs | 1.13x |
| 1 MiB streaming request | 552 µs | 777 µs | 1.41x |
| 1 MiB streaming response | 366 µs | 776 µs | 2.12x |

The buffered 1 MiB fasthttp GET intentionally stores the entire response,
while `net/http.Response.Body` is inherently streaming in this harness; its
memory numbers are therefore not comparable. Streaming memory is bounded by
the configured connection and stream receive windows and can be reduced by
lowering `MaxResponseBufferPerConnection` and
`MaxResponseBufferPerStream`.

## HTTP/1 regression guard

The existing `BenchmarkClientGetEndToEnd100Inmemory` and
`BenchmarkServerGet10KReqPerConn` benchmarks remained at zero allocations per
operation after the protocol bridge and HTTP/2 implementation. Ten-sample
`benchstat` output showed no throughput regression on this machine.

## Current conclusion

The current implementation clears the original acceptance gate of exceeding
Go HTTP/2 throughput in the measured primary scenarios while allocating less.
It does **not** demonstrate a 10x end-to-end throughput advantage. The large
advantage is presently in allocation count and allocated bytes; network,
TLS, scheduling, copying, and flow control dominate wall time. The pull
request must remain Draft if a 10x throughput target is required, and future
optimization should be justified by CPU, heap, mutex, and block profiles
rather than by weakening protocol validation or resource limits.
