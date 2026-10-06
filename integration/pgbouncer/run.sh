#!/bin/sh
set -eu

compose() {
  docker compose -f compose.yaml -f integration/pgbouncer/compose.yaml "$@"
}
cleanup() {
  compose stop barnacle_pooltest pgbouncer_session pgbouncer_transaction pgbouncer_statement || true
  compose rm -f barnacle_pooltest pgbouncer_session pgbouncer_transaction pgbouncer_statement || true
}
trap cleanup EXIT INT TERM

BARNACLE_PG_QUERY_EXEC_MODE=exec compose up -d --build barnacle_pooltest
go run ./integration/pgbouncer/pgx-repro.go
node integration/pgbouncer/pool-modes.mjs
BARNACLE_BASE_URL=http://127.0.0.1:18080 \
TEST_DATABASE_URL=postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle \
  node --test integration/neon.test.mjs

BARNACLE_PG_QUERY_EXEC_MODE=cache_describe compose up -d --no-deps barnacle_pooltest
node integration/pgbouncer/pool-modes.mjs
