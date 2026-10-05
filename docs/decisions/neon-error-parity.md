# Neon SQL-over-HTTP error parity

- **Status:** Implemented
- **Date:** 2026-10-04
- **Scope:** `POST /sql` and `@neondatabase/serverless` 1.2.0

## Driver contract

The [Neon SQL handler](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/sql_over_http.rs) sends a JSON object with `message`, `code`, and the PostgreSQL detail fields (`severity`, `detail`, `hint`, `position`, `internalPosition`, `internalQuery`, `where`, `schema`, `table`, `column`, `dataType`, `constraint`, `file`, `line`, `routine`). For failures without a PostgreSQL error, those fields are `null`. The [published driver](https://www.npmjs.com/package/@neondatabase/serverless) 1.2.0, built from [`src/httpQuery.ts`](https://github.com/neondatabase/serverless/tree/main/src), parses that JSON and copies fields into `NeonDbError` **only for HTTP 400**. For every other non-OK status, it reads the response as text and throws `NeonDbError("Server error (HTTP status N): ...")`; `code` and other fields are `undefined`. The driver does not expose the raw status as a separate error property.

Neon's `code` is a PostgreSQL SQLSTATE when there is a `DbError`, otherwise `null`. Hermit uses `code: "HERMIT_ERROR"` for its own validation and limit errors, and a SQLSTATE for PostgreSQL errors. We do not synthesize SQLSTATE values for gateway errors.

## Case decisions

| Case | Neon proxy | Hermit | Decision |
| --- | --- | --- | --- |
| Bad JSON | `400`, full error object, `code: null` | `400`, `{message, code:"HERMIT_ERROR"}` | **Keep and document.** Both reach the driver's parsed-error branch; the extra gateway code is not a PostgreSQL SQLSTATE. |
| Missing `query`/`queries` | `400`, full error object, `code: null` (Serde payload parse error) | `400`, minimal gateway error | **Keep and document.** The driver itself always sends a query. |
| Too many queries | No SQL-handler count cap; a batch over 100 can succeed if it fits the request limit. Empty batches produce `200 {"results":[]}`. | `400` when batch count is outside 1–100, minimal gateway error | **Keep and document.** Hermit's bounded batch is an intentional resource limit. |
| Request over body limit | `413`, full error object, `code: null` | **Now `413`**, minimal gateway error (was `400`) | **Match status.** A 400 caused the driver to treat a transport-size failure as a structured database error and expose `HERMIT_ERROR` as `error.code`. Both now take the generic non-400 path. Keep Hermit's 1 MiB limit. |
| Response over limit | `507`, full error object, `code: null` | `413`, minimal gateway error if detected before headers; a streamed response may end as incomplete `200` JSON | **Keep and document.** For a complete response, both non-400 statuses take the same driver path. Hermit's streaming behavior is a separate limitation. |
| SQL query error | `400`, full PostgreSQL fields with SQLSTATE | `400`, PostgreSQL fields with SQLSTATE | **Match existing behavior.** Preserve the SQLSTATE, message, severity, and detail fields used by `NeonDbError`. |
| Password authentication failure | `400`, full object with `code: null` for Neon's proxy-side password check. A PostgreSQL-side connect error can instead be `500` with a SQLSTATE. | `400`, PostgreSQL fields with `code: "28P01"` for a rejected PostgreSQL password | **Keep and document.** Hermit authenticates at PostgreSQL and preserves its real SQLSTATE, which clients can use. |
| Connection failure | `500`, full error object; code usually `null` unless a PostgreSQL error is attached | `502`, minimal gateway error, or a specific TLS/OAuth gateway code | **Keep and document.** The driver takes the same non-400 path. `502` identifies Hermit's upstream boundary. |
| Timeout | Client cancellation in the Neon SQL handler is `400` with SQLSTATE `08P01` for either connect or query cancellation; PostgreSQL statement timeout is `400` with SQLSTATE `57014`. A separate outer HTTP timeout can be `408` with `{msg}`. | Hermit's query deadline generally produces `502` gateway error; PostgreSQL statement timeout is `400` with SQLSTATE `57014`. | **Keep and document.** Neon has no direct equivalent to Hermit's independent 30-second query deadline. Both preserve actual PostgreSQL timeout SQLSTATE. |
| Saturation | SQL-handler connection admission/rate-limit errors are `500`, full error object with `code: null`; a separate outer HTTP API can return `429` or `503` with `{msg}`. | `503`, minimal gateway error when HTTP or upstream slots are full | **Keep and document.** Both SQL-handler responses enter the driver's non-400 path; `503` states that Hermit is temporarily full. |

The Neon statuses above describe the SQL handler's response path, not a guarantee about CDN, ingress, or other Neon HTTP endpoints. Its [payload error mapping](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/error.rs) supplies the 413, and its [backend error classification](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/backend.rs) controls connection and admission failures. The driver's behavior was verified against the pinned published package in `integration/node_modules/@neondatabase/serverless/index.mjs` and exercised by `integration/neon-error-parity.test.mjs`.

## Tests

`integration/neon-error-parity.test.mjs` uses raw `fetch` to check status and JSON body, then the published driver to check `NeonDbError` on the two **match** rows: oversized requests and SQL query errors. The request-size test asserts that a 413 has no driver `code`; the SQL test asserts that a 400 retains SQLSTATE `22012`, severity, and message.
