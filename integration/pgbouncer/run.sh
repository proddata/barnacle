#!/bin/sh
set -eu

compose() {
  docker compose -f compose.yaml -f integration/pgbouncer/compose.yaml "$@"
}
cleanup() {
  compose stop hermit_pooltest pgbouncer_session pgbouncer_transaction pgbouncer_statement || true
  compose rm -f hermit_pooltest pgbouncer_session pgbouncer_transaction pgbouncer_statement || true
}
trap cleanup EXIT INT TERM

HERMIT_PG_QUERY_EXEC_MODE=exec compose up -d --build hermit_pooltest
go run ./integration/pgbouncer/pgx-repro.go
node integration/pgbouncer/pool-modes.mjs
HERMIT_BASE_URL=http://127.0.0.1:18080 \
TEST_DATABASE_URL=postgres://hermit:hermit_dev_password@postgres:5432/hermit \
  node --test integration/neon.test.mjs

HERMIT_PG_QUERY_EXEC_MODE=cache_describe compose up -d --no-deps hermit_pooltest
node integration/pgbouncer/pool-modes.mjs
