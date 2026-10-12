# Hermit on Aiven PostgreSQL nodes: required improvements

Audience: Hermit developer. Context: shipping Hermit as a sidecar on Aiven PostgreSQL nodes (background: Jira PGSQL-289 and PGSQL-290).

**Source of truth:** the Jira tickets are background only and are not the specification. Hermit's compatibility target is the **Neon proxy and the `@neondatabase/serverless` driver**. Where the tickets and Neon's actual behaviour differ (status codes, error shapes, response format), follow Neon and treat the ticket as wrong. Hermit may deliberately differ from Neon where the platform requires it (no multi-tenant routing, different limits); list such differences in `todo.md`.

**Design principle:** Hermit should behave like the other service ports (`pg_port`, `pgbouncer_port`). It uses the same node certificate, the same customer `ip_filter`, the same public/private/privatelink exposure, and it follows failover. The platform side lives in aiven-core (`py/pg-prune`, `py/prune`); this document covers what Hermit itself must provide.

Items marked **(verify)** are assumptions about aiven-core that were read, not run.

## 1. How Hermit will be deployed

- One Hermit per PG node, as an RPM in the PG service container (`py/pg-service-container-NN/aiven.toml` dependency list) with a systemd unit written by prune. CI already builds a Fedora RPM.
- **v2 nodes:** HAProxy (TCP mode, no TLS termination) fronts PG. `pg_port` is the HAProxy frontend; PG itself listens on `pg_direct_port`. HAProxy routes `pg_port` to the Patroni leader (`HEAD /leader`). Hermit should dial the **`pg_port` frontend** (or `pgbouncer_port`), so a Hermit on any node follows failover.
- PgBouncer listens on `pgbouncer_port` and authenticates clients itself.
- Hermit's own port is registered as a new ingress port in prune, which makes it inherit `ip_filter`, `public_access`, `private_access` and `privatelink_access` like the PG port. This is platform work, not a Hermit change.

## 2. Certificates and TLS (required)

Aiven's PG and PgBouncer both use:

| File | Path |
| --- | --- |
| Cert chain | `/etc/pki/aiven/service.crt` |
| Key | `/run/aiven/service-keys/service.key` |
| CA bundle | `/etc/pki/aiven/ca-bundle.crt` |

### 2.1 Upstream TLS server name (blocking)
`PGTLSConfig(host)` in `internal/gateway/pgtls.go` sets `ServerName` to the address it dials. The service cert is issued for the service DNS name, so dialing a node IP or `127.0.0.1` fails verification.

- Add `HERMIT_PG_TLS_SERVER_NAME`. It applies to WebSocket (`DialPostgres`) and HTTP (`PGTLSConfig(host)` in `sql.go`).
- Keep the default behaviour when unset.
- Tests: handshake succeeds when dialing an IP with the server name set, and fails on a mismatch (extend `pgtls_test.go` and `integration/pg-tls.mjs`).

### 2.2 Listener TLS
Hermit has no TLS listener (`main.go` calls `ListenAndServe`). Two options; pick one with the platform team:

- **A (preferred first step):** HAProxy terminates TLS with the service cert and forwards to Hermit on loopback. Needs from Hermit: trusted-proxy handling (see 3.3) and nothing else.
- **B:** native TLS. `HERMIT_TLS_CERT_FILE` / `HERMIT_TLS_KEY_FILE`, reload on file change or SIGHUP (the cert is renewed on the node; the runbook says a node can otherwise keep serving an old one until `aiven-pruned` restarts), minimum TLS 1.2, key readable via group membership.

## 3. Behaviour to match the other ports

### 3.1 Multiple allowed browser origins
`HERMIT_ALLOWED_ORIGIN` accepts a single exact origin. PG Studio runs on several consoles (prod, staging, regions). Accept a comma-separated list, with exact matching only (no wildcards). Update `OriginAllowed`, `AllowOrigin`, `Preflight`, the WebSocket origin check, `docs/api.md`, and the browser CORS test.

### 3.2 Network access controls
Hermit must not implement its own IP filter. Document that the platform filters at the port level, and that Hermit must bind per environment:

- `HERMIT_LISTEN` supports binding to the node/internal address, not only `:8080`.
- Make sure fixed-upstream mode is the documented default: `HERMIT_PG_ALLOWED_ADDRS` unset, so clients cannot choose upstreams. Consider failing startup if both a public listen address and an allowlist are set, unless explicitly confirmed.

### 3.3 Trusted proxy handling
`OriginAllowed` trusts `X-Forwarded-Proto` from any client. Add `HERMIT_TRUSTED_PROXIES` (CIDRs); only honour forwarded headers from those peers. Needed if HAProxy fronts Hermit. Also decide real client IP logging for metrics and rate limiting.

### 3.4 Authentication
- PG authenticates the user (SCRAM inside the tunnel for WebSocket; `Neon-Connection-String` or Bearer for HTTP). Do not bake service-admin credentials into Hermit's environment.
- The OIDC gate and `pg_oauth_validator` path only apply if PG has the validator. Keep it off by default on managed nodes.
- Document the exact credential flow PG Studio uses (open product decision, see section 6).

### 3.5 Resource and process limits
- Dedicated user, systemd `MemoryHigh`/`MemoryMax`, `GOMEMLIMIT`, `OOMScoreAdjust`, restart policy (already in the RPM unit).
- `HERMIT_MAX_CONNECTIONS` must be sized below PG's connection budget and below PgBouncer's limits per plan tier.
- Characterize memory/CPU per plan tier. Not yet done: currently only a local Docker measurement exists (`benchmarks.md`).
- `statement_timeout` stays a PG-side policy.

### 3.6 Observability
- `HERMIT_METRICS=true` on a private/loopback address for the node's Telegraf to scrape.
- Add upstream-failure, rejection and active-session counters with stable names (some exist in `gateway.Metrics`).
- Log lines must not contain credentials or SQL (the existing HAProxy canary check already covers headers; repeat it for Aiven's logging pipeline).

## 4. Neon-compatibility checks and robustness fixes

The reference is the Neon proxy source and the serverless driver, compared against a deployed Neon proxy where possible (`integration/type-compare.mjs` already does this for types and metadata). Do not change a behaviour to match a ticket unless Neon does the same.

| # | Topic | Observation in Hermit | Action |
| --- | --- | --- | --- |
| 1 | Status code for oversize results | Hermit returns **413** (`writeLimitedJSON`, `dbError`, `docs/api.md:133`). The ticket asks for 507; I have not confirmed what Neon returns. | Read Neon's `sql_over_http` error mapping, record its status codes for request-too-large and response-too-large, and match them if the driver or clients depend on them. If Hermit keeps its own codes, document the difference. Different codes are acceptable if the driver treats all non-2xx with a JSON error body the same way. |
| 2 | Limit hit after HTTP streaming starts | The JSON body is cut off with a 200 status (`streamSingle`; admitted in `todo.md`). | Check how Neon behaves (it buffers up to its limit). Either buffer before sending headers up to a threshold, or document the failure mode and make sure the driver surfaces a clear error rather than hanging. |
| 3 | Batch rollback | No test proves a later failing query rolls back earlier writes (no `rollback` test in `integration/`). | Add tests: later failure, timeout, cancellation, and result-limit failure after an earlier write. Compare with Neon's batch semantics. |
| 4 | Transaction-control SQL in a batch | `COMMIT`/`ROLLBACK` inside a batch ends Hermit's transaction early (`docs/decisions/http-batch-semantics.md`). | Test what Neon does with the same input and align, or reject explicitly. |
| 5 | Stale decision doc | The doc says a connection string can select `simple_protocol`; `connectionConfig` (`sql.go:92-101`) rejects every query option except `sslmode`, `channel_binding`, `application_name`, `require_auth`. | Add a test proving `default_query_exec_mode` is rejected, then correct the doc. |
| 6 | Array mode / raw text | Hermit returns raw text only with `Neon-Raw-Text-Output`, otherwise JSON-converted values. This matched the Neon proxy in 107 of 110 compared cases. | No change; keep the three known differences documented. |
| 7 | Error codes on non-query failures | Hermit uses its own `HERMIT_ERROR` code for gateway-level errors and 502/503 for upstream and saturation. | Compare with Neon's error bodies and statuses for auth failure, bad request, timeout and saturation. Match where the driver inspects them; otherwise keep Hermit's codes and document them in `docs/api.md`. |

## 5. Verification still missing

- PgBouncer in each pool mode: HTTP batches, WebSocket sessions, `SET`, prepared statements, cancellation, TLS, failures (`todo.md`). pgx's default statement cache uses named prepared statements, which breaks under transaction pooling. Consider forcing an exec mode that is safe for PgBouncer when the upstream is a pooler, or document it.
- WebSocket session semantics (transactions, `SET`, prepared statements) through the tunnel. The current WebSocket tests only exercise simple queries and large messages.
- Failover: behaviour during a Patroni switchover with active WebSockets and in-flight HTTP queries.
- Load and soak test on a representative PG node (concurrent WebSockets, slow clients, restarts).
- The Neon driver defaults need documenting for migrators: `pipelineConnect=false` for SCRAM, `forceDisablePgSSL`, `useSecureWebSocket`, `wsProxy`, `fetchEndpoint`.
- Aiven developer docs page covering the driver configuration (this repo only has a README).

## 6. Product/platform decisions (not Hermit code)

1. **Credentials for PG Studio.** Browser-direct access means the browser needs a database credential. Today acorn holds the encrypted service-user password and decrypts it per query. Options: user types a password, a short-lived role, or OAuth through a PG validator.
2. **v1 nodes.** Without HAProxy there is no leader-following frontend; decide whether v1 is supported.
3. **Port and DNS.** Add a Hermit component to `PGServiceComponent`, user config, DNS usages, and `get_service_ports` in prune, so it appears in service connection info like the other ports.
4. **What stays in almond.** Admin and maintenance calls (users, databases, PgBouncer stats) remain in almond, which works when customer networking blocks inbound access. Interactive SQL moves to Hermit. Almond's `readonly`, `schema` and `SET SESSION AUTHORIZATION` options have no Hermit equivalent and would need replacing by PG roles or acorn logic.
5. **Limits per plan tier** (row, buffer, response, concurrency, timeouts). Hermit exposes the knobs; the values need a decision (`docs/decisions/sql-http-limits.md`).

## 7. Build, packaging and CI

How aiven-core ships comparable sidecars (`crabsniproxy`, `aiven-patroni-callbacks-go`, `aiven-pg-gatekeeper`): each has a directory under `deps/` with a `.spec`, a `sources` checksum file (written by `avnpkg newsources`), and an `rpms.json` with one pinned version per Fedora release (fc42, fc43, fc44). The spec uses `Epoch: 1000`, `Release: N.aiven%{?dist}` and `Vendor: Aiven`. Packages are built by `avnpkg` in the Buildkite `aiven-deps` pipeline and uploaded with `reposit`. The container image then lists the RPM in `py/pg-service-container-NN/aiven.toml`.

### Hermit-side changes
1. **Build must work offline from a self-contained source tarball.** `packaging/rpm/build.sh` runs `go mod download`, and the spec builds with `GOPROXY=off`. Aiven's builder consumes `Source:` files listed in `sources`, so Hermit needs either a vendored-modules tarball (`go mod vendor`, built with `-mod=vendor`) or a documented, checksummed source release. A tagged GitHub release tarball plus a vendor tarball is the usual shape.
2. **Fix the RPM script's `web` copy.** `packaging/rpm/build.sh` runs `cp -R web ...` under `set -eu`, but the repo has no `web/` directory (the browser app is `console/`). On a clean checkout this looks like it fails the script. Confirm against CI and remove the line if the directory is gone.
3. **Go toolchain version.** The spec requires `golang >= 1.25.14`. Check what the Fedora 42/43/44 build roots used by `aiven-deps` provide. If one is older, either relax the requirement where safe, or build with a toolchain download. `go.mod` carries `go 1.25.14` and `golang.org/x/text 0.39.0`, both chosen to clear `govulncheck`.
4. **Spec conventions for Aiven.** Support overriding `Release`/`Epoch`/`Vendor` (or let Aiven carry a thin wrapper spec in `deps/hermit/` that consumes Hermit's release tarball). Keep the licence files (`THIRD-PARTY-*`) in the RPM, as today.
5. **systemd unit must suit a prune-managed container.** The shipped unit uses `DynamicUser=yes`, `ProtectSystem=strict`, and `HERMIT_PG_ADDR=127.0.0.1:5432` in sysconfig. On Aiven nodes prune writes the unit and environment, and the process must read the cert key under `/run/aiven/service-keys/` (group-restricted) and `/etc/pki/aiven/`. Provide a variant (or documented overrides) with a fixed system user/group, `SupplementaryGroups`, and `ReadOnlyPaths` for those directories. `DynamicUser` and group-based key access do not mix.
6. **Both architectures.** `packaging/release/build.sh` builds amd64 and arm64. The RPM CI job only builds on the runner's architecture; add an arm64 RPM build and install smoke test if Aiven nodes run on arm.

### CI additions
- **Run the PG-container-like test:** an integration job where Hermit dials PG through HAProxy in TCP mode (leader frontend) and through PgBouncer. The existing HAProxy job covers only public TLS termination.
- **Patroni failover test:** a switchover while WebSocket and HTTP traffic is active (section 5).
- **PgBouncer pool modes** (session, transaction, statement) with the published Neon driver.
- **Cert rotation test:** replace the cert/key files and confirm Hermit picks them up (if native TLS is chosen) and that upstream verification works with `HERMIT_PG_TLS_SERVER_NAME`.
- **Real PG matrix:** keep PG 14-19; confirm Aiven's supported versions are covered. Aiven nodes run Fedora, and the Fedora integration job already exists.
- **Release hygiene:** CI already runs govulncheck, npm audit and a container scan. Add a signed tag/release workflow that publishes the source and vendor tarballs plus `SHA256SUMS`, so Aiven's `sources` file can pin them.
- **Aiven-side (not in this repo):** `deps/hermit/` with spec, `sources`, `rpms.json`; entry in `pg-service-container-NN/aiven.toml`; spellcheck word list entry if CI there checks docs.

## 8. Suggested order of work

1. `HERMIT_PG_TLS_SERVER_NAME` and tests (blocks any node deployment).
2. Multiple origins and trusted-proxy handling.
3. Neon-reference checks from section 4 (status codes, rollback tests, streaming-failure behaviour).
4. PgBouncer and failover validation on a v2 test service.
5. Platform wiring in prune (ingress port, unit, env file) and the load test per plan tier.
