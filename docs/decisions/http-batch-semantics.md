# SQL-over-HTTP batch semantics

- **Status:** Decided — conditional atomicity with PostgreSQL transaction-control semantics
- **Date:** 2026-10-04
- **Scope:** `POST /sql` with a `queries` array

## Contract

Barnacle opens one PostgreSQL connection, begins a transaction, runs the query objects sequentially, collects and encodes the full response, then commits. An error before commit rolls back ordinary transactional writes. The tests cover a later SQL error, PostgreSQL statement timeout, Barnacle's whole-request deadline, client abort during a later query, the buffered-result limit, and the final JSON response limit. The final JSON is now encoded and checked **before** commit, so its `413` response does not leave earlier writes committed. Once commit succeeds, a later client disconnect cannot undo it.

**Explicit transaction-control SQL is allowed and follows PostgreSQL semantics.** It can end Barnacle's transaction, so batches containing it are not unconditionally atomic. In particular, `COMMIT` persists an earlier write despite a later query error. `ROLLBACK` discards earlier work, but a later write can run in a new implicit transaction and persist despite a subsequent error. `BEGIN` inside the already open transaction leaves the later error to roll it back. Clients that need all-or-nothing writes must not include transaction-control commands or SQL with nontransactional side effects. Barnacle does not attempt keyword filtering: safely rejecting every transaction-control form would require a PostgreSQL-aware parser or another server-side enforcement mechanism, and Neon accepts these commands too.

Each query object must contain one SQL statement. Barnacle uses PostgreSQL's extended query protocol, which rejects multiple commands in a prepared query with SQLSTATE `42601`. The HTTP connection-string allowlist rejects `default_query_exec_mode=simple_protocol` with `400` / `BARNACLE_ERROR`; clients cannot select simple protocol through that option. PostgreSQL's [protocol documentation](https://www.postgresql.org/docs/current/protocol-flow.html) describes the extended-protocol multi-command restriction and the effect of explicit transaction commands.

The 100-query limit counts JSON query objects, not SQL statements. One `BARNACLE_QUERY_TIMEOUT` deadline covers connection setup, every query, and commit. Batch results share the configured buffered-result budget. Ordinary PostgreSQL query errors return `400` with the SQLSTATE; configured result limits return `413`.

## Integration evidence

The local [batch integration tests](../../integration/batch-atomicity.test.mjs) use a disposable table and check persistence after each request. A short-deadline gateway checks Barnacle's own timeout, and a low-response-limit gateway checks the final encoding path.

The configured `NEON_COMPARE_DATABASE_URL` was also exercised on 2026-10-04 with equivalent read-only batch inputs:

| Input | Barnacle | Neon proxy |
| --- | --- | --- |
| `COMMIT`, `ROLLBACK`, or `BEGIN` between a successful query and `1/0` | `400`, `22012` | `400`, `22012` |
| Two statements in one query string | `400`, `42601` | `400`, `42601` |
| `default_query_exec_mode=simple_protocol` with `select 1` | `400`, unsupported connection option | `200`; adding two statements still returns `42601`, so acceptance does not enable simple protocol there |
| PostgreSQL statement timeout after an earlier query | `400`, `57014` | `400`, `57014` |
| About 5 MiB of batch result data | `413` under Barnacle's 4 MiB buffered limit | `200` |
| Client abort during a later sleeping query | Client receives `AbortError` | Client receives `AbortError` |

The Neon comparison role (`pgext_probe` in `test2`) has neither database nor `public` schema `CREATE` privilege. Creating the disposable table returned `400` (`permission denied for schema public`), so remote write persistence, Barnacle's configured deadline, and Barnacle's configured response-size limit could not be compared. The local integration tests prove those persistence outcomes against PostgreSQL. PgBouncer pooling modes have a separate [integration check](../../integration/pgbouncer/pool-modes.mjs).
