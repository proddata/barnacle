# Private upstream topology tests

Run from the repository root after `npm ci --prefix integration`:

```sh
./integration/topology/run.sh haproxy-postgres
./integration/topology/run.sh pgbouncer-postgres
./integration/topology/run.sh haproxy-pgbouncer
```

Each case starts a separate Barnacle test container, waits for a successful SQL request, then runs the published Neon driver's integration suite. The public Barnacle endpoint is plain loopback HTTP for this test. Barnacle's **upstream** connection uses TLS and verifies the `postgres` name via `BARNACLE_PG_TLS_SERVER_NAME` and the generated Compose CA. HAProxy uses TCP mode and does not terminate TLS. The PgBouncer fixture uses session pooling and TLS on both its client and PostgreSQL connections. Session pooling is required by the suite's WebSocket `SET`, cursor, and prepared-statement tests.

The pooler cases set `BARNACLE_TEST_PGBOUNCER=1`. PgBouncer reports its own `08P01` login errors and can retain an idle PostgreSQL backend after a client disconnect, so the suite uses the matching authentication expectation and skips the direct-backend-release assertion. The rest of the HTTP, WebSocket, cancellation, and type tests run through each topology.
