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
| Estimated heap held by request bodies and buffered results, all requests together | `GOMEMLIMIT` less 64 MiB (96 MiB for the packaged 256 MiB service) | A result that could never fit returns `413`; one that does not fit beside other requests returns `503`. |
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

## Memory budget

The 10 MiB buffered-result limit caps one result by its size on the wire, but decoded rows occupy several times that, and eight requests can buffer at once. A one-column integer result of 10 MiB holds about 30 MiB of heap, and one row with a large array holds far more than its text. Request bodies are similar: a 10 MiB body is about 40 MiB while it is decoded. Measured with eight concurrent requests, results alone peaked at 220 to 350 MiB and bodies at 330 to 345 MiB, above a 256 MiB service limit. When a service exceeds its cgroup limit the kernel kills the whole process, dropping every WebSocket session and query, not just the request that caused it.

Barnacle therefore keeps a gateway-wide budget of **estimated** heap, shared by request bodies and buffered results. Unset, `BARNACLE_HTTP_MAX_MEMORY_MIB` is derived as `GOMEMLIMIT` less a fixed 64 MiB reserve, so raising the service's memory limit and `GOMEMLIMIT` together raises the budget (the startup log states the value and its source). That is 96 MiB for the packaged 256 MiB service (`GOMEMLIMIT` 160 MiB) and 256 MiB at 512 MiB. With no `GOMEMLIMIT` it is 96 MiB. [The decision record](memory-limits.md) explains these fractions and how to change them.

- **Requests** are charged as bytes arrive, so an announced but unsent body costs nothing. Decoding adds about four times the body (the decoder's copy, the decoded values, and pgx's send buffer) plus a small amount per JSON token; once decoding is done only about twice the body (the decoded values and the send buffer) stays charged until the result has been read. A batch frees each query's parameters as it finishes.
- **Buffered results** are charged per row by what the decoded values and JSON encoding allocate: map or slice overhead, boxed values, and an element cost for every array item and JSON member. The per-item figures are approximations measured on the current Go and pgx, kept deliberately rough. Tests compare both estimates with measured heap for many shapes, so a change that makes requests or rows heavier fails them.
- **Result metadata** is charged for every query, including those returning no rows, before response field containers are allocated. The estimate includes column descriptors, names, and their JSON encoding copies. Batch queries retain these reservations until the response is sent; streaming also reserves its field metadata before sending headers.
- **Streaming responses** are not buffered, so the 10 MiB buffered limit and this budget do not apply to their total size. The per-row (8 MiB) and total-response (2 GiB) limits still do. Text and JSON stream without a copy; a row with an array column is parsed whole, which is about 50 times its text, so such a row is charged while it is converted and returned straight after.
- A request that could never fit, even with the whole budget to itself, returns `413`. One that does not fit beside others returns `503` with a message naming the memory budget, and counts in `barnacle_limit_rejections_total{limit="http_memory"}`. It is safe to retry, or to stream.
- Reservations never wait: a request that does not fit returns what it holds and gets the `503`. Under heavy contention several large requests can therefore all be refused at once; each is safe to retry.

Typical queries are far below the budget: eight concurrent 1 MiB results, or eight 20,000-row results, fit with room to spare. WebSocket sessions do not use it. The [memory scenarios](../development.md#memory-scenarios) benchmark shows how it behaves at 256 and 512 MiB.

## Choosing a response mode

Single queries and batches buffer results by default. The 10 MiB buffer limit applies to the complete result, so a batch shares it across all its queries. For example, 500 rows averaging about 20 KiB of raw values approach the limit; large text, JSON, or binary fields can reach it sooner. The size of the table does not matter, only the rows returned.

For a single query that needs a larger result, send `Barnacle-Response-Mode: streaming`. This avoids the 10 MiB total buffer limit, while the 8 MiB per-row and 2 GiB response limits still apply. Some custom types still need buffering. Streaming reduces Barnacle's memory use, but the Neon JavaScript driver reads the full JSON response into client memory.

The 2 GiB limit is a maximum response size, not a transfer-time guarantee. Barnacle cancels database work after 60 seconds per query and ends the whole HTTP request after 10 minutes. A slow client or database may hit a time limit first.

## What callers see

- Oversized bodies return `413` before connecting to PostgreSQL. Missing, malformed, duplicate, or oversized credential headers are rejected before reading the body. Failed PostgreSQL authentication takes precedence over JSON errors. After authentication, excessive JSON complexity returns `413` and malformed JSON returns `400`; both close the connection without executing SQL.
- Oversized buffered results return `413`; a result that does not fit beside other large results returns `503` and can be retried. A batch with more than 100 queries returns `400`; requests over the connection or concurrency limit return `503`.
- A streamed response may already have begun when a later row, timeout, or response limit causes it to stop. In that case the client may see HTTP `200` with incomplete JSON rather than a new error status.
- Ordinary transactional batches roll back earlier writes when a later query or result limit fails before commit. A single write query can complete in PostgreSQL before Barnacle discovers that its result is too large.

## Test results

- Request hardening was compared against the prior implementation: the 300,000-object expansion payload now rejects with 4.8 MiB cumulative allocations instead of 135.8 MiB. Local HTTP measurements found no large throughput regression. See [request validation benchmarks](../../benchmarks.md#http-request-validation-cost) for parsing overhead, methodology, and limitations.

- A 512 MiB result streamed successfully through a Barnacle container limited to 256 MiB; the container peaked at 19.4 MiB. A full 2 GiB response was not tested.
- Before the memory budget, eight concurrent buffered queries, each returning 8 MiB, completed in a 256 MiB container with a peak of 218.8 MiB, and other row shapes (a text-array column peaked at 348 MiB) exceeded it. Eight concurrent 10 MB request bodies peaked at 331 to 345 MiB. With the budget, none of the scenarios below was OOM-killed.
- `integration/memory-scenarios.mjs` (see the [development guide](../development.md#memory-scenarios)) ran realistic workloads in a container with a real cgroup limit, with `GOMEMLIMIT` at 5/8 of it and the budget derived by Barnacle (96 MiB at 256 MiB, 256 MiB at 512 MiB) (the packaged profile). Peak cgroup memory in MiB, against the limit, for 256 MiB / 512 MiB:

| Workload (8 clients unless stated) | Peak, 256 / 512 MiB | Outcome |
| --- | --- | --- |
| Primary-key lookups | 38 / 45 | all succeed, p95 6 ms |
| Dashboard batch of 4 aggregates | 24 / 29 | all succeed |
| 1,000-row list pages | 45 / 43 | all succeed, 120 MiB/s |
| 2,000 jsonb documents (about 2 MiB) | 116 / 115 | all succeed |
| 5,000 rows with array columns | 84 / 78 | all succeed |
| 5 API, 2 dashboards, 1 buffered 8k-row export | 48 / 51 | all succeed |
| 4 bulk inserts of about 2 MiB | 75 / 72 | all succeed |
| 4 simultaneous 9.9 MB request bodies | 159 / 256 | 48% / 100% succeed, the rest `503` |
| 4 streamed 200k-row exports (about 40 MiB each) plus 4 API clients | 52 / 48 | all succeed, memory flat |
| 4 streamed rows with a 100,000-element array | 48 / 53 | all succeed |
| 20 WebSocket sessions plus 8 HTTP clients | 46 / 49 | all succeed |
| Abuse: 4 streamed 6 MB array rows | 73 / 208 | 0% / 50% succeed, the rest `413`/`503` |
| Abuse: 5 million buffered rows, 8 clients | 159 / 280 | `413` / `503`, no crash |
| Abuse: buffered `text[]` of 5 million rows, 8 clients | 147 / 281 | `413` / `503`, no crash |

Realistic workloads are identical at both sizes because they fit in the smaller budget; the larger tier only matters for the heavy ones (maximum-size bodies and large simultaneous results), where more of them succeed. The worst peak was 62% of the limit at 256 MiB and 56% at 512 MiB; the [decision record](memory-limits.md) compares a profile closer to the limit. These are local measurements in Docker Desktop's Linux VM on macOS, not a capacity guarantee; repeat them on the target hardware.

- A short per-query limit reset between batch queries. A slower query was canceled and its earlier batch write rolled back; cancellation also worked through PgBouncer transaction pooling.

These are local Docker measurements, not a capacity guarantee for the target host. See the [larger result](../../benchmarks.md#100-mib-result-comparison) and [connection](../../benchmarks.md#http-connection-reuse-decision) benchmarks for more detail.
