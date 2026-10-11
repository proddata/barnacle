# Barnacle load measurements

These measurements provide a baseline for reproducing tests and comparing deployment hosts. They are not capacity ratings. The runs used PostgreSQL 17, the published Neon serverless driver, and Docker Desktop on macOS with Barnacle limited to 256 MiB. Database backend memory and CPU are outside the Barnacle memory figures. Run the same profiles with representative SQL and concurrency on the target host before setting service limits.

## Query throughput and memory

The eight query profiles below ran for ten seconds each on 2026-10-06. Barnacle was restarted between profiles. HTTP connections represent requests in flight; WebSocket connections stayed open. Every scheduled request completed without an error or skip. The memory column shows sampled usage and the cgroup's recorded peak.

| Transport | Open WS / HTTP in-flight cap | Target → achieved QPS | Query | p95 latency | Sampled / cgroup peak MiB |
| --- | ---: | ---: | --- | ---: | ---: |
| HTTP | 1 | 10 → 10.0 | `select 1` | 19.3 ms | 12.4 / 17.6 |
| HTTP | 8 | 50 → 49.9 | `select 1` | 8.4 ms | 13.2 / 20.2 |
| HTTP | 8 | 100 → 99.9 | `select 1` | 6.4 ms | 13.6 / 20.6 |
| HTTP | 8 | 30 → 29.4 | `pg_sleep(0.2)` | 214.3 ms | 12.8 / 17.9 |
| HTTP | 8 | 50 → 49.9 | 8 KiB text result | 9.4 ms | 15.1 / 23.6 |
| WebSocket | 1 | 10 → 10.0 | `select 1` | 5.5 ms | 9.2 / 16.8 |
| WebSocket | 8 | 50 → 50.0 | `select 1` | 4.3 ms | 10.0 / 19.2 |
| WebSocket | 32 | 100 → 99.9 | `select 1` | 3.6 ms | 12.3 / 19.1 |

At an offered 500 QPS, eight HTTP slots completed 4,977 requests in ten seconds, skipped 23 when all slots were busy, and had 7.2 ms p95 latency. Eight persistent WebSocket sessions completed all 5,000 at 3.2 ms p95. These local results do not include an ingress or a separate PostgreSQL host.

## 100 MiB result comparison

For 100 rows of 1 MiB each, streaming HTTP completed with a 20.0 MiB Barnacle cgroup peak under the 256 MiB container limit. WebSocket completed the same shape near 16–17 MiB. A single 100 MiB HTTP row exceeded the default 8 MiB row limit and returned 413 before pgx allocated the full field; Barnacle peaked at 13.4 MiB. A 512 MiB streaming HTTP result also completed under the 256 MiB container limit, with a 19.4 MiB peak. These tests do not establish that every query shape or the full 2 GiB response limit fits within 256 MiB.

The Neon driver's `neon()` API reads and parses the complete JSON response on the client. Streaming reduces Barnacle's result buffering, but callers still need enough memory for their result. A stream interrupted after HTTP headers are sent can end with incomplete JSON. See the [HTTP limits](docs/decisions/sql-http-limits.md) for current defaults.

Reproduce a large-result profile with the Compose stack running:

```sh
docker compose restart barnacle
node integration/large-result.mjs --transport=http --rows=100 --row-mib=1
```

Use `--transport=ws` for the WebSocket comparison. The HTTP script opts into `Barnacle-Response-Mode: streaming`.

## HTTP connection reuse decision

Barnacle opens one upstream connection per HTTP request and keeps one per WebSocket session. In a separate ten-second local profile, eight HTTP slots completed 4,981 small queries with 6.7 ms p95 latency while eight WebSocket sessions completed 5,000 with 3.5 ms p95. A direct pgx probe measured fresh PostgreSQL connections at 4.79 ms p95 with one worker, compared with 0.25 ms p95 on a reused connection. The direct probe isolates connection setup and does not measure an HTTP pooling implementation.

The current design keeps one upstream connection per HTTP request. PgBouncer can pool PostgreSQL backend connections for stateless HTTP traffic, while WebSocket already provides persistent sessions. A pooler still leaves Barnacle-to-pooler connection setup in the request path. Measure that cost in the intended topology before changing the connection strategy. See [deployment](docs/deployment.md#postgresql-and-its-pooler) for pool-mode guidance.

Run the presets with `BENCH_SECONDS=10 ./integration/bench.sh` after starting Compose and installing the integration dependencies. For a single query scenario, use `node integration/bench.mjs --transport=http --connections=8 --qps=500 --seconds=10`. The presets also include WebSocket connection churn: 20 new authenticated sessions per second, at most 8 simultaneous handshakes and 24 open or connecting sessions, each held for 500 ms. Run it alone with:

```sh
node integration/bench.mjs --workload=churn --transport=ws --connections=24 --connect-rate=20 --connecting=8 --hold-ms=500 --seconds=10
```

The churn report includes successful connections per second, PostgreSQL startup latency, the maximum number of handshakes and open sessions, failed and rejected connections, locally skipped attempts, and Barnacle memory. Raising `--connections` above `BARNACLE_MAX_CONNECTIONS` can exercise its 503 limit. A skipped attempt was stopped by the benchmark's own caps and never reached Barnacle. For an installed systemd service, use `--memory-source=systemd`. The [development guide](docs/development.md#measure-memory-under-load) has the complete commands.

## HTTP request validation cost

On 2026-10-11, the request hardening was compared with a snapshot of the working tree taken immediately before that change. Both binaries used Go 1.26.9 and pgx 5.11.0 on an Apple M3 Pro. The end-to-end test used an isolated local PostgreSQL 14 cluster with SCRAM, HTTP keep-alive, and one fresh upstream connection per request. Four rounds alternated binary order, with 15 seconds between samples to avoid exhausting local ephemeral ports. Each workload warmed up with 16 requests, then measured 120 requests at concurrency 1 and 320 at concurrency 8.

| Workload | Before → after requests/s, concurrency 1 | Before → after requests/s, concurrency 8 | Before → after p95 ms, concurrency 8 |
| --- | ---: | ---: | ---: |
| Simple query | 367 → 446 | 1,579 → 1,922 | 8.63 → 5.39 |
| Scalar parameters | 438 → 490 | 1,288 → 1,932 | 12.56 → 5.33 |
| JSON object and nested array parameters | 375 → 500 | 1,833 → 1,959 | 6.04 → 5.31 |
| 64 KiB text parameter | 380 → 377 | 1,423 → 1,480 | 7.54 → 6.70 |
| Ten-query batch | 395 → 436 | 1,593 → 1,699 | 6.36 → 5.97 |

Values are medians of the four samples. The largest median throughput decrease was 1.0%. Host scheduling varied considerably, so the apparent throughput gains should not be attributed to the change. These measurements found no large end-to-end regression; they do not establish production capacity or cover TLS, OAuth validators, or PgBouncer.

Separate Go benchmarks measured upload buffering, decoding, and PostgreSQL parameter conversion without connection or query time. Medians of five 300 ms samples:

| Input | Before → after time | Before → after allocated bytes |
| --- | ---: | ---: |
| Simple query | 0.755 → 0.937 µs | 1,216 → 1,824 |
| Five scalar parameters | 1.284 → 1.524 µs | 1,600 → 2,208 |
| Object and nested array parameters | 2.315 → 2.600 µs | 2,537 → 3,145 |
| 100-query batch | 49.377 → 49.034 µs | 37,200 → 45,568 |
| 64 KiB string parameter | 260.229 → 295.975 µs | 326,369 → 472,769 |
| Array of 2,000 small objects | 2,692.797 → 917.347 µs | 14,626,070 → 1,245,645 |

Raw upload buffering and the complexity scan add measurable parsing cost, around 0.2 µs for a simple query and 36 µs for the 64 KiB parameter. Reusing the PostgreSQL array string escaper removes repeated large allocations for array elements. The normal request path still performs a single PostgreSQL authentication exchange and preserves numeric precision.

A 2.29 MiB body containing 300,000 small object parameters previously expanded before checking credentials. The isolated upload/decode benchmark allocated 135.8 MiB and took 64.4 ms; the new upload and complexity rejection allocated 4.8 MiB and took 0.31 ms (medians of three samples). These are cumulative allocations, not process RSS. Missing or oversized credentials now reject even before upload buffering; failed PostgreSQL authentication skips JSON decoding. Tests also verify the five-second authentication deadline, slot release, and that slow uploads open no upstream connection.

Run the current parsing benchmarks with:

```sh
go test ./internal/sqlhttp -run '^$' -bench 'Benchmark(RequestDecode|RejectComplexRequest)' -benchmem -count=5
```

With a test gateway and PostgreSQL running, set `BARNACLE_BASE_URL` and `TEST_DATABASE_URL`, then run `node integration/request-benchmark.mjs` for the HTTP workloads. It prints requests/s and p50/p95 milliseconds as JSON. The benchmark creates thousands of PostgreSQL connections; space repeated runs apart when comparing binaries on the same host.
