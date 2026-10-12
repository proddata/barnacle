# Barnacle debug console

The console is a development and debugging UI for manual queries through Barnacle. It is separate from the Barnacle image and binary and is not intended for production deployment.

Start it with:

```sh
docker compose --profile console up --build
```

Open `http://localhost:8081`. Compose binds the console to host loopback. The workbench has a PostgreSQL editor, schema and table explorer, grid and JSON results with CSV export, and a request debug panel. The **Metrics** view shows Barnacle's WebSocket and HTTP activity and the raw Prometheus response; it refreshes every five seconds. The console proxies `/metrics` through Barnacle's main listener, so leave `BARNACLE_METRICS_LISTEN` empty to use this view.

## Connections and queries

Add, rename, save, and switch connections from the connection bar. Host, database, user, and transport settings are saved in browser local storage. Each connection has its own query tabs and in-memory results. Custom passwords and bearer tokens stay in the current browser tab and must be re-entered after a reload; query text and results are not saved between reloads. The built-in Compose connection uses the development database and password.

**Run all** or `Ctrl/Cmd+Enter` runs every statement in the active tab when no text is selected. Select SQL to use **Run selection**. Statements run in order and stop on the first error; each successful statement gets a result tab. The editor recognizes semicolons inside PostgreSQL quotes, comments, and dollar-quoted strings.

With HTTP selected, **One transaction** sends up to 100 statements in a single `sql.transaction()` request and displays the results together after completion. Keep explicit `BEGIN`, `COMMIT`, and `ROLLBACK` out of an HTTP batch when you need ordinary all-or-nothing behavior. Without that option, HTTP queries are independent requests.

**Max rows** (HTTP only) sends `Barnacle-Max-Rows`, so Barnacle returns at most that many rows per query. The result summary shows the full row count and marks the result as limited. The query still runs to completion on PostgreSQL, so use SQL `LIMIT` to reduce its work. **Cancel** appears next to **Run** while a statement is running. Over HTTP it aborts the request through an `AbortSignal`, Barnacle cancels the PostgreSQL query, and a transaction batch is rolled back. Over WebSocket it closes the session, which also cancels the query but ends any open transaction. In your own code the HTTP equivalent is:

```js
const controller = new AbortController();
const sql = neon(url);
const pending = sql.query('select pg_sleep(60)', [], { fetchOptions: { signal: controller.signal } });
controller.abort(); // rejects `pending`; Barnacle cancels the query
```

The Examples menu has **Cancel a slow query** and **Limit result size** for trying both.

With WebSocket selected, queries in one editor tab share a PostgreSQL session. Use **Connect** and **Disconnect** to manage it, or run SQL to connect automatically. Closing the tab or changing its connection settings closes the session. The Examples menu includes server information, a delayed transaction, and a failed-transaction recovery sequence.

## Authentication and routing

The console uses the published `@neondatabase/serverless` driver. HTTP password and bearer modes use `neon()`; WebSocket password mode uses `Client` with password pipelining disabled for SCRAM and MD5. The HTTP password-method selector can require SCRAM-SHA-256 or MD5. HTTP bearer mode requires PostgreSQL OAuth. WebSocket clients authenticate directly over the PostgreSQL wire protocol.

The host and port in the connection form select the PostgreSQL route. The local Compose stack accepts external hosts by default (`BARNACLE_PG_ALLOWED_ADDRS=*`). To restrict routing, set exact `host:port` values, including `postgres:5432` if you need the local database, or set an empty string for fixed routing. Barnacle verifies upstream TLS against the selected host.

For a hosted PostgreSQL service, enter the host, port, database, and user from its connection information. If it uses a private CA, save the CA certificate as `console/certs/ca.pem` and restart the stack:

```sh
BARNACLE_PG_EXTRA_CA_FILE=/console-certs/ca.pem docker compose --profile console up --build -d
```

The extra CA is added to Barnacle's system and local development CAs. To keep this setting for later Compose starts, put `BARNACLE_PG_EXTRA_CA_FILE=/console-certs/ca.pem` in the gitignored root `.env` file.

For the default Compose database, use host `postgres:5432`, database `barnacle`, user `barnacle`, and password `barnacle_dev_password`. The Explorer lists schemas, tables, views, and columns; a table preview runs a 100-row query.

## Debug data and local development

The HTTP debug panel displays the driver's request headers, JSON body, timing, and a bounded response preview. It masks connection-string passwords and bearer tokens. WebSocket calls show their PostgreSQL operation and whether a connection was reused. The Explorer shows the active session's startup `ParameterStatus` values and backend process ID; it does not show the cancellation secret. SQL, parameters, and results remain visible in the browser tab.

The Compose console uses local HTTP and includes the development password in its bundle. If developers need remote access, use a private HTTPS ingress and keep the console separate from production deployments. For local frontend development, run `npm ci && npm run dev` in `console/` while Barnacle listens on `127.0.0.1:8080`; Vite proxies `/sql`, `/v2`, and `/metrics` to Barnacle.
