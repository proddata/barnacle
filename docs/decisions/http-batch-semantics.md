# SQL-over-HTTP batch semantics

- **Status:** Deferred — decide before promising unconditional batch atomicity to customers
- **Date:** 2026-10-04
- **Scope:** `POST /sql` with a `queries` array

## Current behavior

Hermit opens one PostgreSQL connection, begins a transaction, runs the query objects sequentially, collects their results, and commits. Errors before commit trigger a deferred rollback. The 100-query limit counts JSON query objects, not SQL statements. One `HERMIT_QUERY_TIMEOUT` deadline covers connection setup, every query, and commit. Batch results share the configured buffered-result budget.

Hermit does not parse SQL. By default pgx uses PostgreSQL's extended query protocol, which rejects multiple SQL commands in one prepared query. A client-supplied connection string can set `default_query_exec_mode=simple_protocol`, however, and that mode accepts multiple commands in one query string. A single transaction-control command such as `COMMIT` can also interfere with the transaction Hermit opened. The current batch integration tests prove successful results and isolation settings, but do not prove rollback on a later failure or behavior with these inputs. Treat the batch as one transaction for ordinary statements, not as an enforced SQL statement boundary.

## Decisions to make

1. Should Hermit force one extended-protocol execution mode for all HTTP queries, regardless of connection-string options?
2. Should batch query objects reject explicit transaction-control commands? If so, choose a PostgreSQL-aware parser or another robust enforcement method; semicolon and keyword matching are insufficient.
3. What error contract should clients receive for a rejected multi-command query or transaction-control command?
4. Should batch count, request size, buffered result size, and the whole-request deadline remain at their current defaults? The sizing decision is tracked in [SQL-over-HTTP limits](sql-http-limits.md).

## Evidence required

- Integration tests that confirm a later SQL error rolls back earlier writes, including through PgBouncer transaction pooling.
- Tests for a multi-command query string, a connection-string request for simple protocol, and explicit transaction-control SQL in a batch.
- Tests for timeout, client cancellation, and result-limit failure after an earlier write, confirming what persists in PostgreSQL.
- A representative customer workload to size the batch count, total duration, and result budget.

Do not change the execution behavior until the contract and compatibility impact are decided together.
