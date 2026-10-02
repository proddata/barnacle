# Hermit compatibility TODO

This is a gap check for Hermit as a **single PostgreSQL instance** proxy, compared with `@neondatabase/serverless` 1.2.0 and Neon's SQL-over-HTTP/WebSocket implementation. Checked 2026-10-02. Items below describe missing behavior; a passing smoke test does not imply full protocol parity.

## Working baseline

| Surface | Today | Evidence |
| --- | --- | --- |
| `POST /sql` single query | Parameterized query and Neon-shaped result | `integration/neon.test.mjs` |
| `POST /sql` batch | One transaction with an array of results | `integration/neon.test.mjs` |
| HTTP bearer header | Value passed to PostgreSQL as password or SASL OAUTHBEARER token | `integration/neon.test.mjs`, `integration/oauth/run.mjs` |
| `GET /v2` and `/v1` WebSocket | Binary PostgreSQL wire tunnel; SCRAM login tested | `integration/neon.test.mjs`, `ws_test.go` |
| `OPTIONS /sql` | Browser preflight for configured origin | `sql.go` |
| `GET /healthz` | Process liveness | `main.go` |

## P0 — close gaps in the advertised API

- [x] **Accept the driver's batch isolation values.** `ReadUncommitted`, `ReadCommitted`, `RepeatableRead`, and `Serializable` all map to PostgreSQL transaction options and pass through `sql.transaction(..., { isolationLevel })` tests.
- [x] **Finish the parameter and type matrix.** The generated suite covers 55 built-in PostgreSQL type and parameter cases in both raw-text and plain-JSON modes; earlier integration cases cover custom enums, domains, composites, and ranges. A live comparison against a separately deployed Neon proxy matched 107 of 110 raw/plain variants, including result metadata. The three differences are in Neon's plain array conversion: two empty-array queries returned HTTP 500 and `box[]` was split at commas. Hermit preserves the PostgreSQL values. `integration/type-compare.mjs` reports these known differences and fails on new ones; the local Neon checkouts remain inspection-only.
- [x] **Match common result metadata and plain JSON conversion.** DDL reports `rowCount: null`; SELECT and INSERT counts, field metadata, duplicate names, array mode, `fullResults`, a custom type parser, and unusual scalar/array values have integration coverage. The generated type matrix compares metadata and both JSON modes with a deployed Neon proxy.
- [x] **Bound ordinary HTTP result memory.** Single queries stream, while the defaults cap HTTP concurrency at eight queries, raw row data at 8 MiB, buffered batch/custom-type data plus row overhead at 4 MiB, and uncompressed response JSON at 128 MiB. Size-limit regressions cover first-row, buffered-batch, and many-empty-row HTTP 413 responses. The historical 100 MiB comparison is in [benchmarks.md](benchmarks.md#100-mib-result-comparison).
- [x] **Handle huge fields before pgx materializes them.** Hermit configures pgx's PostgreSQL wire reader to reject oversized message bodies before allocation. With pgx 5.11, a 100 MiB single row returned HTTP 413 with a 13.4 MiB Hermit cgroup peak under the default policy, while 100 × 1 MiB still completed at 20.0 MiB. A limit reached after HTTP streaming begins still interrupts JSON rather than returning a clean 413.
- [x] **Prove OAuth/JWT with a real PostgreSQL validator.** The disposable PostgreSQL 18 suite uses [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator), a local HTTPS issuer, a pinned audience and scope, and direct `appuser` identity mapping. A published Neon driver query succeeds with a signed JWT. Invalid signatures and audiences fail at Hermit's gate; wrong scope and requested role reach PostgreSQL and are rejected by its validator. The PostgreSQL 17 SCRAM Compose setup remains the ordinary developer suite. The pinned validator image build is in CI but the actual GitHub job still needs observation.
- [x] **Add an optional OIDC access-token gate before PostgreSQL.** Issuer and audience are pinned; discovery and JWKS are fetched over HTTPS, RS256 and `at+jwt` are required, and invalid HTTP bearer tokens or WebSocket cookies fail before dialing PostgreSQL. Tests cover key rotation, expiry, wrong issuer/audience, invalid signatures, and JWKS failure. PostgreSQL authentication remains separate.
- [x] **Map auth and SQL errors for the driver.** PostgreSQL auth failures now return HTTP 400 with SQLSTATE and Neon-shaped error fields; network failures return a generic 502. Integration covers wrong password, missing role/database, syntax and column errors, and a unique constraint violation. SCRAM intentionally returns `28P01` for both wrong passwords and missing roles.

## P1 — transport and operational confidence

- [ ] **Support per-query `arrayMode` in request bodies.** Neon's batch `QueryData` can set `arrayMode` for each query; Hermit only reads the header for the whole request.
- [ ] **Exercise `Pool.query()` over HTTP.** Add an integration case with `neonConfig.poolQueryViaFetch = true`, as well as `arrayMode` and `fullResults` options. Keep the existing `Client`/WebSocket test.
- [ ] **Verify cancellation.** Abort a slow HTTP fetch and check that the PostgreSQL query stops promptly. Check WebSocket CancelRequest forwarding and cleanup after abrupt client disconnects. Add a database `statement_timeout` policy if request cancellation alone is insufficient.
- [ ] **Harden WebSocket shutdown and limits.** Test close handshake, ping/pong, fragmented and large frames, half-open connections, idle sessions, connection count, and backpressure under slow clients. The current relay uses two copy goroutines per session and closes both sockets when either side ends.
- [ ] **Test PostgreSQL native TLS through the WebSocket tunnel.** The relay forwards bytes, so the client/server TLS negotiation may work, but it is not in the integration suite. `HERMIT_PG_SSLMODE` currently configures only HTTP pgx connections.
- [ ] **Add readiness and basic metrics.** `/healthz` is process liveness only. Add an optional database readiness check, active WebSocket count, HTTP latency/error counts, and bounded log fields without credentials.
- [ ] **Run end-to-end deployment checks.** HAProxy `https`/`wss`, Compose, and a Fedora RPM build have run locally. Browser CORS, graceful restart, and the actual GitHub Fedora CI job still need observation.
- [ ] **Decide whether HTTP connection reuse is needed.** Each HTTP request opens a PostgreSQL connection today. Measure the latency and connection cost next to Postgres before adding pooling. If added, isolate connections by effective credentials and ensure no token/session state crosses requests.

## Deliberately outside the current target

Neon platform routing, compute wake-up, `Neon-Pool-Opt-In`, `Neon-Request-Id`, REST broker paths, console redirect auth, and multi-tenant endpoint management are not needed for one fixed PostgreSQL upstream. Add them only if Hermit's deployment model changes. The serverless driver's default Neon URLs still need explicit `fetchEndpoint` and `wsProxy` overrides for Hermit.

## Sources inspected

- Published `@neondatabase/serverless` 1.2.0, installed from npm in `integration/`, especially `src/http/index.ts`, `src/http/types.ts`, and `CONFIG.md` in the local inspection checkout at `/Users/georg/Developer/neon/serverless`.
- Neon proxy's `proxy/src/serverless/sql_over_http.rs`, `json.rs`, `http_util.rs`, and `websocket.rs` in `/Users/georg/Developer/neon/neon`.
- Upstream references: [serverless configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md), [Neon SQL-over-HTTP implementation](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/sql_over_http.rs), and [Neon JSON conversion](https://github.com/neondatabase/neon/blob/main/proxy/src/serverless/json.rs).

The local checkouts are reference material only. Hermit's integration tests install published packages normally and do not import either checkout.
