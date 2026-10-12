# Authentication

Barnacle passes credentials to PostgreSQL. It does not validate access tokens or choose database roles from token claims. Configure PostgreSQL authentication and role permissions for each destination.

| Entrance | Client credential | PostgreSQL authentication |
| --- | --- | --- |
| HTTP connection URL | User and password in `Neon-Connection-String` | Password, MD5, or SCRAM-SHA-256 |
| HTTP Basic | `Authorization: Basic <credentials>` with PostgreSQL username and password; URL supplies host and database | Password, MD5, or SCRAM-SHA-256 |
| HTTP Bearer | `Authorization: Bearer <token>`; URL supplies user and database | OAuth SASL only |
| WebSocket | PostgreSQL wire messages inside binary frames | The WebSocket client authenticates with PostgreSQL |

For HTTP bearer requests, Barnacle gives pgx the token through `OAuthTokenProvider`, clears the PostgreSQL password, and requires the `oauth` authentication method. A PostgreSQL route offering only password, SCRAM, MD5, or `trust` returns HTTP `502` with `BARNACLE_UPSTREAM_OAUTH_UNAVAILABLE`. The token is never retried as a password. PostgreSQL must have an OAuth validator that checks the token and authorizes the requested role. PostgreSQL 18 requires a separately configured validator module; Barnacle does not provide one. The local Compose PostgreSQL 17 SCRAM setup does not accept bearer requests.

For example, once PostgreSQL OAuth is configured:

```sh
curl -sS https://db.example.com/sql \
  -H 'Authorization: Bearer <token>' \
  -H 'Neon-Connection-String: postgres://app@db.example.com/app' \
  -d '{"query":"select current_user","params":[]}'
```

With the Neon driver, use `neon(databaseUrlWithoutPassword, { authToken: getToken })` and point `fetchEndpoint` at Barnacle. The [OAuth integration suite](development.md#postgresql-oauth-end-to-end) tests PostgreSQL 18 with [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator), including invalid tokens and role mapping.

For password requests, the PostgreSQL username and password can be in the connection URL or in HTTP Basic, but not both. Barnacle requires a password authentication exchange; a `trust` rule does not silently accept the connection. Add `require_auth=scram-sha-256` or `require_auth=md5` to the URL to require one method specifically. Barnacle accepts `sslmode`, `channel_binding`, and `application_name` URL options and sets the actual upstream address and TLS policy from its configuration.

WebSocket sessions are checked for the same `trust` bypass. Until PostgreSQL sends its first `ReadyForQuery`, Barnacle watches the server's login messages. If PostgreSQL answers `AuthenticationOk` without first requesting a credential (a `trust` rule), Barnacle withholds that message, closes the WebSocket with code `1008`, and increments `barnacle_websocket_login_rejections_total{reason="unauthenticated"}`. Cleartext, MD5, SCRAM-SHA-256, and OAUTHBEARER logins all request a credential and are unaffected, as is all traffic after login. Because every client reaches PostgreSQL from Barnacle's address, a `pg_hba.conf` rule that trusts that address would otherwise log in any role without a password. If you deliberately run a `trust` upstream such as a local pooler, set `BARNACLE_WS_ALLOW_TRUST_AUTH=true`. Clients must not send an `SSLRequest` inside the tunnel; an unexpected reply before login also closes the session (reason `malformed`).

Each HTTP request creates and closes its own PostgreSQL connection. WebSocket sessions have a separate upstream socket and authenticate over the PostgreSQL wire protocol; the HTTP Basic and Bearer headers do not authenticate a WebSocket session. Keep connection URLs and Authorization headers out of ingress logs, use HTTPS/WSS to the ingress, and use verified TLS to PostgreSQL.

## Request admission and credential limits

HTTP requests validate routing and credential headers before reading the body. Barnacle reads the complete upload under its byte limit before opening PostgreSQL, then allows at most five seconds for connection and authentication. Failed authentication skips JSON decoding. Successful authentication is followed by an allocation-free depth and token check, then ordinary JSON decoding and query validation. No SQL executes until the complete request passes these checks.

Limits are 16 KiB for the connection URL, 16 KiB for a bearer token, 1 KiB each for decoded username and database name, and 4 KiB for the decoded password. The Authorization header is capped at 16 KiB plus seven scheme bytes. These byte limits apply to URL and Basic credentials; excess returns `431` before reading the body, without truncating or echoing the credential. Duplicate Authorization headers return `400`. See [request limits](decisions/sql-http-limits.md) for JSON complexity limits and error precedence.
