# SQL-over-HTTP limits

- **Status:** Proposed — production defaults have not been decided
- **Date:** 2026-10-03
- **Scope:** Hermit's `/sql` endpoint on a small Fedora host near PostgreSQL

## Question

Which request, result, time, and concurrency limits should ship for the intended workload? The current values are intended to protect a 256 MiB Hermit service, but they have not been sized on the target Fedora host. Keep the current defaults until a release profile is measured and approved.

## Reference point

The inspected Neon proxy source sets default SQL-over-HTTP request and response sizes to **10 MiB each**. Its SQL handler reads the request with the configured limit and checks buffered response JSON while adding rows. These are proxy configuration defaults, not a promise about every Neon deployment or a precise bound on a single row. See [Neon's proxy options](https://github.com/neondatabase/neon/blob/main/proxy/src/binary/proxy.rs) and [SQL-over-HTTP handler](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/sql_over_http.rs).

Hermit streams ordinary single-query JSON rows, but buffers batches and some custom-type results. That makes a larger total response possible with low proxy memory when rows are individually small. The Neon JavaScript driver still calls `response.json()` and retains the entire result on the client. Hermit's 256 MiB systemd `MemoryMax` is a separate final safeguard; an OOM kill drops active connections.

## Proposed choices

| Limit | Current Hermit value | Proposal and reason | Evidence needed before changing |
| --- | --- | --- | --- |
| HTTP request body | 1 MiB, fixed in code | **Keep provisionally; make configurable before committing to a production value.** It is below Neon's 10 MiB source default and may reject large parameter sets. A higher cap increases concurrent JSON decoding and parameter memory. | Largest real request bodies; 8 concurrent near-limit requests under the 256 MiB service limit; rejection behavior. |
| Queries per batch | 100, fixed in code | **Keep provisionally.** It bounds transaction time and per-batch bookkeeping, but it is an arbitrary compatibility limit. | Largest real transactions and their duration, memory, and rollback behavior. |
| Raw PostgreSQL data in one HTTP row | 8 MiB, `HERMIT_HTTP_MAX_ROW_MIB` | **Keep 8 MiB by default.** pgx must materialize a PostgreSQL row; the cap prevents one field from consuming most of the service's memory. Raise only for a demonstrated workload. | Peak cgroup memory for the required row size at concurrent load. |
| Buffered batch/custom-type result | 4 MiB, `HERMIT_HTTP_MAX_BUFFERED_MIB` | **Keep 4 MiB initially.** These paths retain results in memory. This may be the first practical limit for batches returning many rows. | Representative largest batch, including many small rows, at concurrent load. |
| Uncompressed response JSON | 128 MiB, `HERMIT_HTTP_MAX_RESPONSE_MIB` | **Keep provisionally for streamed single queries; reconsider the public contract.** It allows a 100 MiB many-row result at low proxy memory, but greatly exceeds Neon's 10 MiB source default and can consume substantial client memory. Once streaming has begun, crossing the cap interrupts JSON instead of returning a clean HTTP 413. | Client memory and latency at the largest allowed result; whether callers need a structured size error; expected result size distribution. |
| Concurrent HTTP queries | 8, `HERMIT_MAX_HTTP_QUERIES` | **Keep as the small-instance starting cap.** It protects CPU, upstream connections, and memory; bursts above it receive 503. | Target QPS and p95/p99 under slow queries and large results; 503 rate. |
| Total upstream connections | 32, `HERMIT_MAX_CONNECTIONS` | **Keep as the starting cap and size against PostgreSQL's connection budget.** HTTP and WebSocket sessions share it. PgBouncer may reduce PostgreSQL backend use, but Hermit still holds client connections. | Concurrent WebSockets, HTTP overlap, PostgreSQL or PgBouncer limits, and memory per connection. |
| HTTP connect and query deadline | 30 seconds, `HERMIT_QUERY_TIMEOUT` | **Keep initially.** Set a PostgreSQL `statement_timeout` for the deployment as well. Longer legitimate queries need an explicit workload decision. | Query duration distribution, cancellation behavior, and recovery from database stalls. |
| HTTP request read deadline | 15 seconds, `HERMIT_HTTP_READ_TIMEOUT` | **Keep initially.** It bounds clients that send bodies slowly. | Legitimate upload duration through the actual ingress and slow-client tests. |

The row, buffer, response, and concurrency values are configurable today. The request-body and batch-count limits require code changes to alter; changing their proposed defaults should include configuration support and integration tests.

## Evidence already collected

- Under a 256 MiB Hermit cgroup, a 100 MiB single row used about **116.6 MiB** before the wire-size cap. With the default 8 MiB row cap, it returned 413 at a **13.4 MiB** peak. A 100 × 1 MiB HTTP result completed at about **20 MiB** peak. See [large-result measurements](../../benchmarks.md#100-mib-result-comparison).
- A local small-query test completed about **500 HTTP QPS** with eight in-flight slots, **6.7 ms p95**, and about **17 MiB** sampled Hermit peak. This is a Docker Desktop result, not a target-host capacity guarantee. See [connection-cost measurements](../../benchmarks.md#http-connection-reuse-decision).
- Existing integration tests check HTTP 413 behavior for oversized rows and buffered results. [Current settings](../../README.md#deploy-near-postgresql) and [implementation](../../internal/sqlhttp/sql.go) define the enforced limits.

## Decision gate

Before release, run the representative workload on the intended Fedora host with its actual ingress, PostgreSQL or PgBouncer path, TLS, and auth mode. Include near-limit request bodies, large single rows, many-row responses, batches, concurrent slow queries, and WebSockets. Record Hermit cgroup peak/RSS, client memory, PostgreSQL connections, p95/p99, 413/503 counts, interrupted JSON, and restart behavior. Decide the production values and update this document, the packaged sysconfig, and the README deployment guidance together.
