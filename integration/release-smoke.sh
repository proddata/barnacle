#!/bin/sh
set -eu

: "${HERMIT_PG_ADDR:?set HERMIT_PG_ADDR}"
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL}"
binary=${1:?usage: integration/release-smoke.sh /path/to/hermit}
test -x "$binary" || { echo "binary is not executable: $binary" >&2; exit 1; }

HERMIT_BASE_URL=${HERMIT_BASE_URL:-http://127.0.0.1:18080}
HERMIT_LISTEN=${HERMIT_LISTEN:-127.0.0.1:18080}
export HERMIT_BASE_URL HERMIT_LISTEN
log=$(mktemp)
cleanup() {
    kill "$hermit_pid" 2>/dev/null || true
    wait "$hermit_pid" 2>/dev/null || true
    rm -f "$log"
}
trap cleanup EXIT HUP INT TERM

"$binary" >"$log" 2>&1 &
hermit_pid=$!
ready=0
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl --silent --fail "$HERMIT_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
    if ! kill -0 "$hermit_pid" 2>/dev/null; then break; fi
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

kill "$hermit_pid" 2>/dev/null || true
wait "$hermit_pid" 2>/dev/null || true
HERMIT_PG_ADDR=127.0.0.1:1
HERMIT_BASE_URL=http://127.0.0.1:18082
HERMIT_LISTEN=127.0.0.1:18082
export HERMIT_PG_ADDR HERMIT_BASE_URL HERMIT_LISTEN
"$binary" >"$log" 2>&1 &
hermit_pid=$!
ready=0
for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    if curl --silent --fail "$HERMIT_BASE_URL/healthz" >/dev/null; then ready=1; break; fi
    if ! kill -0 "$hermit_pid" 2>/dev/null; then break; fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    cat "$log"
    echo 'release artifact did not become ready with unavailable upstream' >&2
    exit 1
fi
if ! HERMIT_SMOKE_UNREACHABLE=1 node integration/release-smoke.mjs; then
    cat "$log"
    exit 1
fi
