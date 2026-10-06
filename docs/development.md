# Development

Run commands in this guide from the repository root.

## Tagged releases

The [release workflow](../.github/workflows/release.yml) runs only when a `vX.Y.Z` tag is pushed; branch pushes build CI candidates but do not publish a release. Update `Version:` in [the RPM spec](../packaging/rpm/barnacle.spec) to `X.Y.Z`, merge and verify CI, then create and push an annotated tag on that commit:

```sh
git tag -a vX.Y.Z -m 'Barnacle vX.Y.Z'
git push origin vX.Y.Z
```

The workflow checks that the tag matches the spec version. It creates deterministic `barnacle-X.Y.Z.tar.gz` and `barnacle-X.Y.Z-vendor.tar.gz` archives, then builds static Linux amd64/arm64 binaries, both Debian packages, both Fedora RPMs from those exact source archives, and a multi-platform OCI archive with SBOM and provenance attestations. After all jobs succeed, it writes and verifies `SHA256SUMS` over the nine artifacts and publishes them together as GitHub Release assets. The source archive contains the Go service, RPM packaging files, and licenses; it excludes the optional console. The vendor archive has a top-level `vendor/` directory for RPM `Source1`. Source preparation can download Go modules; the RPM build itself uses the vendor archive offline.

Each asset has a versioned download URL, for example `https://github.com/proddata/barnacle/releases/download/vX.Y.Z/barnacle-X.Y.Z.tar.gz` and `https://github.com/proddata/barnacle/releases/download/vX.Y.Z/barnacle-X.Y.Z-vendor.tar.gz`. Package builders can pin the published SHA256 values from `SHA256SUMS`. To keep released asset bytes and tags from being replaced, [enable GitHub release immutability](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes) for the repository before the first tagged release. The workflow creates the release with all assets attached before it is published. A rerun must not replace an existing published release; issue a new version for corrections.

## Release binaries, container, and Debian package

The default [Dockerfile](../Dockerfile) builds Barnacle from source but has no Go toolchain in its runtime stage. For a release built from one tested set of static Linux binaries, run:

```sh
./packaging/release/build.sh  # linux/amd64 and linux/arm64; writes dist/SHA256SUMS
./packaging/deb/build.sh      # requires dpkg-deb; writes both .deb packages
docker build -f Dockerfile.release --platform linux/amd64 -t barnacle:release .
```

Use the matching architecture for a local container build. [Dockerfile.release](../Dockerfile.release) copies the prebuilt binary into a nonroot distroless image with CA certificates and license notices; it does not run Go or copy source files. The Debian package installs `/usr/bin/barnacle`, a systemd unit, and `/etc/default/barnacle`; edit that file and run `sudo systemctl enable --now barnacle`. `./packaging/release/image.sh` produces a multi-platform OCI archive with SBOM and build provenance attestations, without publishing it. CI builds and tests these release candidates. The existing [RPM](../packaging/rpm/barnacle.spec) continues to build independently from source and has its own Fedora smoke test.

## Fedora RPM

Build a local RPM on Fedora with Go 1.25.14 or newer:

```sh
sudo dnf install golang rpm-build systemd-rpm-macros
./packaging/rpm/build.sh
```

`build.sh` creates a source tarball and a separate `go mod vendor` tarball. The latter may require access to the Go module cache or network while preparing the sources; `rpmbuild` then builds and tests with `-mod=vendor` and `GOTOOLCHAIN=local`, so its build root needs no module or toolchain download. The browser console is not included in either the source archive or the RPM. All `THIRD-PARTY-*` notices and licenses remain in the installed package.

The spec requires the system `golang` RPM to provide Go 1.25.14 or newer. [Fedora 42's Go 1.24 package](https://packages.fedoraproject.org/pkgs/golang/golang-bin/fedora-42.html) does not meet this requirement. [Fedora 43's original Go 1.25.1 package](https://packages.fedoraproject.org/pkgs/golang/golang/fedora-43.html) also does not; use an updated Fedora 43 repository that provides 1.25.14 or newer, or use a [Fedora 44 build root](https://packages.fedoraproject.org/pkgs/golang/golang/fedora-44.html), which ships Go 1.26. For a pinned older build root, provide a newer `golang` RPM in its configured repository before resolving `BuildRequires`; installing an upstream Go tarball alone does not satisfy the RPM dependency. Check with `dnf repoquery --latest-limit=1 --qf '%{version}' golang` before building.

CI builds, installs, and smoke-tests x86_64 and aarch64 RPMs in Fedora 44 containers, then uploads both artifacts. It also runs the arm64 package under systemd with the opt-in fixed-user key-access variant. With Docker available, run that systemd test in a disposable Fedora container:

```sh
./integration/fedora-service/run.sh dist/barnacle-*.rpm
```

This local test uses a privileged container with its own cgroup namespace. It checks package and unit verification, startup, the effective memory limits, restart after a forced crash, sysconfig changes across a restart, the opt-in key-access variant's group-restricted key access, and clean stop. Use an RPM built for Docker's architecture. A real Fedora host is still needed to check integration with its ingress, PostgreSQL, certificates, and memory pressure. For installation and operation, see the [Fedora service guide](deployment.md#fedora-service).

## Test and inspect

With the Compose stack running, test Barnacle with the published Neon driver:

```sh
npm ci --prefix integration
TEST_DATABASE_URL='postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle' \
BARNACLE_BASE_URL='http://localhost:8080' \
npm test --prefix integration
```

The database URL's host is ignored in fixed routing mode; PostgreSQL stays inside the Compose network. The suite covers HTTP queries and types, bearer forwarding, batches, gzip, and Neon's WebSocket `Client`. The optional [debug console](../console/) uses the same driver in the browser.

For a native development setup with PostgreSQL listening on the host, run `go test -race ./...` and `go vet ./...`, then use `integration/run.sh` to build and start a temporary Barnacle process:

```sh
npm ci --prefix integration
BARNACLE_PG_ADDR=127.0.0.1:5432 \
BARNACLE_PG_SSLMODE=disable \
BARNACLE_PG_USER=barnacle \
BARNACLE_PG_DATABASE=barnacle \
TEST_DATABASE_URL='postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle' \
./integration/run.sh
```

The integration suite installs `@neondatabase/serverless` and `ws` from npm using the committed lockfile. [CI](../.github/workflows/ci.yml) runs Go tests with the race detector and Go/npm vulnerability scans on Ubuntu, scans the built container for high and critical vulnerabilities, checks browser CORS and WebSocket origins in headless Chrome, and builds, installs, and smoke-tests the Fedora RPM in an independent job. It runs the full driver integration suite against PostgreSQL 14–18 without repeating package builds or vulnerability scans for each version. PostgreSQL 14 leaves upstream support in November 2026 and should then leave the required matrix. A separate [next-major workflow](../.github/workflows/postgres-next.yml) runs the same suite nightly against a pinned PostgreSQL 19 beta; it does not block pull requests until 19 is generally available. The direct integration runner also sends SIGTERM to a second Barnacle process with HTTP and WebSocket queries active; it checks the HTTP response, WebSocket close code, PostgreSQL cleanup, and process exit. A separate process-level check sends an incomplete slow HTTP body, holds idle keep-alive connections, and verifies that an upgraded WebSocket stays usable. Run the browser check locally with `BARNACLE_BROWSER_CORS=1 ./integration/run.sh` when Chrome or Chromium is installed. The WebSocket frame and PostgreSQL key parsers have Go fuzz targets in `internal/pgws/`.

To verify Compose's upstream TLS directly, run `node integration/pg-tls.mjs` while the stack is up. It asks PostgreSQL's `pg_stat_ssl` view whether the current HTTP and WebSocket sessions use TLS. The Go suite also checks that WebSocket connections reject plaintext servers and certificates with the wrong hostname.

The suite includes 55 built-in PostgreSQL type and parameter cases in both raw-text and plain-JSON modes. To compare the same cases with a deployed Neon proxy, put a **disposable** connection URL in the gitignored `integration/.env` as `NEON_COMPARE_DATABASE_URL=...`, then run `node integration/type-compare.mjs` while the Compose stack is up. Set `NEON_COMPARE_ENDPOINT=...` there only for a custom SQL endpoint. The comparator checks rows and result metadata and never prints the connection URL. In the 2026-10-02 run, 107 of 110 raw/plain variants matched. Neon's plain-JSON path returned HTTP 500 for empty integer arrays and split `box[]` elements at commas; Barnacle preserves those PostgreSQL values. The comparison reports these three observed differences separately and fails on any other difference.

### PostgreSQL OAuth end to end

Run the disposable PostgreSQL 18 OAuth suite with Docker, Git, OpenSSL, and Node 24:

```sh
npm ci --prefix integration
node integration/oauth/run.mjs
```

The runner fetches a pinned [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator) commit and builds its PostgreSQL 18 image. It starts a local HTTPS issuer with a generated CA and JWKS, PostgreSQL with an OAuth HBA rule, and Barnacle with its OIDC gate enabled. The published Neon driver completes a query with a signed access token. Bad signatures and audiences fail at Barnacle; tokens with the wrong scope or requested role pass Barnacle's gate and are rejected by PostgreSQL. A separate SCRAM role has the token itself as its password; the test confirms gated HTTP still refuses password fallback. Barnacle maps pgx's wrapped OAuth SASL rejection to HTTP 401 without exposing validator details. The test network uses unencrypted Barnacle-to-PostgreSQL traffic and is disposable; configure PostgreSQL TLS for a deployment. CI runs this suite separately from the PostgreSQL 17 SCRAM tests.

To test HTTPS and secure WebSockets through HAProxy, run `./integration/tls/run.sh` after `npm ci --prefix integration`. The script creates a temporary local CA and localhost server certificate, starts HAProxy on `127.0.0.1:8443`, checks its TLS health endpoint, verifies that canary auth and connection-string headers do not appear in the example deployment's logs, and reruns the Neon driver tests over HTTPS/WSS. Node trusts only that test CA through `NODE_EXTRA_CA_CERTS`; hostname and certificate verification stay enabled. The script removes its certificate and HAProxy container afterward. CI runs this on Ubuntu.

### Measure memory under load

The benchmark uses the published Neon driver and samples Barnacle's **Docker cgroup memory** while it runs. Start the Compose stack and install the Node dependencies, then run the eight preset scenarios:

```sh
docker compose up --build -d
npm ci --prefix integration
BENCH_SECONDS=10 ./integration/bench.sh
```

Each JSON line records transport, configured connections, maximum observed in-flight queries, target and achieved QPS, skipped requests, errors, p50/p95/p99 latency, sampled peak MiB, and the cgroup's recorded peak MiB. The preset covers 1 and 8 concurrent HTTP queries, 1, 8, and 32 WebSocket connections, a 200 ms query that holds HTTP connections open, and an 8 KiB HTTP result that exercises gzip. HTTP `connections` means maximum in-flight requests, since `/sql` opens a new PostgreSQL connection for each query; WebSocket connections stay open. The default eight-query HTTP limit returns 503 if more than eight are in flight; raise `BARNACLE_MAX_HTTP_QUERIES` in Compose to measure higher HTTP concurrency. If `skipped` is nonzero, the offered QPS exceeded the chosen concurrency at that latency. Run one scenario with `node integration/bench.mjs --transport=ws --connections=16 --qps=80 --seconds=30`. On a Fedora RPM installation, add `--memory-source=systemd` to sample `barnacle.service` and set `TEST_DATABASE_URL` for its PostgreSQL credentials. The runner requests samples every 500 ms, but Docker CLI calls can take longer; the cgroup peak catches shorter spikes when available. Restart Barnacle before a one-off run to reset the peak. [Observed development results](../benchmarks.md), including the [HTTP connection reuse decision](../benchmarks.md#http-connection-reuse-decision), are a starting point; run this on the target Fedora host for sizing.

For large results, `node integration/large-result.mjs --transport=ws --rows=100 --row-mib=1` requests 100 MiB and reports Barnacle's cgroup peak and whether the query completed. Run it with `--transport=http` for the matching HTTP request, restarting Barnacle between runs. In the [observed 100 MiB comparison](../benchmarks.md#100-mib-result-comparison), WebSocket stayed near 16–17 MiB; with the current pgx release, streamed HTTP peaked at 20.0 MiB for 100 rows and rejected one 100 MiB row with 413 at a 13.4 MiB peak. Raising `BARNACLE_HTTP_MAX_ROW_MIB` allows the single-row case at a higher memory cost.

The root Go package wires the server together. `internal/sqlhttp/` implements SQL over HTTP, `internal/pgws/` implements the PostgreSQL WebSocket tunnel, and `internal/gateway/` holds their shared authentication, routing, TLS, limits, and metrics. `console/` is a separate optional browser app, and `integration/` contains the Node suite. Both use the published Neon driver.
