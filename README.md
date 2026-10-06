# Barnacle

Barnacle is an HTTP and WebSocket gateway for PostgreSQL. It brings serverless and agent-driven applications to self-hosted or managed PostgreSQL using a subset of the Neon serverless driver interface. Barnacle connects to a fixed database or pooler by default; optional routing is limited to addresses you allow.

```mermaid
flowchart LR
    subgraph App[Application]
        HTTPClient["neon() / sql.query()"]
        WSClient["Client / Pool"]
    end

    subgraph Gateway[Barnacle]
        HTTP["POST /sql<br/>HTTP query or transaction batch"]
        PGX["pgx<br/>PostgreSQL wire client"]
        WS["GET /v2<br/>WebSocket session"]
        Relay["PostgreSQL wire relay"]
        HTTP --> PGX
        WS --> Relay
    end

    Upstream["PostgreSQL or PgBouncer"]
    HTTPClient -->|HTTP JSON| HTTP
    WSClient -->|PostgreSQL wire over WebSocket| WS
    PGX -->|PostgreSQL wire protocol| Upstream
    Relay -->|PostgreSQL wire protocol| Upstream
```

## Why Barnacle?

A barnacle holds fast while the current moves around it. Barnacle anchors to the PostgreSQL setup you already run and meets serverless and browser clients over HTTP or WebSocket. Your data and SQL stay in PostgreSQL; Barnacle carries the traffic between those transports and the PostgreSQL wire protocol.

Barnacle is [Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD-PARTY-NOTICES.md). It is a [compatible subset](todo.md) of Neon's interface, not Neon's full proxy or platform routing.

## Quick start

```sh
docker compose up --build
```

To start the separate debug console as well:

```sh
docker compose --profile console up --build
```

Open [localhost:8081](http://localhost:8081) and use host `postgres:5432`, database `barnacle`, user `barnacle`, and password `barnacle_dev_password`. This console is for development and debugging; do not deploy it in production. It has a Monaco SQL editor, a database explorer, result grids, and a request debug panel. It offers HTTP password, HTTP bearer, and WebSocket password queries through the Neon serverless driver. Barnacle itself listens at [localhost:8080](http://localhost:8080).

The equivalent PostgreSQL connection string is:

```text
postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle
```

The Compose stack is for local development: it binds Barnacle to loopback, keeps PostgreSQL off the host network, and verifies a generated PostgreSQL certificate. The example password is not for production.

If you used the earlier Hermit Compose stack, its existing PostgreSQL volume still has the old sample role and database. Stop the earlier containers before reusing ports 8080 and 8081. This Compose file uses the `barnacle` project name, so `docker compose up --build` creates fresh named development volumes without deleting the old ones. Update any local `.env` settings from `HERMIT_*` to `BARNACLE_*`.

The local Compose stack allows the console's host field to select any PostgreSQL `host:port`, including external services. Barnacle still verifies upstream TLS using the host name and the system trust store plus the generated local CA. Use the provider's PostgreSQL host and port, database, user, and password in the console. For services with private CAs, see the [console setup instructions](console/README.md). Set `BARNACLE_PG_ALLOWED_ADDRS` to exact addresses to restrict destinations, or set it to an empty string to use only `BARNACLE_PG_ADDR`. The console container proxies HTTP and WebSocket requests to Barnacle on the same browser origin; it is an optional debugging artifact, not part of the Barnacle binary.

Try SQL over HTTP:

```sh
curl -sS http://localhost:8080/sql \
  -H 'Neon-Connection-String: postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle' \
  -d '{"query":"select $1::int as answer","params":[42]}'
```

## Neon driver

```js
import { neon, neonConfig, Client } from '@neondatabase/serverless';
import WebSocket from 'ws';

const url = 'postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle';
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
| Direct PostgreSQL | `BARNACLE_PG_ADDR=postgres:5432` |
| PgBouncer or another pooler | `BARNACLE_PG_ADDR=pgbouncer:6432` |
| Both routes | `BARNACLE_PG_ALLOWED_ADDRS=postgres:5432,pgbouncer:6432` |

Addresses may instead be loopback ports on one host, such as `127.0.0.1:5432` and `127.0.0.1:6432`. With no allowlist, clients cannot choose a different upstream. Read the [deployment guide](docs/deployment.md) for routing, network isolation, TLS, configuration, systemd packages, and production checks.

## Interfaces and operations

| Endpoint | Purpose |
| --- | --- |
| `POST /sql` | One query or a transactional batch over HTTP |
| `GET /v2` (`/v1` also works) | PostgreSQL wire session over WebSocket |
| `GET /healthz`, `GET /readyz` | Liveness and optional PostgreSQL transport readiness |
| `GET /metrics` | Optional Prometheus metrics (`BARNACLE_METRICS=true`) |

See the [API and protocol reference](docs/api.md) for request and response details, limits, and cancellation. An OpenTelemetry Collector can scrape `/metrics`; Barnacle does not emit OTLP or traces. Keep metrics private.

## Build and contribute

The [development guide](docs/development.md) covers tests, integration suites, benchmarks, release binaries, the `.deb`, and the binary-based container. The default Dockerfile builds from source but its runtime image contains no Go toolchain. CI also builds and tests a Fedora RPM.
