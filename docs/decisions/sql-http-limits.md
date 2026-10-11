# SQL-over-HTTP limits

These are Barnacle's current defaults for `/sql`. The [deployment guide](../deployment.md#configuration-reference) lists the settings you can change.

| Limit | Default | If exceeded |
| --- | ---: | --- |
| Request body | 10 MiB | The request returns `413`. |
| JSON nesting | 32 containers | The authenticated request returns `413`. |
| JSON tokens per request | 16,384 | The authenticated request returns `413`. |
| Parameters per query | 4,096 | The authenticated request returns `413`. |
| Connection URL / bearer token | 16 KiB each | The request returns `431` before reading the body. |
| Decoded username / database name | 1 KiB each | The request returns `431` before reading the body. |
| Decoded password | 4 KiB | The request returns `431` before reading the body. |
| Queries in one batch | 100 | A larger batch returns `400`. |
| Data in one row | 8 MiB | The result is rejected. |
| Buffered result | 10 MiB | A single query or batch returns `413`. |
| Uncompressed response JSON | 2 GiB | Streaming stops if it reaches this limit. |
| Concurrent `/sql` requests | 8 | Additional requests return `503`. |
| Upstream connections, including WebSockets | 32 | Additional connections return `503`. |
| PostgreSQL connection and authentication | 5 seconds | Connection fails; no JSON decoding or SQL execution. |
| Time per HTTP query | 60 seconds | The query is canceled. Each batch query gets its own timer. |
| Time for the whole HTTP request | 10 minutes | The request is canceled, including a batch or streamed result. |
| Time to read an HTTP request | 15 seconds | A slow or incomplete request is closed. |

## Request parsing

The HTTP slot is acquired before reading the upload. Credential and routing headers are validated first, including a 16 KiB plus seven-byte Authorization header cap. Barnacle reads the complete raw body under the byte limit before acquiring an upstream slot. Slow uploads therefore cannot occupy PostgreSQL connections. PostgreSQL authentication must succeed before JSON decoding; its five-second timer is also bounded by the remaining whole-request deadline.

After authentication, an allocation-free scan bounds nesting and tokens before JSON decoding allocates maps, arrays, or parameter values. Tokens include object keys, scalar values, and opening containers throughout the request, including unknown and duplicate fields. A JSON document encoded inside a string counts as one string and remains subject to the body byte limit. Number precision and PostgreSQL array conversion are preserved. The fixed complexity and credential limits apply independently of the configurable body limit.

This follows OWASP guidance to [bound input complexity before parsing allocations](https://cheatsheetseries.owasp.org/cheatsheets/Input_Validation_Cheat_Sheet.html#parse-safely-then-validate) and [limit resource use and timeouts](https://cheatsheetseries.owasp.org/cheatsheets/Denial_of_Service_Cheat_Sheet.html). The numeric limits are Barnacle policy choices, not OWASP-prescribed values.

## Choosing a response mode

Single queries and batches buffer results by default. The 10 MiB buffer limit applies to the complete result, so a batch shares it across all its queries. For example, 500 rows averaging about 20 KiB of raw values approach the limit; large text, JSON, or binary fields can reach it sooner. The size of the table does not matter, only the rows returned.

For a single query that needs a larger result, send `Barnacle-Response-Mode: streaming`. This avoids the 10 MiB total buffer limit, while the 8 MiB per-row and 2 GiB response limits still apply. Some custom types still need buffering. Streaming reduces Barnacle's memory use, but the Neon JavaScript driver reads the full JSON response into client memory.

The 2 GiB limit is a maximum response size, not a transfer-time guarantee. Barnacle cancels database work after 60 seconds per query and ends the whole HTTP request after 10 minutes. A slow client or database may hit a time limit first.

## What callers see

- Oversized bodies return `413` before connecting to PostgreSQL. Missing, malformed, duplicate, or oversized credential headers are rejected before reading the body. Failed PostgreSQL authentication takes precedence over JSON errors. After authentication, excessive JSON complexity returns `413` and malformed JSON returns `400`; both close the connection without executing SQL.
- Oversized buffered results return `413`. A batch with more than 100 queries returns `400`; requests over the connection or concurrency limit return `503`.
- A streamed response may already have begun when a later row, timeout, or response limit causes it to stop. In that case the client may see HTTP `200` with incomplete JSON rather than a new error status.
- Ordinary transactional batches roll back earlier writes when a later query or result limit fails before commit. A single write query can complete in PostgreSQL before Barnacle discovers that its result is too large.

## Test results

- Request hardening was compared against the prior implementation: the 300,000-object expansion payload now rejects with 4.8 MiB cumulative allocations instead of 135.8 MiB. Local HTTP measurements found no large throughput regression. See [request validation benchmarks](../../benchmarks.md#http-request-validation-cost) for parsing overhead, methodology, and limitations.

- A 512 MiB result streamed successfully through a Barnacle container limited to 256 MiB; the container peaked at 19.4 MiB. A full 2 GiB response was not tested.
- Eight concurrent buffered queries, each returning 8 MiB, completed in a 256 MiB container. Peak memory was 218.8 MiB with the earlier 10 MiB JSON response cap.
- A short per-query limit reset between batch queries. A slower query was canceled and its earlier batch write rolled back; cancellation also worked through PgBouncer transaction pooling.

These are local Docker measurements, not a capacity guarantee for the target host. See the [larger result](../../benchmarks.md#100-mib-result-comparison) and [connection](../../benchmarks.md#http-connection-reuse-decision) benchmarks for more detail.
