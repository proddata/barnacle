# Barnacle

Barnacle is an HTTP and WebSocket gateway for PostgreSQL. It brings serverless and agent-driven applications to self-hosted or managed PostgreSQL through a native API or a compatible subset of the Neon serverless driver interface. Barnacle connects to a fixed database or pooler by default; optional routing is limited to addresses you allow.

```mermaid
flowchart LR
    subgraph App[Application]
        HTTPClient["HTTP JSON / neon()"]
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

Barnacle is independent and is not affiliated with or endorsed by Neon or Databricks. [Neon is part of Databricks](https://neon.com/brand); the Neon name and logo are trademarks of their respective owners.

## Quick start

This is a local development setup with PostgreSQL and Barnacle. The browser console is optional. Docker Compose keeps PostgreSQL inside its network and exposes Barnacle only on your machine.

Start the database and gateway:

```sh
docker compose up --build
```

To include the optional console, use this command instead:

```sh
docker compose --profile console up --build
```

Barnacle listens at [localhost:8080](http://localhost:8080). With the console profile, open [localhost:8081](http://localhost:8081). The console is prefilled for the development database: host `postgres:5432`, database `barnacle`, user `barnacle`, password `barnacle_dev_password`. See the [console guide](console/README.md) for its SQL editor, database explorer, and connection options. Keep the console for development and debugging.

The development connection string is:

```text
postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle
```

Compose generates a PostgreSQL TLS certificate for this setup. The example password is for local development.

### Try the HTTP route with curl

Check that Barnacle is running, then send one parameterized query:

```sh
curl -sS http://localhost:8080/healthz

curl -sS http://localhost:8080/sql \
  -H 'Content-Type: application/json' \
  -H 'Connection-String: postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle' \
  -d '{"query":"select $1::int as answer","params":[42]}'
```

Send two queries in one transactional batch:

```sh
curl -sS http://localhost:8080/sql \
  -H 'Content-Type: application/json' \
  -H 'Neon-Connection-String: postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle' \
  -d '{"queries":[{"query":"select 1::int as first"},{"query":"select 2::int as second"}]}'
```

`postgres:5432` is the service name and port inside the Compose network; PostgreSQL is not published on your host. Barnacle uses that address to select the upstream and the URL's user, password, and database to log in. See [upstream routing](docs/deployment.md#how-requests-choose-postgresql) before connecting another database.

### Try the WebSocket route with the Neon serverless driver

Install the [Neon serverless driver](https://neon.com/docs/serverless/serverless-driver) and `ws` for this Node.js example. Modern Node.js also provides a built-in `WebSocket`, but `ws` gives this example consistent connection cleanup and works with older Node.js versions. See the driver's [source and configuration reference](https://github.com/neondatabase/serverless):

```sh
npm install @neondatabase/serverless ws
```

Save this as `websocket.mjs` and run `node websocket.mjs` while Compose is running:

```js
import { Client, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const url = 'postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle';
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = false; // local demo only
neonConfig.wsProxy = () => 'localhost:8080/v2';
neonConfig.pipelineConnect = false; // Compose uses SCRAM

const client = new Client(url);
await client.connect();
try { console.log((await client.query('select 42::int as answer')).rows); }
finally { await client.end(); }
```

The same driver also offers `neon()` for HTTP queries to `/sql`; its `Client` and `Pool` use `/v2` for PostgreSQL sessions. See the driver's [configuration options](https://github.com/neondatabase/serverless/blob/main/CONFIG.md) and Barnacle's [API reference](docs/api.md). Use HTTPS and WSS at the ingress in production; see [authentication](docs/authentication.md).

## Deployment choices

- **One PostgreSQL instance:** `BARNACLE_PG_ADDR=postgres:5432`.
- **PostgreSQL and its pooler:** `BARNACLE_PG_ALLOWED_ADDRS=postgres:5432,pgbouncer:6432`.
- **Separate gateway to several instances:** `BARNACLE_PG_ALLOWED_ADDRS=db-a.internal:5432,db-b.internal:5432`.

Nearby services may instead use loopback addresses such as `127.0.0.1:5432`. With no allowlist, clients cannot choose a different upstream. Read the [deployment guide](docs/deployment.md) for routing, network isolation, TLS, and production checks, or [package operations](docs/operations.md) for systemd installation.

## Interfaces and operations

| Endpoint | Purpose |
| --- | --- |
| `POST /sql` | One query or a transactional batch over HTTP |
| `GET /v2` | PostgreSQL wire session over WebSocket |
| `GET /healthz`, `GET /readyz` | Liveness and optional PostgreSQL transport readiness |
| `GET /metrics` | Optional Prometheus metrics (`BARNACLE_METRICS=true`) |

See the [API and protocol reference](docs/api.md) for request and response details, limits, and cancellation. An OpenTelemetry Collector can scrape `/metrics`; Barnacle does not emit OTLP or traces. Metrics are off by default in the binary. When enabled, they use a separate `127.0.0.1:9090` listener by default; set `BARNACLE_METRICS_LISTEN` for another private address. Local Compose enables metrics on an unpublished container port for its optional console. The supplied HAProxy example blocks `/metrics` on its public listener.

## Build and contribute

The [development guide](docs/development.md) covers tests, integration suites, benchmarks, release binaries, the `.deb`, and the binary-based container. The default Dockerfile builds from source but its runtime image contains no Go toolchain. CI also builds and tests a Fedora RPM.
