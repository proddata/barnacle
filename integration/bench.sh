#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
seconds=${BENCH_SECONDS:-10}
cd "$repo_dir"

run() {
    docker compose restart barnacle >/dev/null
    ready=false
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
        if curl --silent --fail http://127.0.0.1:8080/healthz >/dev/null; then
            ready=true
            break
        fi
        sleep 1
    done
    if [ "$ready" != true ]; then
        echo 'Barnacle did not become ready for the benchmark' >&2
        exit 1
    fi
    node integration/bench.mjs "$@" --seconds="$seconds"
}

run --transport=http --connections=1 --qps=10
run --transport=http --connections=8 --qps=50
run --transport=http --connections=8 --qps=100
run --transport=http --connections=8 --qps=30 --sleep-ms=200
run --transport=http --connections=8 --qps=50 --payload-bytes=8192
run --transport=ws --connections=1 --qps=10
run --transport=ws --connections=8 --qps=50
run --transport=ws --connections=32 --qps=100
