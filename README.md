# Hermit 🦀

**PostgreSQL has a portable home on the web.** Hermit wraps PostgreSQL wire traffic in a WebSocket connection and gives short queries a separate HTTP entrance. It is a small Go process meant to sit next to one PostgreSQL instance, with HAProxy handling public TLS.

Hermit is licensed under [Apache-2.0](LICENSE). See the
[third-party license inventory](THIRD-PARTY-NOTICES.md) for bundled Go code,
test dependencies, and the container base.

```text
@neondatabase/serverless                      Hermit                     PostgreSQL

neon() / sql.query()   ─── POST /sql ───►  HTTP query handler ─── pgx ───►  :5432
Client / Pool          ─── WS /v2 ─────►  binary wire tunnel ─── TCP ───►  :5432
```

Hermit is inspired by [Neon's wsproxy](https://github.com/neondatabase/wsproxy). It is a **compatible subset**, not a drop-in replacement for Neon's full proxy. [todo.md](todo.md) lists the precise gaps found against the published serverless driver and Neon proxy source.

## Start here

```sh
docker compose up --build
```

Open **[localhost:8080](http://localhost:8080)**. The development console has **Run via HTTP** and **Run via WebSocket** buttons. Use this connection string for both:

```text
postgres://hermit:hermit_dev_password@localhost:5432/hermit
```

Leave the bearer token field blank for this password-based demo. The WebSocket button speaks PostgreSQL wire protocol and authenticates with SCRAM. The HTTP button calls `POST /sql`. Compose keeps PostgreSQL off the host network and binds Hermit to `127.0.0.1:8080`; the example password is for local development only.

### Try the HTTP API without the console

```sh
curl -sS http://localhost:8080/sql \
  -H 'Neon-Connection-String: postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
  -d '{"query":"select $1::int as answer","params":[42]}'
```

Expected `rows`: `[{"answer":42}]`. The hostname in the connection string is never used as a network destination: Hermit always connects to `HERMIT_PG_ADDR` (`postgres:5432` in Compose).

## Use the Neon serverless driver

Install `@neondatabase/serverless` and `ws` in your Node application. These settings direct both driver transports to Hermit:

```js
import { neon, neonConfig, Client } from '@neondatabase/serverless';
import WebSocket from 'ws';

const databaseUrl = 'postgres://hermit:hermit_dev_password@localhost:5432/hermit';

// One-shot SQL over HTTP.
neonConfig.fetchEndpoint = 'http://localhost:8080/sql';
const sql = neon(databaseUrl);
console.log(await sql`select 42::int as answer`);

// A PostgreSQL session over WebSocket.
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = false; // local HTTP demo only
neonConfig.wsProxy = () => 'localhost:8080/v2';
neonConfig.pipelineConnect = false; // SCRAM needs the auth challenge first
const client = new Client(databaseUrl);
await client.connect();
try {
  console.log((await client.query('select current_user')).rows);
} finally {
  await client.end();
}
```

`neon()` and `sql.transaction()` use HTTP. `Client` and `Pool` use WebSocket, unless you explicitly enable the driver's `poolQueryViaFetch` option. In production, send HTTPS and WSS to HAProxy. The driver's default `pipelineConnect: "password"` assumes cleartext password authentication; turn it off for the SCRAM setup in Compose. See [Neon's driver configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md) for those options.

## Authentication: who checks what?

| Entrance | What the client sends | Who verifies it |
| --- | --- | --- |
| HTTP password | `Neon-Connection-String` with user, database, and password | PostgreSQL |
| HTTP bearer | `Authorization: Bearer <token>`; user and database come from the connection string or Hermit defaults | Optional Hermit OIDC gate, then PostgreSQL receives the token as its password |
| WebSocket | Optional access-token cookie, then normal PostgreSQL startup and password/SCRAM messages inside binary frames | Optional Hermit OIDC gate, then PostgreSQL |

By default, Hermit forwards the HTTP bearer value as PostgreSQL's password without validating a JWT. Setting both `HERMIT_OIDC_ISSUER` and `HERMIT_OIDC_AUDIENCE` enables an access-token gate before any PostgreSQL connection. It discovers the issuer's JWKS, permits only RS256 signatures with an `at+jwt` token type, and checks issuer, audience, expiry, issue time, and required access-token claims. Keys refresh every five minutes or when a new key ID appears; a failed refresh rejects requests. The gate follows the [JWT access-token profile](https://www.rfc-editor.org/rfc/rfc9068.html) and [OIDC discovery](https://openid.net/specs/openid-connect-discovery-1_0.html).

With the gate enabled, **every HTTP query needs `Authorization: Bearer <token>`** and every WebSocket upgrade needs a valid `hermit_access_token` cookie. A trusted frontend must set that cookie for Hermit's origin with `Secure`, `HttpOnly`, and an appropriate `SameSite` policy; Hermit does not issue cookies. WebSocket `Origin` checks and PostgreSQL's normal username/password authentication still apply. HTTP passes the verified token to PostgreSQL through SASL OAUTHBEARER when the server requests it, or as a password for older authentication setups. The Compose PostgreSQL 17 SCRAM setup rejects real JWTs. PostgreSQL 18 also needs a separately configured validator module and role mapping for native OAuth authentication; Hermit does not provide that module.

A bearer request looks like this once such a PostgreSQL validator is configured:

```sh
curl -sS https://db.example.com/sql \
  -H 'Authorization: Bearer <jwt>' \
  -H 'Neon-Connection-String: postgres://app@db.example.com/app' \
  -d '{"query":"select current_user","params":[]}'
```

With the driver, use `neon(databaseUrlWithoutPassword, { authToken: getToken })` and point `fetchEndpoint` at Hermit. The regular integration suite proves header forwarding with a password-shaped token. A separate [PostgreSQL 18 OAuth test](#postgresql-oauth-end-to-end) exercises a signed JWT through Hermit and [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator).

## Endpoint contract

| Method and path | Purpose | State |
| --- | --- | --- |
| `POST /sql` | One parameterized query: `{ "query": "...", "params": [] }` | Working subset |
| `POST /sql` | Atomic batch: `{ "queries": [{ "query": "...", "params": [] }] }` | Working subset |
| `OPTIONS /sql` | Browser CORS preflight | Working |
| `GET /v2`, `GET /v1` | WebSocket upgrade; binary PostgreSQL wire bytes | Working |
| `GET /healthz` | Process liveness | Working |
| `GET /` | Manual console when `HERMIT_CONSOLE=true` | Development only |

The HTTP response has `rows`, `fields`, `command`, `rowCount`, and `rowAsArray`; batch responses wrap these in `results`. DDL returns `rowCount: null` when PostgreSQL's command tag has no count. The driver requests `Neon-Raw-Text-Output: true` and `Neon-Array-Mode: true`, so it can apply its own PostgreSQL type parsers. Hermit accepts the corresponding headers, plus `Neon-Batch-Read-Only`, `Neon-Batch-Deferrable`, and all four driver `Neon-Batch-Isolation-Level` values.

`POST /sql` compresses JSON responses of 1 KiB or more when the client sends `Accept-Encoding: gzip`. Small responses stay plain. The Neon driver uses `fetch()`, so browser and Node HTTP stacks handle response decompression; the driver itself does not set a gzip option. Request bodies remain plain JSON.

Hermit accepts up to 1 MiB per HTTP request and up to 100 queries per batch. HTTP queries have a 30 second default timeout. Single-query HTTP responses stream JSON rows; batches and plain JSON responses involving custom PostgreSQL types use a 4 MiB buffered-result budget that counts field bytes and row overhead. By default, one HTTP row may contain up to 8 MiB of PostgreSQL field data and one response up to 128 MiB of uncompressed JSON. pgx's wire reader rejects oversized PostgreSQL messages before allocating their bodies. A result over a limit returns HTTP 413 if detected before headers are sent; once streaming has begun, the JSON response is interrupted. Use the WebSocket path for sessions and very large individual rows. The WebSocket relay does not parse SQL and ignores `?address=` to keep the upstream fixed.

Streaming lets Hermit avoid holding a whole result, while the Neon driver's `neon()` API still parses the complete JSON response before returning rows. If PostgreSQL fails after some rows have been sent, Hermit cannot change the HTTP status; the client sees an incomplete JSON response. Errors detected before the first row retain a structured PostgreSQL error response.

## Deploy near PostgreSQL

Set `HERMIT_PG_ADDR` to the nearby server, keep the Hermit listener private, and let HAProxy terminate public TLS. The repository includes a starting [HAProxy configuration](haproxy.cfg) with WebSocket-friendly timeouts and a health check. HAProxy should set `X-Forwarded-Proto: https` when serving the console through TLS.

| Variable | Default | Effect |
| --- | --- | --- |
| `HERMIT_LISTEN` | `:8080` | HTTP and WebSocket listen address |
| `HERMIT_PG_ADDR` | `127.0.0.1:5432` | **Only** PostgreSQL network destination |
| `HERMIT_PG_USER` | `postgres` | Default user for bearer HTTP requests |
| `HERMIT_PG_DATABASE` | `postgres` | Default database for bearer HTTP requests |
| `HERMIT_PG_SSLMODE` | `disable` | Upstream TLS for the HTTP pgx connection: `disable` or `require` |
| `HERMIT_QUERY_TIMEOUT` | `30s` | HTTP connection and query deadline |
| `HERMIT_MAX_CONNECTIONS` | `32` | Maximum simultaneous HTTP and WebSocket PostgreSQL connections; excess requests receive 503 |
| `HERMIT_MAX_HTTP_QUERIES` | `8` | Maximum simultaneous HTTP queries; excess requests receive 503 |
| `HERMIT_HTTP_MAX_ROW_MIB` | `8` | Maximum raw PostgreSQL field data in one HTTP row |
| `HERMIT_HTTP_MAX_BUFFERED_MIB` | `4` | Total field data plus estimated row overhead retained across a buffered batch or custom-type result |
| `HERMIT_HTTP_MAX_RESPONSE_MIB` | `128` | Maximum uncompressed HTTP result JSON bytes |
| `HERMIT_OIDC_ISSUER` | empty | Exact issuer URL; set with `HERMIT_OIDC_AUDIENCE` to enable the access-token gate |
| `HERMIT_OIDC_AUDIENCE` | empty | Required resource audience when the OIDC gate is enabled |
| `HERMIT_ALLOWED_ORIGIN` | empty | One additional allowed browser origin, e.g. `https://app.example.com` |
| `HERMIT_CONSOLE` | `false` | Expose the manual console at `/` |

Same-origin browser requests work without extra configuration. Set `HERMIT_ALLOWED_ORIGIN` to the exact origin of a separate frontend. Do not expose the development console publicly. HTTP upstream TLS is controlled by `HERMIT_PG_SSLMODE`; WebSocket traffic is a byte tunnel, so any PostgreSQL-native TLS negotiation comes from the client and has not yet been integration tested.

### Fedora

Build a local RPM on Fedora with Go 1.25 or newer:

```sh
sudo dnf install golang rpm-build systemd-rpm-macros
./packaging/rpm/build.sh
sudo dnf install ./dist/hermit-*.rpm
sudoedit /etc/sysconfig/hermit
sudo systemctl enable --now hermit
```

The package installs `/usr/bin/hermit`, a systemd service, and `/etc/sysconfig/hermit`. The service listens on `127.0.0.1:8080` and connects to PostgreSQL on `127.0.0.1:5432` by default, so set the latter in the config file if PostgreSQL is elsewhere. The RPM build downloads Go modules before making its source archive, then builds and tests offline inside `rpmbuild`. CI runs the build in a Fedora container and uploads the RPM. The package includes Hermit's [Apache-2.0 license](LICENSE) and the [third-party license inventory](THIRD-PARTY-NOTICES.md).

The unit restarts Hermit after a crash or OOM kill, waits five seconds between attempts, and stops after five starts in one minute. Its cgroup begins throttling at 192 MiB and has a hard 256 MiB memory limit with no swap; `GOMEMLIMIT=160MiB` asks Go to collect earlier. `OOMScoreAdjust=500` makes Hermit a more likely victim than an unadjusted PostgreSQL process if the whole host runs out of memory. These are starter limits for a small proxy; measure your query sizes and concurrent WebSocket sessions before raising them. The hard limit keeps Hermit's memory use bounded, but a request that needs more memory can fail and drop its connection.

Inspect restarts and memory use with `systemctl status hermit`, `journalctl -u hermit`, and `systemctl show hermit -p MemoryCurrent -p MemoryPeak -p NRestarts`. Tune cgroup limits without editing the packaged unit:

```sh
sudo systemctl edit hermit
```

In the editor, add:

```ini
[Service]
MemoryHigh=384M
MemoryMax=512M
```

Then restart the service:

```sh
sudo systemctl restart hermit
```

Also adjust `GOMEMLIMIT` in `/etc/sysconfig/hermit` when changing `MemoryMax`. If repeated failures hit the start limit, fix the cause and run `sudo systemctl reset-failed hermit && sudo systemctl start hermit`. `HERMIT_MAX_CONNECTIONS=32` limits the PostgreSQL backends Hermit can create across both transports; tune it below PostgreSQL's available connection budget. PostgreSQL still needs its own work memory and statement timeout settings.

## Test and inspect

With the Compose stack running, test the same endpoint the console uses with the published Neon driver:

```sh
npm ci --prefix integration
TEST_DATABASE_URL='postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
HERMIT_BASE_URL='http://localhost:8080' \
npm test --prefix integration
```

The database URL's host is ignored by Hermit; PostgreSQL stays inside the Compose network. The suite covers HTTP queries and types, bearer forwarding, batches, gzip, Neon's WebSocket `Client`, and the console's SCRAM wire client.

For a native development setup with PostgreSQL listening on the host, run `go test ./...` and `go vet ./...`, then use `integration/run.sh` to build and start a temporary Hermit process:

```sh
npm ci --prefix integration
HERMIT_PG_ADDR=127.0.0.1:5432 \
HERMIT_PG_USER=hermit \
HERMIT_PG_DATABASE=hermit \
TEST_DATABASE_URL='postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
./integration/run.sh
```

The integration suite installs `@neondatabase/serverless` and `ws` from npm using the committed lockfile. [CI](.github/workflows/ci.yml) defines Ubuntu and Fedora jobs; the Ubuntu job also builds the Docker image.

The suite includes 55 built-in PostgreSQL type and parameter cases in both raw-text and plain-JSON modes. To compare the same cases with a deployed Neon proxy, put a **disposable** connection URL in the gitignored `integration/.env` as `NEON_COMPARE_DATABASE_URL=...`, then run `node integration/type-compare.mjs` while the Compose stack is up. Set `NEON_COMPARE_ENDPOINT=...` there only for a custom SQL endpoint. The comparator checks rows and result metadata and never prints the connection URL. In the 2026-10-02 run, 107 of 110 raw/plain variants matched. Neon's plain-JSON path returned HTTP 500 for empty integer arrays and split `box[]` elements at commas; Hermit preserves those PostgreSQL values. The comparison reports these three observed differences separately and fails on any other difference.

### PostgreSQL OAuth end to end

Run the disposable PostgreSQL 18 OAuth suite with Docker, Git, OpenSSL, and Node 24:

```sh
npm ci --prefix integration
node integration/oauth/run.mjs
```

The runner fetches a pinned [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator) commit and builds its PostgreSQL 18 image. It starts a local HTTPS issuer with a generated CA and JWKS, PostgreSQL with an OAuth HBA rule, and Hermit with its OIDC gate enabled. The published Neon driver completes a query with a signed access token. Bad signatures and audiences fail at Hermit; tokens with the wrong scope or requested role pass Hermit's gate and are rejected by PostgreSQL. Hermit maps pgx's wrapped OAuth SASL rejection to HTTP 401 without exposing validator details. The test network uses unencrypted Hermit-to-PostgreSQL traffic and is disposable; configure PostgreSQL TLS for a deployment. CI runs this suite separately from the PostgreSQL 17 SCRAM tests.

To test HTTPS and secure WebSockets through HAProxy, run `./integration/tls/run.sh` after `npm ci --prefix integration`. The script creates a temporary local CA and localhost server certificate, starts HAProxy on `127.0.0.1:8443`, checks its TLS health endpoint, and reruns the Neon driver tests over HTTPS/WSS. Node trusts only that test CA through `NODE_EXTRA_CA_CERTS`; hostname and certificate verification stay enabled. The script removes its certificate and HAProxy container afterward. CI runs this on Ubuntu.

### Measure memory under load

The benchmark uses the published Neon driver and samples Hermit's **Docker cgroup memory** while it runs. Start the Compose stack and install the Node dependencies, then run the eight preset scenarios:

```sh
docker compose up --build -d
npm ci --prefix integration
BENCH_SECONDS=10 ./integration/bench.sh
```

Each JSON line records transport, configured connections, maximum observed in-flight queries, target and achieved QPS, skipped requests, errors, p50/p95/p99 latency, sampled peak MiB, and the cgroup's recorded peak MiB. The preset covers 1 and 8 concurrent HTTP queries, 1, 8, and 32 WebSocket connections, a 200 ms query that holds HTTP connections open, and an 8 KiB HTTP result that exercises gzip. HTTP `connections` means maximum in-flight requests, since `/sql` opens a new PostgreSQL connection for each query; WebSocket connections stay open. The default eight-query HTTP limit returns 503 if more than eight are in flight; raise `HERMIT_MAX_HTTP_QUERIES` in Compose to measure higher HTTP concurrency. If `skipped` is nonzero, the offered QPS exceeded the chosen concurrency at that latency. Run one scenario with `node integration/bench.mjs --transport=ws --connections=16 --qps=80 --seconds=30`. On a Fedora RPM installation, add `--memory-source=systemd` to sample `hermit.service` and set `TEST_DATABASE_URL` for its PostgreSQL credentials. The runner requests samples every 500 ms, but Docker CLI calls can take longer; the cgroup peak catches shorter spikes when available. Restart Hermit before a one-off run to reset the peak. [Observed development results](benchmarks.md) are a starting point; run this on the target Fedora host for sizing.

For large results, `node integration/large-result.mjs --transport=ws --rows=100 --row-mib=1` requests 100 MiB and reports Hermit's cgroup peak and whether the query completed. Run it with `--transport=http` for the matching HTTP request, restarting Hermit between runs. In the [observed 100 MiB comparison](benchmarks.md#100-mib-result-comparison), WebSocket stayed near 16–17 MiB; with the current pgx release, streamed HTTP peaked at 20.0 MiB for 100 rows and rejected one 100 MiB row with 413 at a 13.4 MiB peak. Raising `HERMIT_HTTP_MAX_ROW_MIB` allows the single-row case at a higher memory cost.

The root Go package is the server, `web/` contains embedded browser modules, and `integration/` contains the Node suite. For source inspection only, the local Neon driver checkout is `/Users/georg/Developer/neon/serverless`; the Neon repository, including its proxy, is `/Users/georg/Developer/neon/neon`. Hermit's tests do not import either checkout.
