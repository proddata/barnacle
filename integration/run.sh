#!/bin/sh
set -eu

if [ -z "${HERMIT_PG_ADDR:-}" ] && [ -z "${HERMIT_PG_ALLOWED_ADDRS:-}" ]; then
  echo 'set HERMIT_PG_ADDR or HERMIT_PG_ALLOWED_ADDRS' >&2
  exit 1
fi
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL}"
HERMIT_BASE_URL=${HERMIT_BASE_URL:-http://127.0.0.1:8080}
HERMIT_LISTEN=${HERMIT_LISTEN:-127.0.0.1:8080}
export HERMIT_BASE_URL HERMIT_LISTEN

binary="${TMPDIR:-/tmp}/hermit-integration-$$"
log="${TMPDIR:-/tmp}/hermit-integration-$$.log"
cleanup() {
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  rm -f "$binary" "$log"
}
trap cleanup EXIT INT TERM

go build -buildvcs=false -o "$binary" .
"$binary" >"$log" 2>&1 &
server_pid=$!

ready=0
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  if curl --silent --fail "$HERMIT_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then cat "$log"; exit 1; fi
if [ "${HERMIT_ROUTING_ONLY:-}" = 1 ]; then
  node integration/routing.mjs
else
  npm test --prefix integration
  HERMIT_SHUTDOWN_BINARY="$binary" node integration/shutdown.mjs
fi
