#!/bin/sh
set -eu

case "${1:-}" in
    haproxy-postgres) T11_UPSTREAM=haproxy_leader:15432; T11_POOLER=0 ;;
    pgbouncer-postgres) T11_UPSTREAM=pgbouncer_tls:6432; T11_POOLER=1 ;;
    haproxy-pgbouncer) T11_UPSTREAM=haproxy_leader:16432; T11_POOLER=1 ;;
    *) echo 'usage: run.sh haproxy-postgres|pgbouncer-postgres|haproxy-pgbouncer' >&2; exit 2 ;;
esac
export T11_UPSTREAM

compose() {
    docker compose -f compose.yaml -f integration/topology/compose.yaml "$@"
}
cleanup() {
    compose stop hermit_topology haproxy_leader pgbouncer_tls >/dev/null 2>&1 || true
    compose rm -f hermit_topology haproxy_leader pgbouncer_tls >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

compose up -d --build hermit_topology
ready=false
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl --silent --fail --max-time 2 \
        -H 'Neon-Connection-String: postgres://hermit:hermit_dev_password@postgres:5432/hermit' \
        -H 'Content-Type: application/json' \
        --data '{"query":"select 1"}' \
        http://127.0.0.1:18083/sql >/dev/null; then
        ready=true
        break
    fi
    sleep 1
done
if [ "$ready" != true ]; then
    compose logs hermit_topology haproxy_leader pgbouncer_tls
    echo "private upstream $1 did not pass a TLS-authenticated SQL query" >&2
    exit 1
fi

HERMIT_BASE_URL=http://127.0.0.1:18083 \
TEST_DATABASE_URL=postgres://hermit:hermit_dev_password@postgres:5432/hermit \
HERMIT_TEST_PGBOUNCER="$T11_POOLER" \
  npm test --prefix integration
