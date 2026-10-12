# Barnacle security review

Reviewed: branch `use-libraries-for-ws-and-gzip` at `72b26b9` (Barnacle 0.1.0), 2026-10-11.
Scope: all Go source (about 3.5k lines excluding tests), packaging (RPM/deb/systemd, Dockerfiles), CI/release workflows, docs. The dev console and `integration/` harness were only skimmed (not shipped in the RPM).
Target use: one gateway per PostgreSQL node, on thousands of services, reachable through the platform's public/private ingress.

## Verdict

**Not ready for fleet-wide production as is.** The core design is sound (PostgreSQL authenticates, nothing is stored, upstream TLS is verified, input is bounded before parsing). But three defects can take the gateway down or weaken authentication, and none are mitigated by the code itself. All four findings below were reproduced locally, not just read.

| # | Severity | Finding | Needs credentials |
|---|---|---|---|
| 1 | High | Unauthenticated clients can exhaust the shared connection budget | No |
| 2 | High (conditional) | WebSocket relay cannot enforce an auth method; all clients share the gateway's source IP | No |
| 3 | Medium | Buffered-result memory budget is under-counted 20-35x; 8 requests exceed the 256 MiB cgroup | Yes |
| 4 | Medium | No listener connection cap; flood can OOM-kill and trip the systemd start limit | No |
| 5 | Medium | No access/audit logging, no client IP, no auth-failure metric | n/a |
| 6 | Medium | Same-origin rule trusts the attacker-controlled `Host` header (DNS rebinding) | n/a |
| 7 | Medium | Insecure defaults in the binary (plain HTTP on all interfaces, `/metrics` public, `*` in Compose) | n/a |
| 8 | Medium | `*` allowlist is an SSRF / generic TCP relay; unit has no egress restriction | No |
| 9 | Medium | systemd unit hardening is minimal | n/a |
| 10 | Low | Supply chain: unsigned artifacts, tag-pinned actions/images, no dependency automation, no SECURITY.md | n/a |
| 11 | Low | Smaller items (CA trust mode, pre-auth error detail, cleartext/md5 accepted, array recursion) | varies |

## Findings

### 1. High: unauthenticated clients exhaust the shared budget
Slots are taken **before** any authentication and held for a long time:

- `/v2`: `AcquireUpstream` at `internal/pgws/ws.go:38`, then the session lives until PostgreSQL closes it or the 30 min idle timeout (`ws.go:101`). No deadline for the startup packet or for completing login.
- `/sql`: `AcquireHTTP` at `internal/sqlhttp/sql.go:365` happens before the body is read (`:380`). The connection string only has to be well-formed; a made-up password is enough to reach the slot.

Reproduced against a local build (defaults: 32 connections, 8 HTTP):

```text
32 unauthenticated WebSocket upgrades   -> all 101, no credentials sent
33rd WebSocket                          -> 503
legitimate /sql                         -> 503 "postgres connection limit reached"
8 POSTs claiming a body and sending none -> each holds a slot ~14.5 s (15 s read timeout); 9th /sql -> 503
```

Caveat: a real PostgreSQL closes a silent pre-auth socket after `authentication_timeout` (60 s default), so the WS case needs about 0.5 new connections/s to keep the gateway saturated rather than a one-off. That is still trivial, and a customer `ip_filter` of `0.0.0.0/0` is the default. There is no per-source limit and no way to reserve capacity for authenticated users.

Fix, in order of value:
1. Pre-auth deadlines: close a WS session that has not completed login within N s (parse the backend `R`/`Z` messages in the relay you already inspect for `K`); read the HTTP body before taking the upstream slot, using a separate small "in-flight body" semaphore.
2. Per-source-IP concurrent connection cap (needs real client IP, see finding 5).
3. Rate limit or tarpit on repeated auth failures per source.
4. At the ingress: HAProxy `maxconn`, `stick-table` connection/rate limits per source. This is the cheapest mitigation if HAProxy fronts Barnacle as planned.

### 2. High (conditional): WebSocket path cannot enforce an authentication method
**Status: fixed in `internal/pgws/loginguard.go`** (opt-out `BARNACLE_WS_ALLOW_TRUST_AUTH`). The description below is the original finding. Still verify `pg_hba.conf` on the node image, since address-based rules remain a risk for HTTP and for finding 5.

The HTTP path sets `require_auth=password,md5,scram-sha-256`, so a `trust` rule is refused. The WS path is a byte relay (`ws.go:83-101`) with no such check. Reproduced against PostgreSQL 17 with `trust`:

```text
HTTP /sql, made-up password   -> 502 (refused by require_auth)
WS, startup packet, no password -> AuthenticationOk
```

Because every client reaches PostgreSQL from the gateway's address, any `pg_hba.conf` rule that grants access by source address (`trust`, loopback rules, `peer`-like patterns, or a broad `host ... <node-ip>/32` allow) becomes an anonymous login for **any role, including admin roles**, for anyone who can reach the port. The packaged default upstream is `127.0.0.1:5432`, which is where such rules usually live.

Fix:
- Verify on the Aiven node image that no `trust`/address-based allow rule matches the address Barnacle dials. Make this a release gate.
- Enforce it in code: in the relay, track backend `R` messages and close the session if `AuthenticationOk` arrives without a prior password/MD5/SASL challenge. Offer an explicit opt-out env var for admins who really want `trust` upstream.
- Related loss: PostgreSQL logs, `log_connections`, `auth_delay` and any per-address control see only the gateway. See finding 5.

### 3. Medium: buffered result budget under-counts memory (authenticated)
**Status: fixed** by a gateway-wide estimated-heap budget (`BARNACLE_HTTP_MAX_MEMORY_MIB`, see `docs/decisions/sql-http-limits.md#memory-budget`). Re-measured with the same eight-request harness: peak 64 to 163 MiB instead of 167 to 348 MiB. A second memory user turned up during the fix: eight concurrent 10 MB request bodies peaked at 331 to 345 MiB, so request bodies are now charged to the same budget (about 4x the body while decoding, about 2x after), the budget is derived from the cgroup limit, and streamed rows that hold an array are charged while they are converted. `integration/memory-scenarios.mjs` now measures 13 workloads at 256 and 512 MiB in a real cgroup: no OOM kills. The memory fractions were then deliberately set close to the limit (`GOMEMLIMIT` 3/4, budget 1/2); the data, the accepted risks and how to dial back are in `docs/decisions/memory-limits.md`. The original finding follows.

`collect` charges `64 + 64*cols + bytes` per row (`sql.go:535`) against a 10 MiB budget, but the live Go representation (`map[string]any`, boxed values, parsed arrays) is far larger. Measured with 8 concurrent single-query requests, each stopped by the 10 MiB limit (macOS RSS, indicative; unit cap is `MemoryMax=256M`, `GOMEMLIMIT=160MiB`):

| Query shape (x8 concurrent) | Peak RSS |
|---|---|
| 1 int column, tiny rows | 222 MiB |
| 1 bool column | 247 MiB |
| jsonb ~1 KiB docs | 167 MiB |
| text array column | **348 MiB** |

`GOMEMLIMIT` is soft and cannot stop this. In the packaged unit the result is a cgroup OOM kill, dropping every WebSocket session. A user with valid credentials, or a compromised app, can do this repeatedly.

Fix: cost rows by what they allocate (multiply by a measured factor, count arrays per element), add one global weighted memory semaphore across HTTP requests instead of a per-request budget, lower `BARNACLE_HTTP_MAX_BUFFERED_MIB` (or `MAX_HTTP_QUERIES`) for the Aiven default, and raise `MemoryMax` to leave headroom. Streaming mode is not affected.

### 4. Medium: no listener connection cap; start limit makes an outage permanent
**Status: start limit fixed** (`StartLimitIntervalSec=0` in both units). The listener connection cap is still open; HAProxy `maxconn` is the intended mitigation.

`http.Server` (`main.go:156`) has no connection limit. Measured about 19 KiB per half-open connection (8,000 connections: 20 to 169 MiB). With a 10 s header timeout, roughly 1,200 new connections/s from one host is enough to reach the 256 MiB cap. The unit then restarts after 5 s but `StartLimitBurst=5` per 60 s means a few repeated kills leave it in `failed` until someone runs `reset-failed` (the docs say so). Across thousands of services that is a manual-recovery outage.

Fix: `netutil.LimitListener` (or HAProxy `maxconn`), and in the unit set `StartLimitIntervalSec=0` so the service always comes back.

### 5. Medium: no access logging, no client IP, no auth-failure signal
**Status: failed-login counter added** (`barnacle_login_failures_total`, both transports). Access logging and client IP are left to the ingress: behind HAProxy its logs carry the real address, and Barnacle only adds what the ingress cannot see, namely whether PostgreSQL refused the login. The original finding follows.

The only runtime log lines are startup/shutdown and stream interruptions. There is no record of who connected, from where, to which route, or whether login failed, and `/metrics` has no auth-failure counter. PostgreSQL's own log shows only the gateway address. You cannot detect password spraying or reconstruct an incident.

Fix: structured connection log (time, client IP from the immediate peer or a trusted-proxy header, route, outcome incl. SQLSTATE `28P01`/`28000`, duration, bytes). Never log SQL, headers or credentials. Add `auth_failures_total`. Decide how the real client IP reaches Barnacle (`X-Forwarded-For` from `BARNACLE_TRUSTED_PROXIES`, or PROXY protocol).

### 6. Medium: same-origin check trusts `Host`
`origin.go:114` allows any request where `Origin == scheme://Host`. A DNS-rebinding page sets both to its own name and passes. Verified: `Host: attacker.example`, `Origin: http://attacker.example` reaches the handler (400 for missing credentials), while a foreign origin gets 403. Credentials are still required, so impact is limited, but it defeats the assumption that a browser on the customer's network cannot talk to the gateway.

Fix: add a Host allowlist (reject unknown `Host` with 421/403) and treat the same-origin shortcut as opt-in.

### 7. Medium: insecure defaults in the binary
- `BARNACLE_LISTEN` defaults to `:8080`, plain HTTP, all interfaces (`main.go:112`). Credentials travel in headers. Packages override to loopback, but the container and any manual run do not.
- `/metrics` is served unauthenticated on the main listener by default (`main.go:150`); the docs rely on the ingress to block it.
- `compose.yaml` defaults `BARNACLE_PG_ALLOWED_ADDRS` to `*`.
- `BARNACLE_PG_SSLMODE=disable` starts silently.

Fix: default listen to loopback; refuse a non-loopback plaintext listen unless TLS or a trusted proxy is configured (or an explicit insecure flag); default metrics to a loopback listener; log a loud warning for `disable` and `*`. This is also what `docs/aiven-integration-requirements.md` §3.2 asks for.

### 8. Medium: wildcard allowlist
`BARNACLE_PG_ALLOWED_ADDRS=*` lets any caller choose `host:port`. Combined with `PG_SSLMODE=disable` the WS endpoint is an anonymous generic TCP tunnel to anything the node can reach (cloud metadata, internal services). Even with `require`, it is a port scanner. Allowlisted hostnames are resolved at dial time. Fixed-upstream mode (the default) is not affected.

Fix: refuse `*` unless an explicit flag is set; for Aiven use fixed upstream and add `IPAddressAllow=`/`IPAddressDeny=any` in the unit so the process can only reach the intended addresses.

### 9. Medium: systemd hardening is minimal
Present: `DynamicUser`, `NoNewPrivileges`, `PrivateTmp`, `ProtectHome`, `ProtectSystem=strict`, memory limits. Missing (not tested with `systemd-analyze security`, which is unavailable on this machine):
`CapabilityBoundingSet=` (empty), `RestrictAddressFamilies=AF_INET AF_INET6`, `SystemCallFilter=@system-service` with `~@privileged @resources`, `ProtectKernelTunables/Modules/Logs`, `ProtectControlGroups`, `ProtectClock`, `ProtectHostname`, `ProtectProc=invisible`, `ProcSubset=pid`, `PrivateDevices`, `LockPersonality`, `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `MemoryDenyWriteExecute`, `UMask=0077`, and `IPAddressAllow` (finding 8). The RPM and deb units are identical copies, so fix both.

### 10. Low: supply chain
- Release assets are unsigned. `SHA256SUMS` is published beside the files it covers, so it does not prove origin. Sign the RPM/deb and the checksum file (GPG or cosign); the OCI image already has SBOM and provenance attestations.
- Actions are pinned by tag (`actions/checkout@v7`, etc.), base images by tag (`alpine:3.21`, `golang:1.26.9-alpine`, `fedora:latest`), not by digest/SHA.
- No Dependabot/Renovate config and no `SECURITY.md` or disclosure contact.
- The release workflow runs `go test` and a binary `govulncheck`, but not the integration suites that CI runs.
- Positive: `govulncheck` is clean (run locally with Go 1.27.2; CI runs 1.26.9, keep that scan as a release gate), Trivy scans both images, `go test -race` and `go vet` run in CI, vendored deterministic source archives, no secrets in git history (the only "PRIVATE KEY" hits are test code that generates keys).

### 11. Low and informational
- `LoadPGRootCAs` (`pgtls.go:20`) **adds** `BARNACLE_PG_CA_FILE` to the system roots. Any public CA-issued certificate for the service hostname would be trusted. For Aiven, trust only the project CA (add a replace-mode option).
- Pre-auth PostgreSQL errors are forwarded verbatim (`sql.go:701`), e.g. `no pg_hba.conf entry for host "10.x.x.x"`, leaking internal addressing to unauthenticated callers; also `file`, `line`, `routine`. Matches Neon's shape but consider scrubbing codes 28xxx/08xxx.
- Default `require_auth` accepts cleartext `password` and `md5` (`sql.go:328`). Safe while upstream TLS is verified; with `PG_SSLMODE=disable` a MITM upstream can downgrade. Prefer `scram-sha-256` only on Aiven.
- `parsePGArray` recursion has no depth cap (`pgjson.go:134`). Only reachable through exotic unknown-OID array types, not found exploitable; add a cap.
- WS sessions have an idle timeout but no maximum lifetime.
- `/readyz` dials PostgreSQL on every unauthenticated hit (limited to one at a time); fine, but keep it off the public ingress.

## Checked and found sound
- **Authentication model:** gateway holds no credentials; PostgreSQL decides. HTTP path refuses `trust` via `require_auth`.
- **Startup-parameter injection:** connection-string options are allowlisted (`sql.go:265-275`, `:284`), and pgx 5.11 refuses NUL bytes in startup values (`pgproto3/startup_message.go`), so a NUL in the user name cannot smuggle `options=` or `replication=`.
- **Routing:** fixed upstream by default; `?address=` and URL host ignored; `Fallbacks` cleared so an alternate host cannot bypass the check; canonicalisation rejects `/ \ @ ? # %`.
- **Upstream TLS:** verify-full with hostname, TLS 1.2 minimum, fails closed when the server answers `N`; pgx cancel requests are encrypted when the main connection is.
- **Request bounds:** body, header, credential, JSON depth/token and parameter limits are enforced before decoding; no request decompression, so no zip bomb; gzip is response-only.
- **Output encoding:** streamed JSON escapes strings and field names correctly; `json`/`jsonb` passthrough relies on PostgreSQL validation.
- **Browser access:** Origin enforced on `/sql` and `/v2`, no `Allow-Credentials`, `Vary: Origin` set, `X-Forwarded-Proto` trusted only from configured peers.
- **TLS listener:** minimum 1.2, atomic certificate reload that keeps the previous pair on a torn or invalid update.
- **Errors:** connection failures return generic messages; credentials are never logged.

## Suggested order of work
1. Findings 1, 2, 4, 5: pre-auth deadlines and caps, auth-method enforcement on WS, listener cap + `StartLimitIntervalSec=0`, access log.
2. Finding 3: global memory budget and corrected cost model, then re-measure on Linux inside the real cgroup.
3. Findings 6-9: Host allowlist, fail-closed defaults, `*` guard, unit hardening drop-in.
4. Finding 10: signing, pinning, Dependabot, `SECURITY.md`.
5. Before rollout: verify `pg_hba.conf` on the node image; run `systemd-analyze security barnacle.service`; load-test on Linux with the production ingress in front.

## Not covered
No test on Linux/systemd or inside a real cgroup (memory figures are macOS RSS), no PgBouncer or HAProxy-on-Aiven topology, no fuzzing beyond the existing tests, no `npm audit` of `console/`, no review of Aiven's platform side (prune, ip_filter, certificates).
