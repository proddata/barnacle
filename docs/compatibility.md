# Neon driver compatibility

Barnacle supports the Neon serverless driver's SQL-over-HTTP and PostgreSQL-over-WebSocket interfaces. Configure the driver's `fetchEndpoint` to point to Barnacle's `/sql` endpoint and `wsProxy` to point to `/v2`. Barnacle connects to operator-configured PostgreSQL or pooler addresses; it does not provide Neon's platform routing or compute lifecycle services.

## Supported interfaces

| Driver interface | Barnacle endpoint | Behavior |
| --- | --- | --- |
| `neon()` and `sql.transaction()` | `POST /sql` | Parameterized queries and transactional batches of up to 100 queries |
| `Client` and `Pool` | `GET /v2` | Persistent PostgreSQL wire sessions over WebSocket |
| `Pool.query()` with `poolQueryViaFetch` | `POST /sql` | HTTP query behavior instead of a persistent pool session |

HTTP results include Neon-style rows, field metadata, command tags, and row counts. Array mode, raw-text output, and batch read-only, isolation, and deferrable options are supported. A batch executes in one transaction and stops on the first error. See the [API reference](api.md) for request headers, result shapes, limits, and errors.

For PostgreSQL password authentication with SCRAM or MD5, set `neonConfig.pipelineConnect = false` for WebSocket clients. Keep `forceDisablePgSSL = true`: Barnacle establishes and verifies a separate TLS connection to PostgreSQL. Use HTTPS and WSS for the client connection. The [authentication guide](authentication.md) describes password and OAuth deployments.

## Scope and differences

- Barnacle uses a configured upstream address or an explicit address allowlist. Neon endpoint discovery, compute wake-up, platform request IDs, and `Neon-Pool-Opt-In` are not implemented.
- HTTP opens a PostgreSQL connection for each request. WebSocket sessions retain PostgreSQL session state until they close. A pooler can reduce database backend churn; choose its pool mode according to the application's session needs.
- HTTP batches are transactional for ordinary SQL. Explicit transaction-control statements can change that behavior; see [batch semantics](decisions/http-batch-semantics.md).
- Buffered HTTP results are the default. `Barnacle-Response-Mode: streaming` allows a larger single-query response while keeping Barnacle's memory use lower. The Neon driver's `neon()` API still collects and parses the complete JSON response on the client.
- The driver's `Client.query()` does not expose a COPY stream. Use a PostgreSQL client with COPY streaming support when needed; Barnacle relays the WebSocket wire messages.

Barnacle tests against the published driver in the [integration suite](development.md#test-and-inspect). The [API reference](api.md) is the source for Barnacle's supported protocol rather than an assumption of complete Neon proxy parity.
