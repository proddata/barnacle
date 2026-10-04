# Hermit 🦀

Hermit is an HTTP and WebSocket gateway for PostgreSQL. It brings serverless and agent-driven applications to self-hosted or managed PostgreSQL using a subset of the Neon serverless driver interface. Hermit connects to a fixed database or pooler by default; optional routing is limited to addresses you allow.

```text
@neondatabase/serverless       Hermit                    PostgreSQL
neon() / sql.query()  ─ HTTP ──► POST /sql ── pgx ────────► :5432
Client / Pool         ── WS ───► GET /v2 ── wire tunnel ──► :5432
```

Hermit is [Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD-PARTY-NOTICES.md). It is a [compatible subset](todo.md) of Neon's interface, not Neon's full proxy or platform routing.

## Quick start

```sh
docker compose up --build
```

Open [localhost:8080](http://localhost:8080) and use the HTTP or WebSocket buttons with:

```text
postgres://hermit:hermit_dev_password@localhost:5432/hermit
```

The Compose stack is for local development: it binds Hermit to loopback, keeps PostgreSQL off the host network, and verifies a generated PostgreSQL certificate. The example password is not for production.

Try SQL over HTTP:

```sh
curl -sS http://localhost:8080/sql \
  -H 'Neon-Connection-String: postgres://hermit:hermit_dev_password@localhost:5432/hermit' \
  -d '{"query":"select $1::int as answer","params":[42]}'
```

## Neon driver

```js
import { neon, neonConfig, Client } from '@neondatabase/serverless';
import WebSocket from 'ws';

const url = 'postgres://hermit:hermit_dev_password@localhost:5432/hermit';
neonConfig.fetchEndpoint = 'http://localhost:8080/sql';
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = false; // local demo only
neonConfig.wsProxy = () => 'localhost:8080/v2';
neonConfig.pipelineConnect = false; // Compose uses SCRAM

const sql = neon(url); // HTTP
console.log(await sql`select 42`);

const client = new Client(url); // WebSocket
await client.connect();
try { console.log((await client.query('select 42')).rows); }
finally { await client.end(); }
```

Use HTTPS and WSS at the ingress in production. See [authentication](docs/authentication.md) for password, bearer-token, and WebSocket behavior.

## Deployment choices

| Choice | Upstream configuration |
| --- | --- |
| Direct PostgreSQL | `HERMIT_PG_ADDR=postgres:5432` |
| PgBouncer or another pooler | `HERMIT_PG_ADDR=pgbouncer:6432` |
| Both routes | `HERMIT_PG_ALLOWED_ADDRS=postgres:5432,pgbouncer:6432` |

Addresses may instead be loopback ports on one host, such as `127.0.0.1:5432` and `127.0.0.1:6432`. With no allowlist, clients cannot choose a different upstream. Read the [deployment guide](docs/deployment.md) for routing, network isolation, TLS, configuration, systemd packages, and production checks.

## Interfaces and operations

| Endpoint | Purpose |
| --- | --- |
| `POST /sql` | One query or a transactional batch over HTTP |
| `GET /v2` (`/v1` also works) | PostgreSQL wire session over WebSocket |
| `GET /healthz`, `GET /readyz` | Liveness and optional PostgreSQL transport readiness |
| `GET /metrics` | Optional Prometheus metrics (`HERMIT_METRICS=true`) |

See the [API and protocol reference](docs/api.md) for request and response details, limits, and cancellation. An OpenTelemetry Collector can scrape `/metrics`; Hermit does not emit OTLP or traces. Keep metrics private.

## Build and contribute

The [development guide](docs/development.md) covers tests, integration suites, benchmarks, release binaries, the `.deb`, and the binary-based container. The default Dockerfile builds from source but its runtime image contains no Go toolchain. CI also builds and tests a Fedora RPM.
