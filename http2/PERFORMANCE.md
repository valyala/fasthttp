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
  -benchtime=500ms -benchmem -count=10 ./http2

go test -run '^$' -bench '^BenchmarkBodies$' \
  -benchtime=500ms -benchmem -count=10 ./http2
```

## Small GET throughput

Median nanoseconds per operation are shown below. “Go” means
`golang.org/x/net/http2`/`net/http`.

| Scenario | Streams | fasthttp | Go | Throughput advantage |
| --- | ---: | ---: | ---: | ---: |
| Native client + native server, cleartext | 1 | 24,500 | 27,830 | 1.14x |
| Native client + native server, cleartext | 10 | 6,244 | 7,815 | 1.25x |
| Native client + native server, cleartext | 100 | 3,969 | 6,596 | 1.66x |
| Native client + native server, cleartext | 1,000 | 4,284 | 6,929 | 1.62x |
| Clients against the same Go server | 100 | 7,294 | 9,887 | 1.36x |
| Servers behind the same fasthttp client | 100 | 3,988 | 7,460 | 1.87x |
| Servers behind the same fasthttp client | 1,000 | 4,271 | 7,496 | 1.76x |
| TLS client + native TLS server | 100 | 3,064 | 6,695 | 2.19x |
| HEADERS/HPACK codec | 1 block | 168.2 | 279.3 | 1.66x |

The 100- and 1,000-stream end-to-end samples have more scheduler variance
than the isolated server samples. The individual raw benchmark output should
be retained in the Draft PR when performance decisions are made.

## Small GET allocation profile

| Scenario | fasthttp | Go | Reduction |
| --- | ---: | ---: | ---: |
| Cleartext, 100 streams | 4 allocs / 164 B | 32 allocs / 3.52 KiB | 8x allocations, about 22x bytes |
| Server comparison, 100 streams | 4 allocs / 165 B | 28 allocs / 2.53 KiB | 7x allocations, about 16x bytes |
| TLS, 100 streams | 4 allocs / 168 B | 33 allocs / 3.53 KiB | about 8x allocations, about 22x bytes |
| HEADERS/HPACK codec | 2 allocs / 64 B | 10 allocs / 480 B | 5x allocations, 7.5x bytes |

The private header codec, stream/context pooling, bounded string interning, and
targeted writer wakeups account for most of this reduction. The two remaining
codec allocations are `HeadersFrame` objects created inside x/net's low-level
Framer; the implementation deliberately keeps that dependency boundary.

## Body throughput

| Scenario | fasthttp | Go | Throughput advantage |
| --- | ---: | ---: | ---: |
| 4 KiB GET | 4.71 µs | 9.84 µs | 2.09x |
| 1 MiB GET, fasthttp buffered | 204 µs | 567 µs | 2.79x |
| 4 KiB POST | 6.82 µs | 10.32 µs | 1.51x |
| 1 MiB POST | 179 µs | 329 µs | 1.84x |
| 1 MiB streaming request | 316 µs | 378 µs | 1.20x |
| 1 MiB streaming response | 219 µs | 581 µs | 2.65x |

The buffered 1 MiB fasthttp GET intentionally stores the entire response and
therefore reports about 1 MiB allocated per operation, while
`net/http.Response.Body` is inherently streaming in this harness; those byte
counts are not comparable. The native buffered path takes 12 allocations
versus 341 for Go in this scenario. The streaming request and response paths
use bounded, reusable frame segments and take 112 and 149 allocations versus
178 and 484. Streaming memory is bounded by the configured connection and
stream receive windows and can be reduced by lowering
`MaxResponseBufferPerConnection` and `MaxResponseBufferPerStream`.

## Profile-guided limits

After allocation and batching work, a 100-stream small-GET CPU profile spent
47% of samples in raw syscalls; most of the remainder was in kqueue and Go
scheduler sleep/wakeup paths. Handler startup was not a significant sampled
hotspot. A fixed handler worker pool was therefore not added: it would impose
head-of-line blocking on slow handlers without evidence of improving this
profile. The remaining high-value target is reducing flow-control wakeups on
bidirectional streaming workloads while retaining per-stream cancellation and
fairness.

## HTTP/1 regression guard

The existing `BenchmarkClientGetEndToEnd100Inmemory` and
`BenchmarkServerGet10KReqPerConn` benchmarks remained at zero allocations per
operation after the protocol bridge and HTTP/2 implementation. Ten-sample
`benchstat` output showed no throughput regression on this machine.

## Current conclusion

The current implementation exceeds Go HTTP/2 throughput in the measured
primary scenarios while using substantially fewer allocations. It does
**not** demonstrate a 10x end-to-end throughput advantage: the measured range
is about 1.14x to 2.79x. HTTP/1's historical order-of-magnitude comparisons
do not transfer directly to a multiplexed protocol whose physical connection,
HPACK state, writer ordering, and flow-control feedback are serialized by the
wire protocol. Network syscalls, TLS, scheduling, copying, and flow-control
now dominate wall time. Future optimization must be justified by CPU, heap,
mutex, and block profiles rather than by weakening protocol validation,
fairness, or resource limits.
