# Third-party license inventory

Barnacle itself is licensed under Apache-2.0; see [LICENSE](LICENSE). The
components below retain their own licenses. This inventory describes the
versions in `go.mod` and `integration/package-lock.json` at the time of review.
Recheck it when those files or the container images change.

## Included in the Barnacle executable

The Go executable is built with `CGO_ENABLED=0`. Its compiled dependency graph
contains these modules and the Go standard library. Copies of the upstream
license texts are in `THIRD-PARTY-LICENSES/` and are packaged with the Docker
image and RPM.

| Component | Version | License | License text |
| --- | --- | --- | --- |
| Go standard library | Go 1.25.14 | BSD-3-Clause | `GO-STANDARD-LIBRARY` |
| `github.com/jackc/pgx/v5` | 5.11.0 | MIT | `PGX` |
| `github.com/jackc/pgpassfile` | 1.0.0 | MIT | `PGPASSFILE` |
| `github.com/jackc/pgservicefile` | 2024-06-06 commit `5a60cdf6a761` | MIT | `PGSERVICEFILE` |
| `golang.org/x/text` | 0.39.0 | BSD-3-Clause | `X-TEXT` |

`pgx`'s module graph also names `puddle/v2`, `golang.org/x/sync`, and test
helpers. They are not in the compiled Barnacle executable according to
`go list -deps`; their license terms remain relevant if the build changes.
`puddle/v2` is MIT and `golang.org/x/sync` is BSD-3-Clause.

## Test-only packages

The npm packages in `integration/package-lock.json` are installed for tests
and are not copied into the Barnacle Docker runtime image or RPM:

| Component | Version | License |
| --- | --- | --- |
| `@neondatabase/serverless` | 1.2.0 | MIT |
| `ws` | 8.22.0 | MIT |

The optional OAuth integration test fetches a pinned
[`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator) source
revision and builds a disposable PostgreSQL test image. That project uses the
PostgreSQL License. Its own third-party notice records statically included
libjwt under MPL-2.0 and Jansson under MIT. These are not part of Barnacle's
normal executable, Docker image, or RPM. If the validator image is distributed,
its license texts and covered-source obligations must be handled as a separate
product artifact.

## Container images and external services

The runtime image starts from `alpine:3.21`; the build stage uses
`golang:1.25.14-alpine`. Development and integration tests also use PostgreSQL,
HAProxy, Node, and Fedora images. Those are separately distributed software,
not relicensed by Barnacle's Apache-2.0 license. PostgreSQL itself uses the
[PostgreSQL License](https://www.postgresql.org/about/licence/).

The locally built `barnacle:license-audit` image inspected for this review has
image ID `sha256:7163905b5eec7f42c3b79434e58c977373184c011f7e3b4e081cf1f8dc2072f5`
and contains Alpine 3.21.8. Its installed package database reports:

| License identifiers | Installed packages |
| --- | --- |
| GPL-2.0-only | `alpine-baselayout`, `alpine-baselayout-data`, `apk-tools`, `busybox`, `busybox-binsh`, `scanelf`, `ssl_client` |
| GPL-2.0-or-later, MIT, BSD-2-Clause | `musl-utils` |
| MPL-2.0, MIT | `ca-certificates-bundle` |
| Apache-2.0 | `libcrypto3`, `libssl3` |
| MIT | `alpine-keys`, `alpine-release`, `musl` |
| Zlib | `zlib` |

These are Alpine package metadata values for that local image. In particular, the full Docker
image contains GPL and MPL packages even though Barnacle's executable does not.
Before distributing a release image, record its digest, generate an image SBOM,
and review the exact installed packages. Provide required notices and source
access for redistributed GPL/MPL components through the image publisher's
release process; Barnacle's Apache-2.0 license does not replace their terms.
