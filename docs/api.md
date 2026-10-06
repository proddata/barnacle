# API and protocol

Barnacle supports [Neon serverless driver](https://github.com/neondatabase/serverless) queries over HTTP and PostgreSQL sessions over WebSocket, but does not implement Neon's platform routing or every proxy feature. `neon()` and `sql.transaction()` use `POST /sql`; `Client` and `Pool` use `/v2` unless `poolQueryViaFetch` is enabled. Point the driver's `fetchEndpoint` and `wsProxy` at Barnacle. For a SCRAM-authenticated database, set `pipelineConnect: false`; the driver's password pipeline assumes cleartext authentication. See the [driver configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md) and [known compatibility gaps](../todo.md).

Barnacle normally listens on plain HTTP behind an HTTPS/WSS ingress. Send credentials and tokens only over HTTPS/WSS, and exclude them from access logs. [Authentication](authentication.md) and [deployment modes](deployment.md) cover the upstream trust boundary.

## On this page

- [Endpoints](#endpoints)
- [Native Barnacle interface](#native-barnacle-interface)
- [Neon-compatible interface and client example](#neon-compatible-interface)
- [HTTP query request and response](#post-sql)
- [PostgreSQL over WebSocket](#get-v2-postgresql-over-websocket)
- [CORS and operational endpoints](#cors-preflight-and-operational-endpoints)
- [Size and timing limits](#size-and-timing-limits)

## Endpoints

| Method and path | Input | Successful response |
| --- | --- | --- |
| `POST /sql` | JSON query or batch; PostgreSQL connection header | `200` JSON result or `{ "results": [...] }` |
| `OPTIONS /sql` | Browser preflight headers | `204` with CORS headers |
| `GET /v2` | WebSocket upgrade; optional `address` query parameter | `101` upgrade, then binary PostgreSQL wire messages |
| `GET /healthz` | None | `200`, `ok\n` |
| `GET /readyz` | None | `200`, `ok\n`, or `503` if the configured upstream probe fails |
| `GET /metrics` | None; requires `BARNACLE_METRICS=true` | `200` Prometheus text on the configured metrics listener; otherwise `404` |

## Native Barnacle interface

`POST /sql` accepts native headers and their Neon aliases. The JSON body contains SQL and parameters; connection details go in headers.

### Connection and authentication

Choose one credential form:

**Password in the connection URL**

```http
Connection-String: postgres://app:secret@db.example.com:5432/app
```

**HTTP Basic** (when Barnacle's OIDC gate is off)

```http
Connection-String: postgres://db.example.com:5432/app
Authorization: Basic YXBwOnNlY3JldA==
```

The Basic example encodes `app:secret`; `curl -u app:secret` sets that header.

**HTTP Bearer**

```http
Connection-String: postgres://app@db.example.com:5432/app
Authorization: Bearer <token>
```

`Neon-Connection-String` is an alias for `Connection-String`. Send one connection header. The OIDC gate requires Bearer; without it, Basic and URL passwords are available. WebSocket clients authenticate over the PostgreSQL wire protocol after the `/v2` upgrade. See [authentication](authentication.md) for the details.

### Query options

Use `query` and optional `params` for one query, or `queries` with 1–100 query objects for a transaction batch. `arrayMode` on an individual query overrides the request header or `options.arrayMode`. The native headers, Neon headers, and optional JSON `options` fields are aliases on `/sql`:

| Native header | Neon header alias | JSON `options` field |
| --- | --- | --- |
| `Array-Mode` | `Neon-Array-Mode` | `arrayMode` |
| `Raw-Text-Output` | `Neon-Raw-Text-Output` | `rawText` |
| `Batch-Read-Only` | `Neon-Batch-Read-Only` | `readOnly` |
| `Batch-Isolation-Level` | `Neon-Batch-Isolation-Level` | `isolationLevel` |
| `Batch-Deferrable` | `Neon-Batch-Deferrable` | `deferrable` |

Allowed header values and examples (the same values apply to each `Neon-*` alias):

- `Array-Mode: true` or `false` (default): Return rows as arrays when `true`.
- `Raw-Text-Output: true` or `false` (default): Return PostgreSQL text values instead of Barnacle's JSON type conversion when `true`.
- `Batch-Read-Only: true` or `false` (default): Request a read-only transaction when `true`.
- `Batch-Isolation-Level: ReadUncommitted`, `ReadCommitted` (default), `RepeatableRead`, or `Serializable`: Choose the transaction isolation level. Case and spaces are ignored.
- `Batch-Deferrable: true` or `false` (default): Request a deferrable transaction when `true`; PostgreSQL decides whether the selected transaction options are valid.

Boolean header values are case-insensitive. Invalid values on applicable headers return `400`. The `Batch-*` settings apply only to transactions and are ignored for single queries.

Supplying more than one form of the same setting returns `400`. For example:

```sh
curl -sS http://localhost:8080/sql \
  -H 'Content-Type: application/json' \
  -H 'Connection-String: postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle' \
  -H 'Array-Mode: true' \
  -d '{"query":"select $1::int as answer","params":[42]}'
```

`OPTIONS /sql` allows both connection header names and the option aliases for permitted browser origins.

## Neon-compatible interface

Install the published driver and `ws` for a Node.js client:

```sh
npm install @neondatabase/serverless ws
```

With the local Compose stack running, save this as `neon-example.mjs` and run `node neon-example.mjs`:

```js
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const url = 'postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle';
neonConfig.fetchEndpoint = 'http://localhost:8080/sql';
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = false; // Local HTTP/WS example only.
neonConfig.wsProxy = () => 'localhost:8080/v2';
neonConfig.pipelineConnect = false; // Compose PostgreSQL uses SCRAM.
neonConfig.forceDisablePgSSL = true; // Barnacle handles upstream TLS.

const sql = neon(url);
console.log(await sql.query('select $1::int as answer', [42]));
console.log(await sql.transaction([sql`select 1::int as first`, sql`select 2::int as second`]));

const client = new Client(url);
await client.connect();
try {
  console.log((await client.query('select 42::int as answer')).rows);
} finally {
  await client.end();
}
```

The PostgreSQL host in `url` is `postgres:5432` because Barnacle reaches PostgreSQL inside Compose; the Node.js client connects to Barnacle at `localhost:8080`. In production, use an HTTPS/WSS ingress and set `useSecureWebSocket` accordingly. `neon()` uses `/sql`; `Client` uses `/v2`. The [driver configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md) describes other client settings.

| Neon driver setting | Barnacle relationship |
| --- | --- |
| `fetchEndpoint` | Point at `/sql`. |
| `wsProxy` | Point at `/v2`. |
| `pipelineConnect` | Set `false` for SCRAM or MD5 so the driver waits for PostgreSQL's authentication challenge; this changes client behavior, not Barnacle's API. |
| `forceDisablePgSSL` | Keep `true` for this WebSocket transport. Barnacle separately verifies TLS to PostgreSQL. |
| `useSecureWebSocket` | Use `true` behind a WSS ingress; `false` is only for the local HTTP/WS example. |

## `POST /sql`

Send one JSON object with either `query` or `queries`. There are no Barnacle-specific URL query parameters for this endpoint. The request body is plain JSON; compressed request bodies are not supported.

### Request headers

See [connection and authentication](#connection-and-authentication) and the [option header values](#query-options). Use `Content-Type: application/json`; `Accept-Encoding: gzip` enables compressed responses of at least 1 KiB. `Origin` is checked against the configured origin policy. See [deployment](deployment.md) for ingress and routing rules.

### JSON body fields

| Field | Type | Use |
| --- | --- | --- |
| `query` | String | Required for a single query; must contain non-whitespace SQL. |
| `params` | Array | Optional values for `$1`, `$2`, and so on; defaults to `[]`. |
| `arrayMode` | Boolean | Optional per-query override for `Array-Mode`, `Neon-Array-Mode`, or `options.arrayMode`. |
| `queries` | Array of query objects | Batch form, containing 1–100 objects with `query`, optional `params`, and optional `arrayMode`. Use this instead of the top-level `query`. |
| `options` | Object | Alternative to the `Neon-*` option headers; see the alias table above. |

### Single query

```http
POST /sql HTTP/1.1
Content-Type: application/json
Neon-Connection-String: postgres://app:secret@db.example.com:5432/app

{"query":"select $1::int4 as answer","params":[42]}
```

`query` must contain non-whitespace SQL. `params` is optional and defaults to an empty array. Parameters correspond to `$1`, `$2`, and so on; JSON strings, numbers, booleans, nulls, objects, and arrays are accepted. Explicit PostgreSQL casts help resolve ambiguous parameter types. Optional `arrayMode` is a boolean on the query object.

```http
HTTP/1.1 200 OK
Content-Type: application/json

{
  "rows": [{"answer": 42}],
  "fields": [{
    "name": "answer", "tableID": 0, "columnID": 0,
    "dataTypeID": 23, "dataTypeSize": 4, "dataTypeModifier": -1,
    "format": "text"
  }],
  "command": "SELECT", "rowCount": 1, "rowAsArray": false
}
```

`rows` is an array of objects by default; duplicate column names overwrite earlier values in an object. With `arrayMode: true`, it is an array of arrays in column order and `rowAsArray` is `true`. `fields` retains every column in order, including duplicate names. Each field contains its name, source table and column IDs (`0` for computed columns), PostgreSQL type OID, type size and modifier, and `format: "text"`. `command` is the first word of PostgreSQL's command tag. `rowCount` is the count from that tag, or `null` when the tag has no count, as with DDL.

| Result field | Type | Meaning |
| --- | --- | --- |
| `rows` | Array | Objects by default, or arrays when `rowAsArray` is `true`. |
| `fields` | Array | Column metadata in query order. |
| `command` | String | PostgreSQL command tag's command, such as `SELECT` or `INSERT`. |
| `rowCount` | Integer or `null` | Count in the command tag, if present. |
| `rowAsArray` | Boolean | Whether rows use positional arrays. |

For example, `Neon-Array-Mode: true` and `Neon-Raw-Text-Output: true` change the example's `rows` to `[["42"]]` and `rowAsArray` to `true`; the other result fields keep the same shape.

### Batch

```http
POST /sql HTTP/1.1
Content-Type: application/json
Neon-Connection-String: postgres://app:secret@db.example.com:5432/app
Neon-Batch-Isolation-Level: Serializable

{"queries":[{"query":"select 1::int4 as value","params":[]},{"query":"select 2::int4 as value","params":[],"arrayMode":true}]}
```

`queries` must contain 1–100 query objects. Each has the same `query`, optional `params`, and optional `arrayMode` fields as a single request. Barnacle executes them in order inside a PostgreSQL transaction, then commits if they succeed. A query object's `arrayMode` overrides `Neon-Array-Mode` for that result.

```json
{
  "results": [
    {
      "rows": [{"value": 1}],
      "fields": [{"name":"value","tableID":0,"columnID":0,"dataTypeID":23,"dataTypeSize":4,"dataTypeModifier":-1,"format":"text"}],
      "command": "SELECT", "rowCount": 1, "rowAsArray": false
    },
    {
      "rows": [[2]],
      "fields": [{"name":"value","tableID":0,"columnID":0,"dataTypeID":23,"dataTypeSize":4,"dataTypeModifier":-1,"format":"text"}],
      "command": "SELECT", "rowCount": 1, "rowAsArray": true
    }
  ]
}
```

Barnacle does not parse SQL inside a batch query object. See the [batch semantics decision](decisions/http-batch-semantics.md) before relying on unconditional atomicity for arbitrary SQL or multiple statements inside one query string.

### Error responses

Most Barnacle-generated HTTP errors are JSON with `message` and `code: "BARNACLE_ERROR"`:

```http
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"message":"provide query or queries","code":"BARNACLE_ERROR"}
```

- **`400` — Invalid request or PostgreSQL error.** Malformed JSON or request shape, missing credentials, invalid routing or batch options produce a Barnacle JSON error. PostgreSQL errors also use `400`, with a SQLSTATE `code` such as `22012` for division by zero or `28P01` for a rejected password.
- **`401` — Invalid or missing access token.** The OIDC gate or PostgreSQL OAuth authentication rejected the token. JSON response with `WWW-Authenticate: Bearer error="invalid_token"`.
- **`403` — Disallowed browser origin.** Plain-text `origin denied`.
- **`413` — Size limit exceeded.** A request body above 1 MiB, or a PostgreSQL row, buffered result, or response above its configured limit when detected before response headers are sent. JSON response.
- **`502` — Upstream failure.** PostgreSQL connection, TLS, or query transport failure, or missing upstream OAuth support. JSON response; OAuth capability mismatch uses `code: "BARNACLE_UPSTREAM_OAUTH_UNAVAILABLE"`.
- **`503` — Connection limit reached.** HTTP query or upstream connection limit reached. JSON response.

Unknown paths return `404`; an unsupported method on a known path returns `405`. PostgreSQL errors include `message`, SQLSTATE `code`, `severity`, and nullable details such as `detail`, `hint`, `position`, `schema`, `table`, `column`, and `constraint`. A malformed HTTP request never reaches PostgreSQL. A client abort cancels its running HTTP query. If a streamed response fails after headers were sent, Barnacle cannot change its `200` status; the client receives incomplete JSON.

The published Neon driver parses error JSON and copies SQLSTATE fields into `NeonDbError` only for HTTP `400`. For other non-OK statuses, it puts the status and raw response text in the error message. See the [Neon error-parity decision](decisions/neon-error-parity.md) for the case-by-case comparison.

## `GET /v2`: PostgreSQL over WebSocket

This path accepts a standard WebSocket upgrade and relays **binary PostgreSQL wire messages** in both directions. It does not accept the HTTP query JSON body or parse SQL. After a successful upgrade, PostgreSQL authentication and query errors are wire-protocol messages inside WebSocket frames, not HTTP JSON errors.

- **`?address=host:port`:** Optional PostgreSQL destination when `BARNACLE_PG_ALLOWED_ADDRS` is configured. It must match an allowed address. If omitted, Barnacle uses `BARNACLE_PG_ADDR` when configured. In fixed mode the requested address is ignored. Supply one `address` value in routed mode.
- **WebSocket handshake:** The client library normally sends `Upgrade: websocket`, `Connection: Upgrade`, `Sec-WebSocket-Version: 13`, and `Sec-WebSocket-Key`.
- **`Origin`:** Checked against the same origin policy as `/sql`; a rejected origin receives `403` before upgrade.
- **`Cookie: barnacle_access_token=<token>`:** Required on the upgrade when Barnacle's OIDC gate is enabled. Barnacle verifies the token; the WebSocket client still performs PostgreSQL wire authentication after upgrade. Barnacle does not issue this cookie.

For example, `GET /v2?address=pgbouncer:6432` selects that allowlisted route. On success the server replies `101 Switching Protocols` with `Upgrade: websocket`, `Connection: Upgrade`, and `Sec-WebSocket-Accept`; the first application data is then PostgreSQL wire traffic, not JSON. Before upgrade, invalid handshake or routing returns `400`, missing/invalid cookie returns `401`, a full connection limit or server shutdown returns `503`, and an unreachable PostgreSQL target returns `502`. These failures are plain-text HTTP responses. PostgreSQL CancelRequest packets are relayed across WebSocket connections. A disconnected client releases its upstream session and Barnacle attempts to cancel work still running there. WebSocket idle and write timeouts are configurable; see [deployment](deployment.md).

The published Neon `Client` and `Pool` retain PostgreSQL session state across WebSocket queries, including transactions, `SET`, named prepared statements, cursors, and `LISTEN`/`NOTIFY`. A `Pool` checkout can inherit state left by an earlier checkout on the same connection; callers should reset session settings before release when they need isolation. The published `Client.query()` API does not provide a COPY stream: `COPY FROM STDIN` fails with `No source stream defined`, and its query handler discards `COPY TO STDOUT` data. Use a PostgreSQL client with COPY streaming support for those operations; Barnacle's `/v2` transport relays the wire messages unchanged.

## CORS preflight and operational endpoints

`OPTIONS /sql` accepts a permitted `Origin` and returns `204 No Content` with `Access-Control-Allow-Methods: POST, OPTIONS`, `Access-Control-Allow-Headers` listing `Authorization`, `Content-Type`, both connection headers, and the native and Neon option headers above, and `Access-Control-Max-Age: 600`. For an allowed cross-origin request it also returns `Access-Control-Allow-Origin` with that exact origin. A denied origin returns `403 origin denied`. The response varies on `Origin`.

`GET /healthz` always returns `200 OK` with `ok\n` while Barnacle's HTTP server is running. `GET /readyz` returns the same response by default. If `BARNACLE_READY_PG_ADDR` is set, it first probes that PostgreSQL address at the network/TLS level; failure or a concurrent probe returns `503` plain text. The probe does not authenticate or execute SQL.

`GET /metrics` is available only with `BARNACLE_METRICS=true`, on a separate listener that defaults to `127.0.0.1:9090`. Set `BARNACLE_METRICS_LISTEN` to change its address. The main listener always returns `404` for `/metrics`, and the metrics listener returns `404` for other routes. Its `200` response has `Content-Type: text/plain; version=0.0.4; charset=utf-8` and Prometheus metrics for active WebSockets, accepted WebSocket upgrades since startup, active and completed HTTP SQL requests, HTTP SQL errors and duration, stream interruptions, connection-limit rejections, and upstream failures. The WebSocket total includes sessions that later disconnect or fail PostgreSQL authentication; it does not count rejected upgrades. Stream interruptions have one of four fixed kinds (`result_limit`, `canceled`, `timeout`, `query_or_transport`) and may occur after a `200` response starts, so they are counted separately from HTTP errors. Restrict access to the metrics listener; this endpoint has no authentication.

## Size and timing limits

The defaults are 1 MiB per HTTP request body, 100 queries per batch, 8 MiB of PostgreSQL field data per row, 4 MiB of buffered batch/custom-type results, and 128 MiB of uncompressed JSON per response. HTTP requests have a 15-second read timeout and a separate 30-second query timeout. Idle HTTP keep-alive connections close after 60 seconds. WebSocket idle and write timeouts default to 30 minutes and 30 seconds. Single-query results usually stream; batches and some custom-type results are buffered. The [SQL-over-HTTP limits decision](decisions/sql-http-limits.md) explains the memory tradeoffs. Use WebSocket for sessions or very large individual rows.
