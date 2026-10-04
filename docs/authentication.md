# Authentication

| Entrance | What the client sends | Who verifies it |
| --- | --- | --- |
| HTTP password | `Neon-Connection-String` with user, database, and password | PostgreSQL |
| HTTP bearer | `Authorization: Bearer <token>`; user and database come from the connection string or Hermit defaults | pgx uses OAUTHBEARER when PostgreSQL offers it; with Hermit's OIDC gate enabled, OAUTHBEARER is required |
| WebSocket | Optional access-token cookie, then PostgreSQL wire messages inside binary frames | Optional Hermit OIDC gate checks the upgrade; PostgreSQL authentication is performed by the WebSocket client |

Without the gate, Hermit does not validate a bearer token: pgx uses OAUTHBEARER if PostgreSQL offers it, with password authentication available for older setups. Setting both `HERMIT_OIDC_ISSUER` and `HERMIT_OIDC_AUDIENCE` enables an access-token gate before any PostgreSQL connection. It discovers the issuer's JWKS, permits only RS256 signatures with an `at+jwt` token type, and checks issuer, audience, expiry, issue time, and required access-token claims. Keys refresh every five minutes or when a new key ID appears; a failed refresh rejects requests. The gate follows the [JWT access-token profile](https://www.rfc-editor.org/rfc/rfc9068.html) and [OIDC discovery](https://openid.net/specs/openid-connect-discovery-1_0.html).

With the gate enabled, **every HTTP query needs `Authorization: Bearer <token>`** and every WebSocket upgrade needs a valid `hermit_access_token` cookie. A trusted frontend must set that cookie for Hermit's origin with `Secure`, `HttpOnly`, and an appropriate `SameSite` policy; Hermit does not issue cookies. For HTTP, Hermit gives pgx the verified token through `OAuthTokenProvider`, sets `RequireAuth` to `oauth`, and leaves the PostgreSQL password empty. If the selected PostgreSQL route offers only password, SCRAM, or trust authentication, Hermit returns HTTP 502 with `HERMIT_UPSTREAM_OAUTH_UNAVAILABLE` instead of falling back or calling the token invalid. Check the upstream version, `pg_hba.conf` rules, and any pooler on that route. The Compose PostgreSQL 17 SCRAM setup therefore cannot serve gated HTTP queries. PostgreSQL 18 also needs a separately configured validator module and role mapping for native OAuth authentication; Hermit does not provide that module. The WebSocket path relays PostgreSQL wire bytes, so its client must implement the desired PostgreSQL authentication method; the cookie gate does not replace those wire messages.

A bearer request looks like this once such a PostgreSQL validator is configured:

```sh
curl -sS https://db.example.com/sql \
  -H 'Authorization: Bearer <jwt>' \
  -H 'Neon-Connection-String: postgres://app@db.example.com/app' \
  -d '{"query":"select current_user","params":[]}'
```

With the driver, use `neon(databaseUrlWithoutPassword, { authToken: getToken })` and point `fetchEndpoint` at Hermit. The regular integration suite proves header forwarding with a password-shaped token. A separate [PostgreSQL 18 OAuth test](development.md#postgresql-oauth-end-to-end) exercises a signed JWT through Hermit and [`pg_oauth_validator`](https://github.com/proddata/pg_oauth_validator).

For HTTP, Hermit accepts the connection URL's user and database, and its password when no bearer token is supplied. It accepts `sslmode`, `channel_binding`, and `application_name` URL query options, plus `require_auth=scram-sha-256` or `require_auth=md5` to test a specific password method. Hermit sets the actual upstream address and TLS policy from its configuration. With the OIDC gate enabled, OAuth is always required regardless of `require_auth`. Each HTTP query creates and closes its own PostgreSQL connection. WebSocket sessions each have a separate upstream socket, kept for that session's lifetime. Hermit does not pool client connections or reuse a PostgreSQL login across requests. Keep connection URLs and bearer tokens out of ingress access logs, and use HTTPS/WSS to the ingress and verified TLS to PostgreSQL in production.
