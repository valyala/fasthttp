# HTTP/2 performance validation

HTTP/2 is experimental. This document defines a reproducible benchmark method
and records one local validation snapshot. The snapshot is evidence for this
implementation, not a general speedup claim; it must be regenerated after
correctness, resource-safety, toolchain, or benchmark-harness changes.

## Comparison matrix

The benchmark suite separates three questions:

- `BenchmarkEndToEnd` compares the native fasthttp HTTP/2 client and server
  with the Go HTTP/2 client and server.
- `BenchmarkClientsAgainstGoServer` compares only clients against one Go
  server.
- `BenchmarkServersWithFasthttpClient` compares only servers behind
  identically configured native fasthttp clients.

`BenchmarkBodies`, `BenchmarkTLSGET`, and `BenchmarkHeaderCodec` cover body
sizes, streaming, TLS, and the private HPACK codec. Both sides are prewarmed.
Single-connection scenarios configure one physical connection; benchmark
changes must retain an explicit dial-count assertion before their results are
used for a performance claim.

Run at least ten one-second samples, serially, on an otherwise idle machine:

```sh
go test -run '^$' \
  -bench '^(BenchmarkEndToEnd|BenchmarkClientsAgainstGoServer|BenchmarkServersWithFasthttpClient|BenchmarkHeaderCodec|BenchmarkTLSGET)$' \
  -benchtime=1s -benchmem -count=10 ./http2 | tee /tmp/fasthttp-h2-small.txt

go test -run '^$' -bench '^BenchmarkBodies$' \
  -benchtime=1s -benchmem -count=10 ./http2 | tee /tmp/fasthttp-h2-bodies.txt

benchstat /tmp/fasthttp-h2-small.txt
benchstat /tmp/fasthttp-h2-bodies.txt
```

Record the CPU model, operating system, Go version, `GOMAXPROCS`, TLS mode,
payload, concurrency, and physical connection count beside every result.
Comparisons from different machines or harness revisions are not combined.

## Acceptance rules

- Enabling no protocol must not add allocations to the existing HTTP/1 hot
  path. The repository's zero-allocation tests are the first gate.
- A statistically significant HTTP/1 throughput regression above 2% requires
  investigation before publication.
- HTTP/2 throughput and allocation claims must come from symmetric arms. API
  construction cost and transport-only cost must be labeled separately.
- A result from one payload, concurrency level, or TLS mode is not generalized
  to all HTTP/2 workloads.
- Correctness checks, flow-control accounting, and resource limits are never
  disabled to improve a benchmark.

When a result misses the target, collect CPU, allocation, mutex, block, and
trace profiles. Optimize one measured hotspot at a time and rerun the same
samples. Stop when two consecutive changes improve less than 3%, or when at
least 70% of sampled time is outside code that can be safely optimized. Only
measured ratios may appear in release notes or a future pull request.

## Local validation snapshots

### Post-audit correctness build

The HEADERS sequencing, write-deadline, shutdown, and request-body ownership
repairs were measured again on 2026-08-04. The ten-sample core matrix still
showed fasthttp ahead of Go HTTP/2 in every paired scenario: end-to-end ratios
were 1.25x, 1.44x, 1.98x, and 1.82x at 1, 10, 100, and 1000 concurrent streams.
Allocations remained 6 per operation at one stream and 4 at higher concurrency,
versus 58 and 57 for the Go arm.

Those timing samples do not supersede the historical table below. During the
run, an unrelated browser renderer continuously occupied approximately one CPU
and the high-concurrency samples showed 15-28% variance. CPU profiling placed
more than 80% of samples in syscalls and runtime scheduling rather than the
HTTP/2 implementation. A quiet-host rerun is therefore required before these
numbers are used in release notes or a pull request.

The 2026-08-06 pre-submission hygiene pass (linter conformance, pointer
parameters on the frame-event path, pooled-buffer wrappers) was verified
allocation-neutral: the end-to-end GET arm still measures 6 allocations per
operation at one stream and 4 at higher concurrency, and the HTTP/1 server
paths still measure zero. A follow-up review pass pooled the streamed
request-body scratch buffer and the deadline timers on the client and writer
paths, taking the 1 MiB streaming request from 13 to 11 allocations per
operation. The quiet-host timing rerun above remains owed.

An allocation profile was still actionable: the streaming request path was
allocating once when its chunk queue discarded the front of a slice and once
when a byte slice was boxed into `sync.Pool`, for every DATA frame. Retaining
the queue backing array and pooling a typed buffer owner reduced the 1 MiB
streaming request from 139 to 13 allocations per operation. Its timing result
from the busy host is intentionally not recorded as a performance claim.

### Pre-audit optimization baseline

The following snapshot was collected on 2026-08-04 with Go 1.26.5 on macOS
arm64, Apple M4. The core small-GET callbacks assert exactly one physical
connection per arm; payload and TLS clients are configured for one connection.
Samples were run serially, ten times for one second each, after connection
warmup:

```sh
go test -run '^$' \
  -bench='^(BenchmarkEndToEnd|BenchmarkClientsAgainstGoServer|BenchmarkServersWithFasthttpClient)$' \
  -benchtime=1s -benchmem -count=10 ./http2

go test -run '^$' \
  -bench='^(BenchmarkBodies|BenchmarkTLSGET|BenchmarkHeaderCodec)$' \
  -benchtime=1s -benchmem -count=10 ./http2
```

Median small-GET transport results:

| Scope | Streams | fasthttp | Go HTTP/2 | Ratio |
| --- | ---: | ---: | ---: | ---: |
| End-to-end | 1 | 25.37 us | 33.12 us | 1.31x |
| End-to-end | 10 | 6.227 us | 8.921 us | 1.43x |
| End-to-end | 100 | 3.782 us | 7.827 us | 2.07x |
| End-to-end | 1000 | 3.855 us | 8.152 us | 2.11x |
| Client-only | 1 / 10 / 100 / 1000 | 30.17 / 8.478 / 6.589 / 6.592 us | 33.31 / 9.035 / 7.718 / 8.173 us | 1.07x-1.24x |
| Server-only | 1 / 10 / 100 / 1000 | 25.49 / 6.245 / 3.808 / 3.850 us | 30.17 / 8.455 / 6.540 / 6.544 us | 1.18x-1.72x |

End-to-end fasthttp used 6 allocations per operation at one stream and 4 at
10-1000 streams; the Go arm used 58 and 57 respectively. Selected median
payload and TLS results were:

`BenchmarkBodies` and `BenchmarkTLSGET` compare the fasthttp and Go clients
against the same fasthttp server, isolating the client/API path for those
scenarios. They are not presented as Go-client-plus-Go-server end-to-end
numbers.

| Scenario | fasthttp | Go HTTP/2 | Ratio |
| --- | ---: | ---: | ---: |
| Header decode | 165.3 ns | 270.5 ns | 1.64x |
| GET 4 KiB | 4.143 us | 8.239 us | 1.99x |
| GET 1 MiB | 154.1 us | 410.9 us | 2.67x |
| POST 4 KiB | 6.369 us | 8.228 us | 1.29x |
| POST 1 MiB | 146.7 us | 216.2 us | 1.47x |
| Streaming request 1 MiB | 182.2 us | 212.1 us | 1.16x |
| Streaming response 1 MiB | 168.3 us | 529.3 us | 3.14x |
| TLS GET, 1 stream | 25.50 us | 28.94 us | 1.13x |
| TLS GET, 100 streams | 2.968 us | 5.882 us | 1.98x |

An allocation profile initially found buffered 1 MiB uploads allocating in
`Request.AppendBody` because GC could clear the global RequestCtx pool. The
bounded connection-local protocol RequestCtx cache reduced that case from
approximately 1.66 MiB/op to 1.74 KiB/op. A final paired run after adding the
128 MiB cache-byte ceiling measured 146.7 us versus 216.2 us for the Go client.
No HTTP/1 hot-path allocation changed: the selected client and server
benchmarks remain at zero allocations per operation.

## HTTP/1.1 versus HTTP/2 on identical fasthttp handlers

`BenchmarkHTTP1VersusHTTP2` answers the adoption question for existing
fasthttp users directly: the same handler served over fasthttp's own
HTTP/1.1 (one pooled connection per in-flight request) and over native
HTTP/2 (every stream multiplexed on a single connection).

```
go test -run '^$' -bench 'BenchmarkHTTP1VersusHTTP2' -benchmem -benchtime 2s ./http2
```

Snapshot, 2026-08-06, Apple M4, Go 1.26.5, moderate background load
(paired arms measured in the same run, so ratios are meaningful even where
absolute numbers are not):

| Scenario | HTTP/1.1 | HTTP/2 | Physical connections (h1 / h2) |
| --- | ---: | ---: | ---: |
| GET, concurrency 1 | 14.12 us, 0 allocs | 28.75 us, 6 allocs | 1 / 1 |
| GET, concurrency 10 | 7.47 us, 0 allocs | 7.92 us, 4 allocs | 10 / 1 |
| GET, concurrency 100 | 5.22 us, 0 allocs | 4.67 us, 4 allocs | 100 / 1 |
| GET, concurrency 1000 | 5.18 us, 0 allocs | 5.83 us, 4 allocs | 1000 / 1 |
| POST 4 KiB, concurrency 100 | 6.84 us, 0 allocs | 7.51 us, 4 allocs | 100+ / 1 |

Reading: a single serialized request pays roughly 2x for HPACK, framing, and
the writer handoff — HTTP/2 cannot beat fasthttp's HTTP/1 parser on one
idle connection, and this document makes no such claim. From ten concurrent
requests upward the gap closes to within about 12% in either direction
(HTTP/2 was 11% faster at concurrency 100 in this snapshot), while HTTP/2
carries the entire load on one TCP connection instead of up to a thousand.
The HTTP/1 hot path itself is untouched: its arms still measure zero
allocations per operation on this branch.

The value proposition is therefore capability at near-parity cost —
multiplexing over one connection, ALPN-negotiated TLS, trailers, server
push, extended CONNECT — not a raw single-request speedup over HTTP/1.1.
