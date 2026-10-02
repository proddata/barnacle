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
