# Hermit load measurements

These are **observations**, not capacity guarantees. They were collected on 2026-10-01 with PostgreSQL 17 in Docker Compose, Hermit limited to 256 MiB, Go `GOMEMLIMIT=160MiB`, and the published `@neondatabase/serverless` 1.2.0 driver. The host was Docker Desktop on macOS. Each profile ran for eight seconds; the runner requested Docker cgroup samples every 500 ms, but the Docker CLI produced only five or six samples per profile. The table reports the largest sampled value, so a shorter peak could have been missed. The runner now also reads the cgroup's recorded peak. All requests in this run completed with no errors or skips.

| Transport | Connection cap / open WS | Target → achieved QPS | Query | Peak Hermit memory | p95 latency |
| --- | ---: | ---: | --- | ---: | ---: |
| HTTP | 1 | 10 → 10.0 | `select 1` | 9.6 MiB | 15.6 ms |
| HTTP | 8 | 50 → 49.9 | `select 1` | 12.5 MiB | 7.5 ms |
| HTTP | 32 | 100 → 99.8 | `select 1` | 12.7 MiB | 6.0 ms |
| HTTP | 32 | 100 → 97.5 | `pg_sleep(0.2)`; up to 25 queries in flight | 16.4 MiB | 208.4 ms |
| HTTP | 8 | 50 → 49.9 | 8 KiB text result, gzip | 13.8 MiB | 8.2 ms |
| WebSocket | 1 | 10 → 10.0 | `select 1` | 6.4 MiB | 5.4 ms |
| WebSocket | 8 | 50 → 49.9 | `select 1` | 8.7 MiB | 4.1 ms |
| WebSocket | 32 | 100 → 99.9 | `select 1` | 9.4 MiB | 3.5 ms |

The HTTP connection number is an **in-flight cap**. Fast HTTP queries typically use far fewer simultaneous PostgreSQL backends than that cap. The 200 ms case exercised overlap and observed 25 in flight. WebSocket clients remained connected throughout each run. Baseline Hermit memory after each restart was about 6 MiB, except for the 200 ms case, which followed another benchmark and began at 13.9 MiB. Since its peak was 16.4 MiB, rerun profiles on a quiet target host before choosing production limits.

These samples do not measure PostgreSQL backend memory, CPU saturation, maximum capacity, or short memory spikes between samples. PostgreSQL query shape, result size, authentication method, concurrent clients, HAProxy, and Fedora host memory will affect the numbers. For an installed Fedora service, `--memory-source=systemd` samples `MemoryCurrent` from `hermit.service`; see the [benchmark instructions](README.md#measure-memory-under-load).

## 100 MiB result comparison

These measurements were taken before the default HTTP size limits were added. The same query returned exactly 100 MiB of ASCII text in either 100 rows of 1 MiB or one row of 100 MiB. Each run followed a Hermit container restart. `memory.peak` from Hermit's cgroup records the peak even when a 200 ms sample misses it. HTTP used the published Neon driver and its normal gzip-capable `fetch()` path. The text repeats an MD5 digest, so it compresses strongly; these timings are not a comparison of uncompressed network throughput.

| Shape | Transport | Hermit limit | Result | Hermit cgroup peak | Elapsed |
| --- | --- | ---: | --- | ---: | ---: |
| 100 × 1 MiB | WebSocket | 256 MiB | Completed | 17.1 MiB | 0.48 s |
| 100 × 1 MiB | HTTP, streamed | 256 MiB | Completed | 20.5 MiB | 0.72 s |
| 1 × 100 MiB | WebSocket | 256 MiB | Completed | 15.8 MiB | 0.50 s |
| 1 × 100 MiB | HTTP, streamed | 256 MiB | Completed | 116.6 MiB | 0.75 s |

Before single-query streaming, the 100 × 1 MiB HTTP case OOM-killed Hermit at the default 256 MiB limit. With a temporary 768 MiB limit it peaked at **342.0 MiB**; one 100 MiB HTTP row peaked at **410.8 MiB**. Streaming removed the all-rows JSON buffer and brought both cases below the default limit. Before the wire-size cap, the 100 MiB single row needed about 117 MiB inside Hermit because pgx received the whole PostgreSQL field. WebSocket relays wire bytes in 32 KiB chunks.

The client still holds the complete result: in the single-row run, the Node process peaked near 516 MiB over HTTP and 434 MiB over WebSocket. Neon's `neon()` API calls `response.json()`, so HTTP transfer streaming does not make the driver expose rows incrementally. These are single-query measurements on Docker Desktop. Hermit now defaults to eight simultaneous HTTP queries, 8 MiB per row, 4 MiB of buffered result budget, and 128 MiB of response JSON. With pgx 5.11, its wire reader checked the PostgreSQL message size before allocating its body: a 100 MiB single-row test returned 413 with a **13.4 MiB** cgroup peak and no OOM. A 100 × 1 MiB streamed HTTP result completed at **20.0 MiB** peak.

Reproduce a profile with the Compose stack running:

```sh
docker compose restart hermit
node integration/large-result.mjs --transport=ws --rows=100 --row-mib=1
docker compose restart hermit
node integration/large-result.mjs --transport=http --rows=100 --row-mib=1
```

Both commands complete under the 256 MiB limit. To repeat the historical one-row HTTP measurement, run `HERMIT_HTTP_MAX_ROW_MIB=128 docker compose up -d --force-recreate hermit` before the test; the default returns 413. Recreate again without that variable to restore the default. A client or PostgreSQL error after HTTP headers have begun produces an interrupted JSON stream; the HTTP status cannot then be changed to a PostgreSQL error response.

## HTTP connection reuse decision

On 2026-10-03, a local Docker Desktop run used the Compose PostgreSQL 17 and Hermit services, TLS between Hermit and PostgreSQL, `select 1`, the published Neon driver, and a 256 MiB Hermit limit. Each endpoint profile lasted ten seconds. `connections` capped HTTP requests in flight or kept that many WebSocket sessions open. PostgreSQL's `sessions` counter rose by approximately one per completed HTTP request; the Compose health check and statistics observer also create sessions. Hermit was not restarted between these profiles, so the sampled memory peaks reflect a warmed process rather than isolated per-profile increments.

| Transport | Offered QPS | Completed / skipped | p50 / p95 / p99 | Hermit sampled peak |
| --- | ---: | ---: | ---: | ---: |
| HTTP, 1 in flight | 100 | 882 / 118 | 3.6 / 4.0 / 5.0 ms | 13.2 MiB |
| HTTP, 8 in flight | 200 | 1,999 / 1 | 4.3 / 5.1 / 5.9 ms | 16.3 MiB |
| WebSocket, 8 open | 200 | 2,000 / 0 | 2.0 / 3.2 / 3.3 ms | 15.9 MiB |
| HTTP, 8 in flight | 500 | 4,981 / 19 | 5.6 / 6.7 / 7.7 ms | 17.1 MiB |
| WebSocket, 8 open | 500 | 5,000 / 0 | 2.1 / 3.5 / 4.2 ms | 17.4 MiB |

The [direct pgx probe](integration/connection-cost.go) isolates PostgreSQL connection setup from HTTP and WebSocket processing. It used TLS from the macOS host to PostgreSQL's localhost port in the same Compose stack. With one worker and 500 queries, a fresh connection per query had p50/p95 4.45/4.79 ms and 221 QPS; one reused connection had 0.19/0.25 ms and 4,635 QPS. With eight workers and 1,000 queries, fresh connections had p50/p95 7.79/10.47 ms and 990 QPS; reused connections had 0.43/0.61 ms and 15,188 QPS. PostgreSQL recorded about 500 or 1,000 new sessions in the fresh cases and only one or eight in the reused cases. The direct probe is **not** an HTTP pooling implementation, so its QPS and latency should not be substituted for Hermit's HTTP results.

**Decision:** keep the current per-request HTTP connection behavior. It sustained roughly 500 small queries per second in this local setup with a 6.7 ms p95 and low Hermit memory. A nearby [PgBouncer](https://www.pgbouncer.org/usage) can pool PostgreSQL server connections even while Hermit opens one client connection for each HTTP request. This addresses backend session churn without putting credential and session-state management inside Hermit. It does not remove the Hermit-to-PgBouncer TCP/TLS/authentication cost, so the direct reuse probe still shows a possible latency benefit from a Hermit-side pool. Measure the intended PgBouncer and Fedora deployment before reconsidering one. The PostgreSQL OAuth/OAUTHBEARER path has not been tested through PgBouncer; [PgBouncer authenticates its clients itself](https://www.pgbouncer.org/config), so do not assume that path will pass through unchanged. WebSocket already offers persistent client sessions when the application needs them.

To repeat the direct probe, expose the disposable Compose PostgreSQL port on localhost with a temporary override, then set `TEST_DATABASE_URL` to that port with `sslmode=require` and run `go run integration/connection-cost.go -mode=fresh -workers=1 -queries=500` and the corresponding `-mode=reuse` command. The endpoint profiles use `node integration/bench.mjs --transport=http --connections=8 --qps=500 --seconds=10` or `--transport=ws`. These numbers are host-specific; Docker Desktop networking and TLS affect them.
