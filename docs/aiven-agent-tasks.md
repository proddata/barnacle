# Agent task list: Hermit on Aiven PostgreSQL nodes

Each task below is self-contained and can be given to a coding agent (Claude Code, Codex, etc.) on its own. Background and rationale are in [aiven-integration-requirements.md](aiven-integration-requirements.md).

## Shared rules for every task

- **Reference behaviour:** Hermit follows the Neon proxy and the `@neondatabase/serverless` driver. Jira tickets are not the spec. If Hermit deliberately differs from Neon, record it in `todo.md`.
- **Style:** match the surrounding Go code (small functions, `slog`, env-var configuration parsed in `main.go`, no new dependencies unless the task says so).
- **Verify before finishing:** `go vet ./...`, `go test -race ./...`. For integration changes, run `./integration/run.sh` against a local PostgreSQL (see [development.md](development.md)). Report failing output rather than skipping a check.
- **Docs:** update `README.md`, `docs/api.md` or `docs/deployment.md` for any new setting or behaviour change.
- **Scope:** one task per branch/PR. Do not refactor unrelated code. Do not commit unless asked.
- **Aiven context:** Hermit runs as a sidecar on each Aiven PG node, dials the node's HAProxy `pg_port` (TCP, routes to the Patroni leader) or PgBouncer, and uses the node certificate (`/etc/pki/aiven/service.crt`, key `/run/aiven/service-keys/service.key`, CA `/etc/pki/aiven/ca-bundle.crt`).

Priority order: T1, T2, T3 first; T4 to T8 next; packaging and CI (T9 to T13) in parallel.

---

## T1. `HERMIT_PG_TLS_SERVER_NAME` (blocking)

**Problem.** `internal/gateway/pgtls.go` `PGTLSConfig(host)` sets `tls.Config.ServerName` to the host it dials. On Aiven nodes Hermit dials a local or internal IP, but the certificate is issued for the service DNS name, so verification fails.

**Do.**
- Add optional env `HERMIT_PG_TLS_SERVER_NAME`. When set, use it as `ServerName` for upstream verification in both paths: WebSocket (`DialPostgres` in `pgtls.go`) and HTTP (`connectionConfig` in `internal/sqlhttp/sql.go`, which calls `c.PGTLSConfig(host)`).
- Plumb it through `gateway.Config` and parse it in `main.go`. When unset, behaviour is unchanged.
- Never disable verification as a side effect.

**Tests.**
- Go: extend `internal/gateway/pgtls_test.go`: handshake to an IP address succeeds with the right server name, fails with a wrong one, and fails when unset and the cert has only a DNS SAN.
- Integration: extend `integration/pg-tls.mjs` (or add a case) to connect by IP with the server name set, over both HTTP and WebSocket.

**Done when.** Both transports verify the cert against the configured name, documented in `docs/deployment.md` configuration reference.

---

## T2. Multiple allowed browser origins

**Problem.** `HERMIT_ALLOWED_ORIGIN` accepts one exact origin (`internal/gateway/origin.go`). PG Studio is served from several consoles (prod, staging, regions).

**Do.**
- Accept a comma-separated list in `HERMIT_ALLOWED_ORIGIN` (keep the single-value form working). Exact string match only; no wildcards or regexes. Trim spaces; reject empty or malformed entries at startup with a clear error.
- Update `OriginAllowed`, `AllowOrigin`, `Preflight`, and the WebSocket origin check (`internal/pgws/ws.go` uses `OriginAllowed`).
- Echo only the matching request origin in `Access-Control-Allow-Origin`, with `Vary: Origin` (already present).

**Tests.** Extend `internal/gateway/origin_test.go` for: two configured origins, an unlisted origin (403), no Origin header (allowed), same-origin fallback unchanged. Update `integration/browser-cors.mjs` if it can cover a second origin cheaply.

**Done when.** Docs (`docs/api.md` `Origin` row, `docs/deployment.md` table) describe the list form.

---

## T3. Trusted proxies for forwarded headers

**Problem.** `OriginAllowed` trusts `X-Forwarded-Proto` from any peer when deciding the scheme for same-origin checks. Behind HAProxy this header should be trusted only from the proxy.

**Do.**
- Add `HERMIT_TRUSTED_PROXIES` (comma-separated CIDRs or IPs, default empty = trust nobody).
- Only honour `X-Forwarded-Proto` (and any forwarded client IP used for logging) when `r.RemoteAddr` is in the list.
- Keep the default safe: with no list, use only `r.TLS`.
- Document HAProxy settings that make the same-origin path work (`http-request set-header X-Forwarded-Proto https if { ssl_fc }`).

**Tests.** Unit tests for trusted and untrusted peers, IPv4 and IPv6, invalid CIDR rejected at startup.

---

## T4. Neon status-code and error-shape parity audit

**Goal.** Know exactly where Hermit's HTTP statuses and error bodies differ from the Neon proxy, and align where the driver or common clients depend on it.

**Do.**
1. Read Neon's SQL-over-HTTP implementation (repo `neondatabase/neon`, `proxy/src/serverless/sql_over_http.rs`) and the driver (`neondatabase/serverless`, `src/httpQuery.ts`). Write down, per failure case, Neon's status code and body shape: bad JSON, missing query, too many queries, request too large, response too large, query error, auth failure, connection failure, timeout, saturation.
2. Compare with Hermit (`internal/sqlhttp/sql.go`: `apiError`, `dbError`, `dbConnectError`, `writeLimitedJSON`; `docs/api.md` status table). Today Hermit uses 413 for oversize results and `{"message", "code":"HERMIT_ERROR"}` for gateway errors.
3. Produce `docs/decisions/neon-error-parity.md` with a table: case, Neon, Hermit, decision (match / keep and document).
4. Implement the "match" decisions with integration tests that call the published driver and check the thrown error (`NeonDbError` fields) as well as raw `fetch` status and body.

**Rules.** If the driver treats the case identically regardless of status code, prefer keeping Hermit's current code and documenting it. Do not invent codes the driver cannot use.

**Done when.** The decision doc exists, tests cover each "match" row, and `docs/api.md` is consistent.

---

## T5. Result-limit failure after streaming starts

**Problem.** For single queries `streamSingle` (`internal/sqlhttp/stream.go`) starts writing JSON before the result is complete. If a size limit or error hits mid-stream, the client gets a truncated 200 response. `todo.md` already notes this.

**Do.**
- Measure how Neon behaves for the same situation (it enforces a response size limit; check whether it buffers).
- Pick one approach and document it in `docs/decisions/sql-http-limits.md`: (a) buffer up to a configurable threshold before sending headers so most limit errors still return a clean status, (b) keep streaming and make the failure detectable (for example abort the connection so the client sees a network error, not a valid prefix), or (c) both.
- Ensure a mid-stream failure can never end with a syntactically valid but incomplete JSON document.
- Add metrics/log kind for interrupted streams (already `interruptedResultKind`).

**Tests.** Integration: large result exceeding the limit; assert the published driver rejects with an error instead of returning partial rows. Include a many-small-rows case and a single large-row case.

---

## T6. HTTP batch atomicity tests and contract

**Problem.** Batches run in one transaction, but nothing proves rollback behaviour, and `docs/decisions/http-batch-semantics.md` leaves open what transaction-control SQL does.

**Do.**
- Add integration tests (`integration/`): (1) a failing later query rolls back earlier writes; (2) a timeout after an earlier write leaves nothing persisted; (3) client abort mid-batch leaves nothing persisted; (4) result-limit failure after an earlier write leaves nothing persisted; (5) `COMMIT`, `ROLLBACK`, `BEGIN` as a batch element; (6) a multi-statement query string; (7) a connection string containing `default_query_exec_mode=simple_protocol` (must be rejected, see `connectionConfig` allowed-keys switch).
- Run the same inputs against a Neon proxy if available (`NEON_COMPARE_DATABASE_URL`, see `integration/type-compare.mjs` for the pattern) and record the differences.
- Decide the contract and update `docs/decisions/http-batch-semantics.md` (the claim that simple protocol can be selected looks wrong; verify with test 7 and correct the doc).
- If transaction-control SQL must be rejected, use a PostgreSQL-aware approach or rely on server-side behaviour; do not use naive keyword matching.

---

## T7. PgBouncer safety: prepared statements and session state

**Problem.** pgx's default statement cache uses named prepared statements. Through PgBouncer in transaction pooling that can fail (`prepared statement does not exist`). Aiven nodes expose PgBouncer in transaction mode by default.

**Do.**
- Reproduce with a PgBouncer container (add a compose profile or an integration script modelled on the routing test in CI) in session, transaction and statement pool modes.
- Choose an execution mode that is safe behind poolers (for example `QueryExecModeExec` or cache-describe, with `PgBouncer`'s `max_prepared_statements` noted) and make it configurable (`HERMIT_PG_QUERY_EXEC_MODE` or auto), keeping binary/text result handling intact. The code path is `conn.Query(ctx, q.Query, pgx.QueryResultFormats{...}, args...)` in `sql.go`.
- Test HTTP single queries, batches with isolation/read-only options, and cancellation through each pool mode.
- Document which features work per pool mode (WebSocket `SET` and prepared statements only work in session mode; that is a PgBouncer property, not Hermit's).

---

## T8. WebSocket session semantics tests

**Problem.** The tunnel passes bytes through, but the tests (`integration/websocket.test.mjs`, `internal/pgws/ws_test.go`) do not cover what PG Studio needs: transactions, `SET`, prepared statements, cursors, `LISTEN`/`NOTIFY` behaviour, and COPY.

**Do.** With the published Neon `Client`/`Pool` over `/v2`, add tests that: open a transaction across several `query()` calls; `SET` then read back with `SHOW`; create and use a named prepared statement; declare and fetch from a cursor; verify `Pool` reuses a connection state correctly; verify idle-timeout and clean close release the PG backend (check `pg_stat_activity`).

**Done when.** Failures (if any) are fixed in `internal/pgws`, or documented as limitations with the reason.

---

## T9. Fix and harden RPM packaging

**Do.**
- `packaging/rpm/build.sh` runs `cp -R web "$source_dir/"` under `set -eu`, but the repo has no `web/` directory (the browser app is `console/`). Confirm and remove or correct the copy. Verify with `./packaging/rpm/build.sh` inside a Fedora container (see the `fedora` job in `.github/workflows/ci.yml`).
- Make the build reproducible offline: add a vendored-modules source (`go mod vendor` tarball) and build with `-mod=vendor`, so the spec no longer needs `GOPROXY=off` plus a prior `go mod download`. Keep `THIRD-PARTY-*` license files in the RPM.
- Check `BuildRequires: golang >= 1.25.14` against Fedora 42/43/44 build roots; document how a build root without that version should get the toolchain.
- Add an arm64 RPM build and install smoke test in CI.

---

## T10. Aiven-ready systemd unit variant

**Problem.** `packaging/rpm/hermit.service` uses `DynamicUser=yes` and `ProtectSystem=strict`. On Aiven nodes Hermit must read the certificate key under `/run/aiven/service-keys/` (group-restricted) and `/etc/pki/aiven/`, and prune writes unit and environment files.

**Do.**
- Add a documented variant (drop-in or second unit) with a fixed system user and group, `SupplementaryGroups=` for key access, and `ReadOnlyPaths=` for the two directories. Keep the memory limits and hardening that still apply.
- Keep `HERMIT_PG_ADDR`, `HERMIT_PG_TLS_SERVER_NAME`, `HERMIT_PG_CA_FILE`, `HERMIT_LISTEN`, and `HERMIT_TRUSTED_PROXIES` as environment variables only, so prune can template them.
- Extend the Fedora systemd test (`integration/fedora-service/`) to run the variant and verify it can read a key with restrictive group permissions.

---

## T11. CI: HAProxy leader frontend and PgBouncer topologies

**Do.** Add CI jobs (in `.github/workflows/ci.yml`) with Docker Compose topologies close to Aiven v2 nodes: (1) Hermit, then HAProxy in TCP mode with no TLS termination, then PostgreSQL over TLS; (2) Hermit, then PgBouncer, then PostgreSQL; (3) Hermit, then HAProxy, then PgBouncer. Run the Neon-driver integration suite against each. Use `HERMIT_PG_TLS_SERVER_NAME` for certificate verification.

The existing `integration/tls/` job covers only public HTTPS/WSS termination.

---

## T12. CI: Patroni failover test

**Do.** A job (can be scheduled nightly) with a two-node Patroni cluster plus HAProxy using `HEAD /leader` health checks, and Hermit dialing the HAProxy frontend. During active HTTP queries and WebSocket sessions, trigger a switchover. Assert: in-flight sessions close promptly with a clear close code, new connections reach the new leader within a bounded time, no Hermit restart is needed, and `/readyz` reflects the state. Record results in `benchmarks.md`.

---

## T13. Release workflow and source artifacts

**Do.** Add a tag-triggered GitHub Actions workflow that builds the release candidate artifacts the CI already produces (static binaries, RPM, `.deb`, OCI archive), plus a source tarball and a vendor tarball, then publishes them with `SHA256SUMS`. Aiven's package build pins sources by checksum, so artifacts must be stable and downloadable by URL. Do not publish automatically on branch pushes. Document the release process in `docs/development.md`.

---

## T14. Migration docs for Neon users

**Do.** Add `docs/neon-migration.md` describing the driver configuration for both transports against a Hermit endpoint: `neonConfig.fetchEndpoint`, `neonConfig.wsProxy`, `useSecureWebSocket`, `forceDisablePgSSL`, `pipelineConnect=false` for SCRAM, `webSocketConstructor`, connection-string differences (host ignored in fixed mode, password required unless bearer), and a short table of known differences from Neon (no routing or wake-up, different limits and error codes from T4, `Neon-Pool-Opt-In`/`Neon-Request-Id` ignored). Test every snippet against the integration environment. Link from `README.md`.

---

## T15 (optional). Native TLS listener with certificate reload

Do this only if HAProxy termination is not used. Add `HERMIT_TLS_CERT_FILE` and `HERMIT_TLS_KEY_FILE`, serving HTTPS and WSS directly (minimum TLS 1.2). Reload the pair on SIGHUP and on file change (poll modification times or use `tls.Config.GetCertificate` with a cached reloader). Failed reloads keep the old certificate and log an error. Tests: rotate files while connections are open; new connections get the new certificate. Do not implement unless the platform team chooses native TLS.
