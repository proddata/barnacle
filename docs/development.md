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

Publication also requires the full [CI workflow](../.github/workflows/ci.yml),
called directly by the release workflow on the tagged commit. Release builds
and validation run in parallel; publishing waits for both to succeed. This
reuses the ordinary integration, package, and regression checks for each release.

## Release binaries, container, and Debian package

The default [Dockerfile](../Dockerfile) builds Barnacle from source but has no Go toolchain in its runtime stage. For a release built from one tested set of static Linux binaries, run:

```sh
./packaging/release/build.sh  # linux/amd64 and linux/arm64; writes dist/SHA256SUMS
./packaging/deb/build.sh      # requires dpkg-deb; writes both .deb packages
docker build -f Dockerfile.release --platform linux/amd64 -t barnacle:release .
```

Use the matching architecture for a local container build. [Dockerfile.release](../Dockerfile.release) copies the prebuilt binary into a nonroot distroless image with CA certificates and license notices; it does not run Go or copy source files. The Debian package installs `/usr/bin/barnacle`, a systemd unit, and `/etc/default/barnacle`; edit that file and run `sudo systemctl enable --now barnacle`. `./packaging/release/image.sh` produces a multi-platform OCI archive with SBOM and build provenance attestations, without publishing it. CI builds and tests these release candidates. The existing [RPM](../packaging/rpm/barnacle.spec) continues to build independently from source and has its own Fedora smoke test.

## Fedora RPM

Build a local RPM on Fedora with Go 1.26.4 or newer:

```sh
sudo dnf install golang rpm-build systemd-rpm-macros
./packaging/rpm/build.sh
```

`build.sh` creates a source tarball and a separate `go mod vendor` tarball. The latter may require access to the Go module cache or network while preparing the sources; `rpmbuild` then builds and tests with `-mod=vendor` and `GOTOOLCHAIN=local`, so its build root needs no module or toolchain download. The browser console is not included in either the source archive or the RPM. All `THIRD-PARTY-*` notices and licenses remain in the installed package.

The spec requires the system `golang` RPM to provide Go 1.26.4 or newer, which updated Fedora 42, 43 and 44 build roots ship. For a pinned older build root, provide a newer `golang` RPM in its configured repository before resolving `BuildRequires`; installing an upstream Go tarball alone does not satisfy the RPM dependency. Check with `dnf repoquery --latest-limit=1 --qf '%{version}' golang` before building. Release binaries and container images are built with the newer toolchain pinned in `go.mod`, not the Fedora one.

CI builds, installs, and smoke-tests x86_64 and aarch64 RPMs in Fedora 42, 43 and 44 containers, then uploads the artifacts. It also runs the arm64 package under systemd with the opt-in fixed-user key-access variant. With Docker available, run that systemd test in a disposable Fedora container:

```sh
./integration/fedora-service/run.sh dist/barnacle-*.rpm
```

This local test uses a privileged container with its own cgroup namespace. It checks package and unit verification, startup, the effective memory limits, restart after a forced crash, sysconfig changes across a restart, the opt-in key-access variant's group-restricted key access, and clean stop. Use an RPM built for Docker's architecture. A real Fedora host is still needed to check integration with its ingress, PostgreSQL, certificates, and memory pressure. For installation and operation, see the [Fedora service guide](operations.md#fedora-service).

### Packaging for a distribution or vendor

A packager who builds Barnacle in their own pipeline should start from [the RPM spec](../packaging/rpm/barnacle.spec) and the two published source archives, not from a copy of an older release. The fields they normally set are `Epoch`, the `Release` suffix, `Vendor`, and `URL` (pin it to the release tag); everything else, including the Go requirement, the license line and the hardened PIE build, should stay as released. When you move to a newer Barnacle version, take that release's spec again: the license line and `BuildRequires: golang` change between releases (for example when a dependency is added), and a stale copy then misdescribes the package.

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
TEST_DATABASE_URL='postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle' \
./integration/run.sh
```

The integration suite installs `@neondatabase/serverless` and `ws` from npm using the committed lockfile. [CI](../.github/workflows/ci.yml) runs Go tests with the race detector, Go source scans using the build toolchain, scans of both release binaries, npm and container vulnerability scans, browser origin checks, the driver suite against supported PostgreSQL versions, and Fedora RPM installation checks. The [next-major workflow](../.github/workflows/postgres-next.yml) exercises an upcoming PostgreSQL version separately. The integration runner also tests shutdown with active HTTP and WebSocket queries, slow request bodies, idle connections, and upgraded WebSockets. Run the browser check locally with `BARNACLE_BROWSER_CORS=1 ./integration/run.sh` when Chrome or Chromium is installed. The PostgreSQL key parser has a Go fuzz target in `internal/pgws/`; WebSocket framing is handled by [`coder/websocket`](https://github.com/coder/websocket).

To verify Compose's upstream TLS directly, run `node integration/pg-tls.mjs` while the stack is up. It asks PostgreSQL's `pg_stat_ssl` view whether the current HTTP and WebSocket sessions use TLS. The Go suite also checks that WebSocket connections reject plaintext servers and certificates with the wrong hostname.

The suite includes 55 built-in PostgreSQL type and parameter cases in both raw-text and plain-JSON modes. To compare the same cases with a deployed Neon proxy, put a disposable connection URL in the gitignored `integration/.env` as `NEON_COMPARE_DATABASE_URL=...`, then run `node integration/type-compare.mjs` while the Compose stack is up. Set `NEON_COMPARE_ENDPOINT=...` there only for a custom SQL endpoint. The comparator checks rows and result metadata without printing the connection URL. It reports known differences in Neon's plain-JSON array conversion separately and fails on other differences.

### PostgreSQL OAuth end to end

Run the disposable PostgreSQL 18 OAuth suite with Docker, Git, OpenSSL, and Node 24:

```sh
npm ci --prefix integration
node integration/oauth/run.mjs
```

The runner fetches a pinned [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator) commit and builds its PostgreSQL 18 image. It starts a local HTTPS issuer, PostgreSQL with an OAuth HBA rule, and Barnacle forwarding bearer tokens. The published Neon driver completes an HTTP query with a signed access token; a small PostgreSQL wire client tests OAuth and a query over WebSocket because the pinned Neon WebSocket client supports SCRAM but not OAuth SASL. The suite checks that PostgreSQL rejects invalid signatures, audiences, scopes, and roles, that Bearer never falls back to password authentication, and that explicit Basic/SCRAM authentication works while a `trust` rule cannot bypass it. The regular Neon integration suite covers HTTP and WebSocket password authentication. The test network is disposable and uses plaintext between Barnacle and PostgreSQL; use verified PostgreSQL TLS for deployments. CI runs this suite separately from the SCRAM tests.

To test HTTPS and secure WebSockets through HAProxy, run `./integration/tls/run.sh` after `npm ci --prefix integration`. The script creates a temporary local CA and localhost server certificate, starts HAProxy on `127.0.0.1:8443`, checks its TLS health endpoint, verifies that canary auth and connection-string headers do not appear in the example deployment's logs, and reruns the Neon driver tests over HTTPS/WSS. Node trusts only that test CA through `NODE_EXTRA_CA_CERTS`; hostname and certificate verification stay enabled. The script removes its certificate and HAProxy container afterward. CI runs this on Ubuntu.

### Measure memory under load

The benchmark uses the published Neon driver and samples Barnacle's **Docker cgroup memory** while it runs. Start the Compose stack and install the Node dependencies, then run the eight preset scenarios:

```sh
docker compose up --build -d
npm ci --prefix integration
BENCH_SECONDS=10 ./integration/bench.sh
```

Each JSON line records throughput, latency, errors, skipped requests, concurrency, and memory. HTTP `connections` means maximum in-flight requests; WebSocket connections stay open. The default HTTP request limit returns 503 if more than eight are in flight. For a single scenario, run `node integration/bench.mjs --transport=ws --connections=16 --qps=80 --seconds=30`. On a Fedora RPM installation, set `TEST_DATABASE_URL` and add `--memory-source=systemd` to sample `barnacle.service`. Restart Barnacle before a one-off run to reset the cgroup peak. Use [local measurements](../benchmarks.md) as a reference and measure representative traffic on the deployment host before sizing limits.

For large results, run `node integration/large-result.mjs --transport=ws --rows=100 --row-mib=1` or use `--transport=http` for streaming HTTP. The script reports Barnacle's cgroup peak and whether the query completed. Restart Barnacle between runs. See the [result-size measurements](../benchmarks.md#100-mib-result-comparison) and the [HTTP limits](decisions/sql-http-limits.md) when interpreting the results.

### Memory scenarios

`integration/memory-scenarios.mjs` runs realistic workloads against Barnacle under a real cgroup limit, to show how the [memory budget](decisions/sql-http-limits.md#memory-budget) scales. For each scenario and tier it starts a fresh container with `docker run --memory`, drives it with closed-loop clients, and reports the budget Barnacle derived, peak cgroup memory, latency, throughput, the status mix, and whether the kernel OOM-killed it. The tiers are 256 MiB, 512 MiB, and 512 MiB with 16 concurrent HTTP slots instead of 8.

```sh
npm ci --prefix integration
node integration/memory-scenarios.mjs                                   # every scenario and tier
node integration/memory-scenarios.mjs --tiers 256 --scenario streaming-export --seconds 10
node integration/memory-scenarios.mjs --check --json results.json      # exit 1 if an expectation fails
```

The scenarios are API point lookups, dashboard aggregate batches, 1,000-row list pages, jsonb documents, array columns, a mixed production load with a buffered export, bulk inserts, maximum-size request bodies, streamed exports beside API traffic, streamed array rows, 32 WebSocket sessions beside HTTP traffic, and two abusive loads that must be refused without crashing. Realistic scenarios must succeed; abusive ones may return `413` or `503` but never kill the gateway. `--check` also fails if peak memory exceeds 85% of the limit. It seeds PostgreSQL in Docker, so it needs Docker and a few minutes; CI does not run it. Results depend on the host, so compare tiers within one run.

The root Go package wires the server together. `internal/sqlhttp/` implements SQL over HTTP, `internal/pgws/` implements the PostgreSQL WebSocket tunnel, and `internal/gateway/` holds their shared authentication, routing, TLS, limits, and metrics. `console/` is a separate optional browser app, and `integration/` contains the Node suite. Both use the published Neon driver.
