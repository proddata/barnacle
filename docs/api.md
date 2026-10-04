# API and protocol

Hermit is inspired by [Neon's wsproxy](https://github.com/neondatabase/wsproxy). It implements a compatible subset of the driver interface; [the gap list](../todo.md) tracks differences from Neon's full proxy and platform routing.

`neon()` and `sql.transaction()` use HTTP. `Client` and `Pool` use WebSocket unless the driver's `poolQueryViaFetch` option is enabled. Set `fetchEndpoint` and `wsProxy` to Hermit for both transports. The driver's default `pipelineConnect: "password"` assumes cleartext password authentication; turn it off for the SCRAM setup in Compose. See [Neon's driver configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md).

| Method and path | Purpose | State |
| --- | --- | --- |
| `POST /sql` | One parameterized query: `{ "query": "...", "params": [] }` | Working subset |
| `POST /sql` | Sequential queries in one transaction: `{ "queries": [{ "query": "...", "params": [] }] }` | Working subset |
| `OPTIONS /sql` | Browser CORS preflight | Working |
| `GET /v2`, `GET /v1` | WebSocket upgrade; binary PostgreSQL wire bytes | Working |
| `GET /healthz` | Process liveness | Working |
| `GET /readyz` | Process readiness; optionally checks PostgreSQL network and TLS | Working |
| `GET /metrics` | Basic Prometheus metrics when enabled | Optional |
| `GET /` | Manual console when `HERMIT_CONSOLE=true` | Development only |

The HTTP response has `rows`, `fields`, `command`, `rowCount`, and `rowAsArray`; batch responses wrap these in `results`. DDL returns `rowCount: null` when PostgreSQL's command tag has no count. The driver requests `Neon-Raw-Text-Output: true` and `Neon-Array-Mode: true`, so it can apply its own PostgreSQL type parsers. Hermit accepts the corresponding headers, plus `Neon-Batch-Read-Only`, `Neon-Batch-Deferrable`, and all four driver `Neon-Batch-Isolation-Level` values. A query object's `arrayMode` can override the header for that query, including inside a batch. Aborting an HTTP request cancels its PostgreSQL query. WebSocket sessions relay PostgreSQL CancelRequest packets; when a client connection drops unexpectedly, Hermit also sends a bounded cancellation request to stop work left running on that backend.

Hermit does not parse SQL inside a batch query object. The [batch semantics decision](decisions/http-batch-semantics.md) records the open question of enforcing single-statement and transaction-control boundaries before promising unconditional atomicity.

`POST /sql` compresses JSON responses of 1 KiB or more when the client sends `Accept-Encoding: gzip`. Small responses stay plain. The Neon driver uses `fetch()`, so browser and Node HTTP stacks handle response decompression; the driver itself does not set a gzip option. Request bodies remain plain JSON.

Hermit accepts up to 1 MiB per HTTP request and up to 100 queries per batch. An HTTP request must arrive within 15 seconds by default, including its body; an idle HTTP keep-alive connection closes after 60 seconds. The separate HTTP connection and query timeout defaults to 30 seconds. Single-query HTTP responses stream JSON rows; batches and plain JSON responses involving custom PostgreSQL types use a 4 MiB buffered-result budget that counts field bytes and row overhead. By default, one HTTP row may contain up to 8 MiB of PostgreSQL field data and one response up to 128 MiB of uncompressed JSON. pgx's wire reader rejects oversized PostgreSQL messages before allocating their bodies. A result over a limit returns HTTP 413 if detected before headers are sent; once streaming has begun, the JSON response is interrupted. Use the WebSocket path for sessions and very large individual rows. The WebSocket relay does not parse SQL. Its upstream address is fixed by default; optional routing can use the driver's `?address=host:port` parameter.

These are starting limits for a small service. The [SQL-over-HTTP limits decision](decisions/sql-http-limits.md) compares them with Neon's source defaults, explains the memory tradeoffs, and records what still needs measurement before production values are chosen.

Streaming lets Hermit avoid holding a whole result, while the Neon driver's `neon()` API still parses the complete JSON response before returning rows. If PostgreSQL fails after some rows have been sent, Hermit cannot change the HTTP status; the client sees an incomplete JSON response. Errors detected before the first row retain a structured PostgreSQL error response.
