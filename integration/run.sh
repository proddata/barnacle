#!/bin/sh
set -eu

if [ -z "${BARNACLE_PG_ADDR:-}" ] && [ -z "${BARNACLE_PG_ALLOWED_ADDRS:-}" ]; then
  echo 'set BARNACLE_PG_ADDR or BARNACLE_PG_ALLOWED_ADDRS' >&2
  exit 1
fi
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL}"
BARNACLE_BASE_URL=${BARNACLE_BASE_URL:-http://127.0.0.1:8080}
BARNACLE_LISTEN=${BARNACLE_LISTEN:-127.0.0.1:8080}
export BARNACLE_BASE_URL BARNACLE_LISTEN

binary="${TMPDIR:-/tmp}/barnacle-integration-$$"
log="${TMPDIR:-/tmp}/barnacle-integration-$$.log"
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
  if curl --silent --fail "$BARNACLE_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then cat "$log"; exit 1; fi
if [ "${BARNACLE_ROUTING_ONLY:-}" = 1 ]; then
  node integration/routing.mjs
else
  BARNACLE_BATCH_BINARY="$binary" BARNACLE_WS_IDLE_BINARY="$binary" npm test --prefix integration
  BARNACLE_SHUTDOWN_BINARY="$binary" node integration/shutdown.mjs
  BARNACLE_TIMEOUT_BINARY="$binary" node integration/http-timeouts.mjs
  if [ "${BARNACLE_BROWSER_CORS:-}" = 1 ]; then
    BARNACLE_BROWSER_BINARY="$binary" node integration/browser-cors.mjs
  fi
fi
