#!/bin/sh
set -eu

: "${BARNACLE_PG_ADDR:?set BARNACLE_PG_ADDR}"
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL}"
binary=${1:?usage: integration/release-smoke.sh /path/to/barnacle}
test -x "$binary" || { echo "binary is not executable: $binary" >&2; exit 1; }

BARNACLE_BASE_URL=${BARNACLE_BASE_URL:-http://127.0.0.1:18080}
BARNACLE_LISTEN=${BARNACLE_LISTEN:-127.0.0.1:18080}
export BARNACLE_BASE_URL BARNACLE_LISTEN
log=$(mktemp)
cleanup() {
    kill "$barnacle_pid" 2>/dev/null || true
    wait "$barnacle_pid" 2>/dev/null || true
    rm -f "$log"
}
trap cleanup EXIT HUP INT TERM

"$binary" >"$log" 2>&1 &
barnacle_pid=$!
ready=0
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl --silent --fail "$BARNACLE_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
    if ! kill -0 "$barnacle_pid" 2>/dev/null; then break; fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    cat "$log"
    echo 'release artifact did not become ready' >&2
    exit 1
fi
if ! node integration/release-smoke.mjs; then
    cat "$log"
    exit 1
fi

kill "$barnacle_pid" 2>/dev/null || true
wait "$barnacle_pid" 2>/dev/null || true
BARNACLE_PG_ADDR=127.0.0.1:1
BARNACLE_BASE_URL=http://127.0.0.1:18082
BARNACLE_LISTEN=127.0.0.1:18082
export BARNACLE_PG_ADDR BARNACLE_BASE_URL BARNACLE_LISTEN
"$binary" >"$log" 2>&1 &
barnacle_pid=$!
ready=0
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl --silent --fail "$BARNACLE_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
    if ! kill -0 "$barnacle_pid" 2>/dev/null; then break; fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    cat "$log"
    echo 'release artifact did not become ready with unavailable upstream' >&2
    exit 1
fi
if ! BARNACLE_SMOKE_UNREACHABLE=1 node integration/release-smoke.mjs; then
    cat "$log"
    exit 1
fi
