# API and protocol

Hermit implements a subset of the [Neon serverless driver](https://github.com/neondatabase/serverless) proxy interface. `neon()` and `sql.transaction()` use `POST /sql`; `Client` and `Pool` use a PostgreSQL wire session over WebSocket unless `poolQueryViaFetch` is enabled. Point the driver's `fetchEndpoint` and `wsProxy` at Hermit. For a SCRAM-authenticated database, set `pipelineConnect: false`; the driver's password pipeline assumes cleartext authentication. See the [driver configuration](https://github.com/neondatabase/serverless/blob/main/CONFIG.md) and [known compatibility gaps](../todo.md).

Hermit normally listens on plain HTTP behind an HTTPS/WSS ingress. Send credentials and tokens only over HTTPS/WSS, and exclude them from access logs. [Authentication](authentication.md) and [deployment modes](deployment.md) cover the upstream trust boundary.

| Method and path | Input | Successful response |
| --- | --- | --- |
| `POST /sql` | JSON query or batch; PostgreSQL connection details in headers | `200` JSON result or `{ "results": [...] }` |
| `OPTIONS /sql` | Browser preflight headers | `204` with CORS headers |
| `GET /v2`, `GET /v1` | WebSocket upgrade; optional `address` query parameter | `101` upgrade, then binary PostgreSQL wire messages |
| `GET /healthz` | None | `200`, `ok\n` |
| `GET /readyz` | None | `200`, `ok\n`, or `503` if the configured upstream probe fails |
| `GET /metrics` | None; requires `HERMIT_METRICS=true` | `200` Prometheus text; otherwise `404` |

## `POST /sql`

Send one JSON object with either `query` or `queries`. There are no Hermit-specific URL query parameters for this endpoint. The request body is plain JSON; compressed request bodies are not supported.

### Request headers

| Header | Use |
| --- | --- |
| `Content-Type: application/json` | Recommended for the JSON request body. |
| `Neon-Connection-String` | PostgreSQL URL such as `postgres://user:password@host:5432/database`. Without `Authorization`, it must contain a password. With a bearer token, user and database can come from this URL or, in fixed-upstream mode, Hermit's configured defaults. In fixed mode the URL host does not choose the TCP destination; in routed mode its host and port must match `HERMIT_PG_ALLOWED_ADDRS`. |
| `Authorization: Bearer <token>` | HTTP bearer credential. With Hermit's OIDC gate enabled, this is required and PostgreSQL OAuth authentication is required upstream. Without the gate, Hermit forwards the token to PostgreSQL for OAuth or legacy password authentication. See [authentication](authentication.md). |
| `Neon-Array-Mode: true` | Return each row as an array instead of an object. Default is `false`. A body-level `arrayMode` overrides this per query. |
| `Neon-Raw-Text-Output: true` | Return PostgreSQL text values for the driver to parse, for example `"42"` instead of `42`. Default is Hermit's JSON type conversion. |
| `Neon-Batch-Read-Only: true` | Request a read-only transaction for a batch. Ignored for a single query. |
| `Neon-Batch-Isolation-Level` | Batch isolation: `ReadUncommitted`, `ReadCommitted` (default), `RepeatableRead`, or `Serializable`; case and spaces are ignored. Invalid values return `400`. |
| `Neon-Batch-Deferrable: true` | Request a deferrable batch transaction; PostgreSQL decides whether the selected transaction options are valid. |
| `Accept-Encoding: gzip` | Allow gzip for JSON responses of at least 1 KiB. Smaller responses remain uncompressed. |
| `Origin` | Browser origin. Same-origin requests and the exact configured `HERMIT_ALLOWED_ORIGIN` are allowed; other origins return `403`. |

When an ingress terminates HTTPS, it should set `X-Forwarded-Proto: https` on requests to Hermit so same-origin checks use the external scheme. Accept that header only from a trusted ingress.

### JSON body fields

| Field | Type | Use |
| --- | --- | --- |
| `query` | String | Required for a single query; must contain non-whitespace SQL. |
| `params` | Array | Optional values for `$1`, `$2`, and so on; defaults to `[]`. |
| `arrayMode` | Boolean | Optional per-query override for `Neon-Array-Mode`. |
| `queries` | Array of query objects | Batch form, containing 1–100 objects with `query`, optional `params`, and optional `arrayMode`. Use this instead of the top-level `query`. |

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

`queries` must contain 1–100 query objects. Each has the same `query`, optional `params`, and optional `arrayMode` fields as a single request. Hermit executes them in order inside a PostgreSQL transaction, then commits if they succeed. A query object's `arrayMode` overrides `Neon-Array-Mode` for that result.

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

Hermit does not parse SQL inside a batch query object. See the [batch semantics decision](decisions/http-batch-semantics.md) before relying on unconditional atomicity for arbitrary SQL or multiple statements inside one query string.

### Error responses

Most Hermit-generated HTTP errors are JSON with `message` and `code: "HERMIT_ERROR"`:

```http
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"message":"provide query or queries","code":"HERMIT_ERROR"}
```

| Status | Typical cause | Response |
| --- | --- | --- |
| `400` | Malformed JSON or request shape, body above 1 MiB, missing credentials, invalid routing or batch options | Hermit JSON error. PostgreSQL errors also use `400`, but their `code` is the SQLSTATE (for example `22012` for division by zero or `28P01` for a rejected password). |
| `401` | Invalid or missing access token with the OIDC gate, or PostgreSQL OAuth token rejection | JSON error; `WWW-Authenticate: Bearer error="invalid_token"`. |
| `403` | Disallowed browser `Origin` | Plain-text `origin denied`. |
| `413` | PostgreSQL row, buffered result, or response exceeds a configured size limit, when detected before response headers are sent | JSON error. |
| `502` | PostgreSQL connection, TLS, or query transport failure; upstream lacks required OAuth support | JSON error. OAuth capability mismatch uses `code: "HERMIT_UPSTREAM_OAUTH_UNAVAILABLE"`. |
| `503` | HTTP query or upstream connection limit reached | JSON error. |

Unknown paths return `404`; an unsupported method on a known path returns `405`. PostgreSQL errors include `message`, SQLSTATE `code`, `severity`, and nullable details such as `detail`, `hint`, `position`, `schema`, `table`, `column`, and `constraint`. A malformed HTTP request never reaches PostgreSQL. A client abort cancels its running HTTP query. If a streamed response fails after headers were sent, Hermit cannot change its `200` status; the client receives incomplete JSON.

## `GET /v2` and `GET /v1`: PostgreSQL over WebSocket

Both paths accept a standard WebSocket upgrade and relay **binary PostgreSQL wire messages** in both directions. They do not accept the `POST /sql` JSON body and do not parse SQL. After a successful upgrade, PostgreSQL authentication and query errors are wire-protocol messages inside WebSocket frames, not HTTP JSON errors.

| Input | Use |
| --- | --- |
| `?address=host:port` | Optional requested PostgreSQL destination when `HERMIT_PG_ALLOWED_ADDRS` is configured. It must match an allowed address; if omitted, Hermit uses `HERMIT_PG_ADDR` when configured. In fixed mode the requested address is ignored. Supply one `address` value in routed mode. |
| `Upgrade: websocket`, `Connection: Upgrade`, `Sec-WebSocket-Version: 13`, `Sec-WebSocket-Key` | Standard WebSocket handshake headers, normally set by the client library. |
| `Origin` | Checked against the same origin policy as `/sql`; a rejected origin receives `403` before upgrade. |
| `Cookie: hermit_access_token=<token>` | Required on the upgrade when Hermit's OIDC gate is enabled. Hermit verifies the token; the WebSocket client still performs PostgreSQL wire authentication after upgrade. Hermit does not issue this cookie. |

For example, `GET /v2?address=pgbouncer:6432` selects that allowlisted route. On success the server replies `101 Switching Protocols` with `Upgrade: websocket`, `Connection: Upgrade`, and `Sec-WebSocket-Accept`; the first application data is then PostgreSQL wire traffic, not JSON. Before upgrade, invalid handshake or routing returns `400`, missing/invalid cookie returns `401`, a full connection limit or server shutdown returns `503`, and an unreachable PostgreSQL target returns `502`. These failures are plain-text HTTP responses. PostgreSQL CancelRequest packets are relayed across WebSocket connections. A disconnected client releases its upstream session and Hermit attempts to cancel work still running there. WebSocket idle and write timeouts are configurable; see [deployment](deployment.md).

## CORS preflight and operational endpoints

`OPTIONS /sql` accepts a permitted `Origin` and returns `204 No Content` with `Access-Control-Allow-Methods: POST, OPTIONS`, `Access-Control-Allow-Headers` listing `Authorization`, `Content-Type`, and the `Neon-*` headers above, and `Access-Control-Max-Age: 600`. For an allowed cross-origin request it also returns `Access-Control-Allow-Origin` with that exact origin. A denied origin returns `403 origin denied`. The response varies on `Origin`.

`GET /healthz` always returns `200 OK` with `ok\n` while Hermit's HTTP server is running. `GET /readyz` returns the same response by default. If `HERMIT_READY_PG_ADDR` is set, it first probes that PostgreSQL address at the network/TLS level; failure or a concurrent probe returns `503` plain text. The probe does not authenticate or execute SQL.

`GET /metrics` is available only with `HERMIT_METRICS=true`; otherwise it returns `404`. Its `200` response has `Content-Type: text/plain; version=0.0.4; charset=utf-8` and Prometheus metrics for active WebSockets, HTTP SQL requests and errors, SQL duration, connection-limit rejections, and upstream failures. Restrict this endpoint at the ingress if enabled.

## Size and timing limits

The defaults are 1 MiB per HTTP request body, 100 queries per batch, 8 MiB of PostgreSQL field data per row, 4 MiB of buffered batch/custom-type results, and 128 MiB of uncompressed JSON per response. HTTP requests have a 15-second read timeout and a separate 30-second query timeout. Idle HTTP keep-alive connections close after 60 seconds. WebSocket idle and write timeouts default to 30 minutes and 30 seconds. Single-query results usually stream; batches and some custom-type results are buffered. The [SQL-over-HTTP limits decision](decisions/sql-http-limits.md) explains the memory tradeoffs. Use WebSocket for sessions or very large individual rows.
