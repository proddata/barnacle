# Development

Run commands in this guide from the repository root.

## Release binaries, container, and Debian package

The default [Dockerfile](../Dockerfile) builds Hermit from source but has no Go toolchain in its runtime stage. For a release built from one tested set of static Linux binaries, run:

```sh
./packaging/release/build.sh  # linux/amd64 and linux/arm64; writes dist/SHA256SUMS
./packaging/deb/build.sh      # requires dpkg-deb; writes both .deb packages
docker build -f Dockerfile.release --platform linux/amd64 -t hermit:release .
```

Use the matching architecture for a local container build. [Dockerfile.release](../Dockerfile.release) copies the prebuilt binary into a nonroot distroless image with CA certificates and license notices; it does not run Go or copy source files. The Debian package installs `/usr/bin/hermit`, a systemd unit, and `/etc/default/hermit`; edit that file and run `sudo systemctl enable --now hermit`. `./packaging/release/image.sh` produces a multi-platform OCI archive with SBOM and build provenance attestations, without publishing it. CI builds and tests these release candidates. The existing [RPM](../packaging/rpm/hermit.spec) continues to build independently from source and has its own Fedora smoke test.

## Fedora RPM

Build a local RPM on Fedora with Go 1.25.14 or newer:

```sh
sudo dnf install golang rpm-build systemd-rpm-macros
./packaging/rpm/build.sh
```

The RPM build downloads Go modules before making its source archive, then builds and tests offline inside `rpmbuild`. CI builds, installs, and smoke-tests the package in a Fedora container, then uploads it. With Docker available, test the installed unit under systemd in a disposable Fedora container:

```sh
./integration/fedora-service/run.sh dist/hermit-*.rpm
```

This local test uses a privileged container with its own cgroup namespace. It checks package and unit verification, startup, the effective memory limits, restart after a forced crash, sysconfig changes across a restart, and clean stop. Use an RPM built for Docker's architecture. A real Fedora host is still needed to check integration with its ingress, PostgreSQL, certificates, and memory pressure. For installation and operation, see the [Fedora service guide](deployment.md#fedora-service).

## Test and inspect

With the Compose stack running, test the same endpoint the console uses with the published Neon driver:

```sh
npm ci --prefix integration
TEST_DATABASE_URL='postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
HERMIT_BASE_URL='http://localhost:8080' \
npm test --prefix integration
```

The database URL's host is ignored by Hermit; PostgreSQL stays inside the Compose network. The suite covers HTTP queries and types, bearer forwarding, batches, gzip, Neon's WebSocket `Client`, and the console's SCRAM wire client.

For a native development setup with PostgreSQL listening on the host, run `go test -race ./...` and `go vet ./...`, then use `integration/run.sh` to build and start a temporary Hermit process:

```sh
npm ci --prefix integration
HERMIT_PG_ADDR=127.0.0.1:5432 \
HERMIT_PG_SSLMODE=disable \
HERMIT_PG_USER=hermit \
HERMIT_PG_DATABASE=hermit \
TEST_DATABASE_URL='postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
./integration/run.sh
```

The integration suite installs `@neondatabase/serverless` and `ws` from npm using the committed lockfile. [CI](../.github/workflows/ci.yml) runs Go tests with the race detector and Go/npm vulnerability scans on Ubuntu, scans the built container for high and critical vulnerabilities, checks browser CORS and WebSocket origins in headless Chrome, and builds, installs, and smoke-tests the Fedora RPM in an independent job. The direct integration runner also sends SIGTERM to a second Hermit process with HTTP and WebSocket queries active; it checks the HTTP response, WebSocket close code, PostgreSQL cleanup, and process exit. A separate process-level check sends an incomplete slow HTTP body, holds idle keep-alive connections, and verifies that an upgraded WebSocket stays usable. Run the browser check locally with `HERMIT_BROWSER_CORS=1 ./integration/run.sh` when Chrome or Chromium is installed. The WebSocket frame and PostgreSQL key parsers have Go fuzz targets in `internal/pgws/`.

To verify Compose's upstream TLS directly, run `node integration/pg-tls.mjs` while the stack is up. It asks PostgreSQL's `pg_stat_ssl` view whether the current HTTP and WebSocket sessions use TLS. The Go suite also checks that WebSocket connections reject plaintext servers and certificates with the wrong hostname.

The suite includes 55 built-in PostgreSQL type and parameter cases in both raw-text and plain-JSON modes. To compare the same cases with a deployed Neon proxy, put a **disposable** connection URL in the gitignored `integration/.env` as `NEON_COMPARE_DATABASE_URL=...`, then run `node integration/type-compare.mjs` while the Compose stack is up. Set `NEON_COMPARE_ENDPOINT=...` there only for a custom SQL endpoint. The comparator checks rows and result metadata and never prints the connection URL. In the 2026-10-02 run, 107 of 110 raw/plain variants matched. Neon's plain-JSON path returned HTTP 500 for empty integer arrays and split `box[]` elements at commas; Hermit preserves those PostgreSQL values. The comparison reports these three observed differences separately and fails on any other difference.

### PostgreSQL OAuth end to end

Run the disposable PostgreSQL 18 OAuth suite with Docker, Git, OpenSSL, and Node 24:

```sh
npm ci --prefix integration
node integration/oauth/run.mjs
```

The runner fetches a pinned [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator) commit and builds its PostgreSQL 18 image. It starts a local HTTPS issuer with a generated CA and JWKS, PostgreSQL with an OAuth HBA rule, and Hermit with its OIDC gate enabled. The published Neon driver completes a query with a signed access token. Bad signatures and audiences fail at Hermit; tokens with the wrong scope or requested role pass Hermit's gate and are rejected by PostgreSQL. A separate SCRAM role has the token itself as its password; the test confirms gated HTTP still refuses password fallback. Hermit maps pgx's wrapped OAuth SASL rejection to HTTP 401 without exposing validator details. The test network uses unencrypted Hermit-to-PostgreSQL traffic and is disposable; configure PostgreSQL TLS for a deployment. CI runs this suite separately from the PostgreSQL 17 SCRAM tests.

To test HTTPS and secure WebSockets through HAProxy, run `./integration/tls/run.sh` after `npm ci --prefix integration`. The script creates a temporary local CA and localhost server certificate, starts HAProxy on `127.0.0.1:8443`, checks its TLS health endpoint, verifies that canary auth and connection-string headers do not appear in the example deployment's logs, and reruns the Neon driver tests over HTTPS/WSS. Node trusts only that test CA through `NODE_EXTRA_CA_CERTS`; hostname and certificate verification stay enabled. The script removes its certificate and HAProxy container afterward. CI runs this on Ubuntu.

### Measure memory under load

The benchmark uses the published Neon driver and samples Hermit's **Docker cgroup memory** while it runs. Start the Compose stack and install the Node dependencies, then run the eight preset scenarios:

```sh
docker compose up --build -d
npm ci --prefix integration
BENCH_SECONDS=10 ./integration/bench.sh
```

Each JSON line records transport, configured connections, maximum observed in-flight queries, target and achieved QPS, skipped requests, errors, p50/p95/p99 latency, sampled peak MiB, and the cgroup's recorded peak MiB. The preset covers 1 and 8 concurrent HTTP queries, 1, 8, and 32 WebSocket connections, a 200 ms query that holds HTTP connections open, and an 8 KiB HTTP result that exercises gzip. HTTP `connections` means maximum in-flight requests, since `/sql` opens a new PostgreSQL connection for each query; WebSocket connections stay open. The default eight-query HTTP limit returns 503 if more than eight are in flight; raise `HERMIT_MAX_HTTP_QUERIES` in Compose to measure higher HTTP concurrency. If `skipped` is nonzero, the offered QPS exceeded the chosen concurrency at that latency. Run one scenario with `node integration/bench.mjs --transport=ws --connections=16 --qps=80 --seconds=30`. On a Fedora RPM installation, add `--memory-source=systemd` to sample `hermit.service` and set `TEST_DATABASE_URL` for its PostgreSQL credentials. The runner requests samples every 500 ms, but Docker CLI calls can take longer; the cgroup peak catches shorter spikes when available. Restart Hermit before a one-off run to reset the peak. [Observed development results](../benchmarks.md), including the [HTTP connection reuse decision](../benchmarks.md#http-connection-reuse-decision), are a starting point; run this on the target Fedora host for sizing.

For large results, `node integration/large-result.mjs --transport=ws --rows=100 --row-mib=1` requests 100 MiB and reports Hermit's cgroup peak and whether the query completed. Run it with `--transport=http` for the matching HTTP request, restarting Hermit between runs. In the [observed 100 MiB comparison](../benchmarks.md#100-mib-result-comparison), WebSocket stayed near 16–17 MiB; with the current pgx release, streamed HTTP peaked at 20.0 MiB for 100 rows and rejected one 100 MiB row with 413 at a 13.4 MiB peak. Raising `HERMIT_HTTP_MAX_ROW_MIB` allows the single-row case at a higher memory cost.

The root Go package wires the server together. `internal/sqlhttp/` implements SQL over HTTP, `internal/pgws/` implements the PostgreSQL WebSocket tunnel, and `internal/gateway/` holds their shared authentication, routing, TLS, limits, and metrics. `web/` contains embedded browser modules, and `integration/` contains the Node suite. Hermit's integration tests use the published Neon driver.
