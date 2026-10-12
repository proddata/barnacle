# Memory limits: how close to the limit

Barnacle runs under a hard memory limit (`MemoryMax`, 256 MiB in the packaged unit). Exceeding it gets the process OOM-killed, which drops every WebSocket session and query, so three settings keep it below that:

- the **cgroup limit** itself, the hard ceiling;
- **`GOMEMLIMIT`**, the point where Go's collector works harder instead of letting the heap grow;
- the **memory budget** (`BARNACLE_HTTP_MAX_MEMORY_MIB`), an estimate of live heap that `/sql` request bodies and buffered results may hold at once. Requests that do not fit get a retryable `503`, or `413` if they never could. See [SQL-over-HTTP limits](sql-http-limits.md#memory-budget).

## Decision

Keep a wide margin: **`GOMEMLIMIT` at 5/8 of the cgroup limit and the budget at `GOMEMLIMIT` less a fixed 64 MiB.** For the packaged unit that is `MemoryMax=256M`, `GOMEMLIMIT=160MiB`, and a 96 MiB budget; at 512 MiB it is `GOMEMLIMIT=320MiB` and a 256 MiB budget. Barnacle derives the budget from `GOMEMLIMIT`, so raising `MemoryMax` and `GOMEMLIMIT` together raises it. The reserve is a fixed amount rather than a share because the heavy scenarios peaked at about the budget plus 50 to 85 MiB at both sizes (the Go runtime, garbage, and relays do not grow with the limit), so a share would leave more of a larger service's memory unused. The constant is in `internal/gateway/memory.go`.

These are all safety limits. Their only visible cost is refusing a heavy request while memory is still free. An OOM kill is far worse than a refused request, and in testing a profile closer to the limit (`GOMEMLIMIT` 3/4, budget 1/2) bought nothing for ordinary traffic: every realistic workload succeeded and peaked at the same memory under every profile. It only let more of the heavy requests through (four simultaneous 10 MB uploads at 256 MiB went from about half to about three quarters succeeding), at the price of peaks near 75% of the limit instead of 62% (56% at 512 MiB), with the rest unmeasured under WebSocket and TLS load.

## Evidence

`integration/memory-scenarios.mjs` runs 14 workloads (API traffic, dashboards, list pages, JSON and array columns, bulk inserts, maximum-size bodies, streamed exports, WebSocket sessions, and abuse) in a container with a real cgroup limit, at 256 MiB, 512 MiB, and 512 MiB with 16 HTTP slots. With the chosen profile all expectations held, nothing was OOM-killed, and the worst peak was 62% of the limit at 256 MiB and 56% at 512 MiB (a first run with the budget at 3/5 of `GOMEMLIMIT`, 192 MiB at 512 MiB, peaked at 54% but refused more heavy requests). The per-workload table is in [SQL-over-HTTP limits](sql-http-limits.md#test-results). Single 6-second runs in Docker Desktop's Linux VM on macOS; differences of a few percent are noise.

## Risks accepted

- At 256 MiB the chosen profile leaves about 96 MiB (37%) for what the heap limit does not cover: goroutine stacks, the 32 WebSocket relays, TLS, and kernel socket buffers. The scenarios barely stress that (20 WebSocket sessions peaked at 46 MiB), so the margin under real traffic is unmeasured, but it is large.
- The cost estimates are rough and deliberately high. Under contention several large requests can all be refused at once rather than one finishing; they are safe to retry.
- Measurements come from macOS, not a Linux node with production traffic and TLS in front.

## What to watch

- OOM kills and restarts: `systemctl show barnacle -p NRestarts` and the kernel log; alert on any increase.
- `barnacle_limit_rejections_total{limit="http_memory"}` (the budget turning requests away) and `barnacle_sql_stream_interruptions_total`.
- `barnacle_login_failures_total`, which is not a memory signal but separates a flood of bad logins from a real load increase when memory or connection limits are hit. Steady rejections with free memory mean the budget can grow.
- Peak cgroup memory against the limit (`MemoryPeak` in `systemctl show barnacle`); sustained peaks over about 85% mean the margin is too thin.
- Before a wide rollout, rerun `node integration/memory-scenarios.mjs` on a Linux node, and add a few hundred WebSocket sessions with TLS in front.

## Loosening

If production shows refusals with plenty of memory free, move closer to the limit without a rebuild. In `/etc/sysconfig/barnacle` (or `/etc/default/barnacle`):

```sh
GOMEMLIMIT=192MiB                # 3/4 of 256 MiB
BARNACLE_HTTP_MAX_MEMORY_MIB=128 # 1/2 of 256 MiB
```

then `systemctl restart barnacle`. An explicit budget overrides the derived one. To change the defaults for every node, edit `memoryReserve` in `internal/gateway/memory.go` and `GOMEMLIMIT` in `packaging/rpm/barnacle.sysconfig` and the Compose files. To compare profiles, run the scenarios with `--gomem-fraction 0.75 --budget-fraction 0.5`.
