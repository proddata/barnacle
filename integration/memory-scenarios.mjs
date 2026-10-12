// Realistic workloads against Barnacle running under a real cgroup memory limit.
//
// For every scenario and memory tier this starts a fresh Barnacle container with
// `docker run --memory`, drives it with closed-loop clients for a few seconds,
// then reports status mix, latency, peak cgroup memory and whether the kernel
// OOM-killed it. It exists to show how the memory budget scales between tiers
// and to catch a change that lets request or result memory escape the budget.
//
//   node integration/memory-scenarios.mjs                 # all scenarios, all tiers
//   node integration/memory-scenarios.mjs --tiers 256     # one tier
//   node integration/memory-scenarios.mjs --scenario mixed-production --seconds 10
//   node integration/memory-scenarios.mjs --check         # exit 1 if an expectation fails
//   node integration/memory-scenarios.mjs --json out.json
import { execFile } from 'node:child_process';
import { writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
import { Client, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const exec = promisify(execFile);
const repo = fileURLToPath(new URL('..', import.meta.url));
const args = process.argv.slice(2);
const option = (name, fallback) => { const i = args.indexOf(`--${name}`); return i < 0 ? fallback : args[i + 1]; };
const check = args.includes('--check');
const seconds = Number(option('seconds', 6));
const onlyScenario = option('scenario');
const jsonOut = option('json');
// The defaults match the packaged service: GOMEMLIMIT at 5/8 of the limit and a
// budget Barnacle derives itself (GOMEMLIMIT less 64 MiB: 3/8 of 256 MiB, 1/2 of 512 MiB). Both can be
// changed to compare profiles, for example a closer-to-the-limit one:
//   --gomem-fraction 0.75 --budget-fraction 0.5
const goMemFraction = Number(option('gomem-fraction', 0.625));
const budgetFraction = option('budget-fraction') === undefined ? undefined : Number(option('budget-fraction'));

// `slots` is BARNACLE_MAX_HTTP_QUERIES; the 512 MiB tier is shown with the default
// and with doubled concurrency, since a bigger node can usually serve more clients.
const allTiers = [
  { name: '256 MiB', limit: 256, slots: 8 },
  { name: '512 MiB', limit: 512, slots: 8 },
  { name: '512 MiB, 16 slots', limit: 512, slots: 16 },
];
const wantedTiers = option('tiers', '').split(',').filter(Boolean).map(Number);
const tiers = allTiers.filter((tier) => !wantedTiers.length || wantedTiers.includes(tier.limit));

const database = 'postgres://app:app_pw@pg:5432/app';
const sql = (query, params = [], extra = {}) => ({ body: { query, params }, ...extra });
const batch = (queries) => ({ body: { queries } });
const text = (bytes) => 'x'.repeat(bytes);

// Each scenario: groups of closed-loop clients. `expect.ok` is the lowest
// acceptable fraction of 2xx responses; `expect.statuses` lists tolerated errors.
const scenarios = [
  { name: 'oltp-point-lookups', about: 'API traffic: primary-key lookups',
    clients: [{ count: 8, request: (n) => sql('select * from orders where id = $1', [1 + (n * 7919) % 200000]) }],
    expect: { ok: 1 } },
  { name: 'dashboard-aggregates', about: 'dashboard refresh: 4 aggregates in one transactional batch',
    clients: [{ count: 8, request: () => batch([
      { query: "select date_trunc('day', created_at) d, count(*) from orders group by 1 order by 1 desc limit 90" },
      { query: 'select status, sum(total) from orders group by status' },
      { query: 'select customer_id, count(*) from orders group by 1 order by 2 desc limit 20' },
      { query: 'select count(*) from events' }]) }],
    expect: { ok: 1 } },
  { name: 'list-pages', about: 'admin table pages: 1,000 rows of orders per request',
    clients: [{ count: 8, request: (n) => sql('select * from orders order by id limit 1000 offset $1', [(n % 100) * 1000]) }],
    expect: { ok: 1 } },
  { name: 'json-documents', about: 'document reads: 2,000 jsonb rows of about 1 KiB',
    clients: [{ count: 8, request: () => sql('select id, doc from events order by id limit 2000') }],
    expect: { ok: 1 } },
  { name: 'array-columns', about: 'tag and number array columns: 5,000 rows',
    clients: [{ count: 8, request: () => sql('select id, tags, nums from items order by id limit 5000') }],
    expect: { ok: 1 } },
  { name: 'mixed-production', about: '5 API clients, 2 dashboards, 1 buffered 8k-row export at the same time',
    clients: [
      { count: 5, request: (n) => sql('select * from orders where id = $1', [1 + (n * 104729) % 200000]) },
      { count: 2, request: () => sql("select date_trunc('day', created_at) d, count(*) from orders group by 1 order by 1 desc limit 90") },
      { count: 1, request: () => sql('select * from orders order by id limit 8000') }],
    expect: { ok: 0.95 } },
  { name: 'bulk-insert', about: 'bulk load: 4 clients each insert 2,000 rows (about 2 MiB of JSON)',
    clients: [{ count: 4, request: () => sql('insert into bulk_load(payload) select unnest($1::text[])', [Array.from({ length: 2000 }, () => text(1000))]) }],
    expect: { ok: 1 } },
  { name: 'large-request-bodies', about: '4 clients send the largest allowed body: one 9.9 MB string parameter',
    clients: [{ count: 4, request: () => sql('select length($1::text)', [text(9_900_000)]) }],
    expect: { ok: 0.4, okAt512: 0.9, statuses: [503] } },
  { name: 'streaming-export', about: '4 streamed 200k-row exports (about 40 MiB each) beside 4 API clients',
    clients: [
      { count: 4, stream: true, request: () => sql('select * from orders', [], { headers: { 'Barnacle-Response-Mode': 'streaming' } }) },
      { count: 4, request: (n) => sql('select * from orders where id = $1', [1 + (n * 7919) % 200000]) }],
    expect: { ok: 1 } },
  { name: 'streaming-array-rows', about: '4 clients stream rows holding a 100k-element array (about 700 KB each, converted whole)',
    clients: [{ count: 4, stream: true, request: () => sql('select array_agg(i) from generate_series(1, 100000) i', [], { headers: { 'Barnacle-Response-Mode': 'streaming' } }) }],
    expect: { ok: 0.95 } },
  { name: 'streaming-huge-array-rows', about: 'abuse: 4 clients stream a 6 MB array row each (about 10 to 30x its size while converted)',
    clients: [{ count: 4, stream: true, request: () => sql('select array_agg(i) from generate_series(1, 900000) i', [], { headers: { 'Barnacle-Response-Mode': 'streaming' } }) }],
    expect: { ok: 0, statuses: [503, 413], noCrash: true } },
  { name: 'websocket-sessions', about: '20 WebSocket sessions running queries beside 8 list-page clients (the 32 upstream connections are shared)',
    websocket: 20,
    clients: [{ count: 8, request: (n) => sql('select * from orders order by id limit 1000 offset $1', [(n % 100) * 1000]) }],
    expect: { ok: 0.95 } },
  { name: 'overload-huge-results', about: 'abuse: 8 clients ask for 5 million rows; must be refused, never crash',
    clients: [{ count: 8, request: () => sql('select i from generate_series(1, 5000000) i') }],
    expect: { ok: 0, statuses: [413, 503], noCrash: true } },
  { name: 'overload-array-heavy', about: 'abuse: 8 clients each buffer a text[] column of 5 million rows',
    clients: [{ count: 8, request: () => sql("select array[i::text, 'a', 'b'] from generate_series(1, 5000000) i") }],
    expect: { ok: 0, statuses: [413, 503], noCrash: true } },
].filter((s) => !onlyScenario || s.name === onlyScenario);

const run = (command, commandArgs, options = {}) => exec(command, commandArgs, { maxBuffer: 64 << 20, ...options });
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
// A hung promise with no other work would let Node exit silently; a timer both
// bounds the wait and keeps the process alive.
const within = (promise, ms, what) => {
  let timer;
  return Promise.race([promise, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${what} timed out`)), ms); })]).finally(() => clearTimeout(timer));
};
const freePort = () => new Promise((resolve, reject) => {
  const server = createServer();
  server.once('error', reject);
  server.listen(0, '127.0.0.1', () => { const { port } = server.address(); server.close(() => resolve(port)); });
});

const suffix = process.pid;
const network = `barnacle-mem-${suffix}`;
const pg = `barnacle-mem-pg-${suffix}`;
const image = 'barnacle-mem-bench';

async function startDatabase() {
  await run('docker', ['network', 'create', network]);
  await run('docker', ['run', '-d', '--name', pg, '--network', network, '--network-alias', 'pg',
    '-e', 'POSTGRES_USER=app', '-e', 'POSTGRES_PASSWORD=app_pw', '-e', 'POSTGRES_DB=app',
    'postgres:17-alpine', '-c', 'max_connections=200', '-c', 'shared_buffers=128MB']);
  for (let i = 0; i < 60; i++) {
    try { await run('docker', ['exec', pg, 'pg_isready', '-U', 'app', '-d', 'app']); break; } catch { await sleep(1000); }
  }
  await sleep(2000);
  const seed = `
    create table orders as select i id, i % 5000 customer_id, (array['new','paid','shipped','returned'])[1 + i % 4] status,
      (random() * 500)::numeric(10,2) total, now() - (i || ' minutes')::interval created_at, repeat('n', 60) notes, md5(i::text) ref, (i % 7)::int qty
      from generate_series(1, 200000) i;
    alter table orders add primary key (id);
    create table events as select i id, jsonb_build_object('user', i % 1000, 'type', 'click', 'payload', repeat('x', 700), 'tags', jsonb_build_array('a','b','c'), 'n', i) doc
      from generate_series(1, 20000) i;
    create table items as select i id, array['red','blue','x' || (i % 100)] tags, array[i, i + 1, i + 2]::int[] nums from generate_series(1, 50000) i;
    create table bulk_load(id bigserial primary key, payload text);
    analyze;`;
  await run('docker', ['exec', pg, 'psql', '-U', 'app', '-d', 'app', '-v', 'ON_ERROR_STOP=1', '-c', seed]);
}

async function startGateway(tier) {
  const name = `barnacle-mem-gw-${suffix}`;
  const port = await freePort();
  const goLimit = Math.floor(tier.limit * goMemFraction);
  const budget = budgetFraction === undefined ? [] : ['-e', `BARNACLE_HTTP_MAX_MEMORY_MIB=${Math.floor(tier.limit * budgetFraction)}`];
  await run('docker', ['run', '-d', '--name', name, '--network', network,
    '--memory', `${tier.limit}m`, '--memory-swap', `${tier.limit}m`,
    '-p', `127.0.0.1:${port}:8080`,
    '-e', 'BARNACLE_PG_ADDR=pg:5432', '-e', 'BARNACLE_PG_SSLMODE=disable', '-e', `GOMEMLIMIT=${goLimit}MiB`,
    '-e', `BARNACLE_MAX_HTTP_QUERIES=${tier.slots}`, ...budget, image]);
  const base = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 40; i++) {
    try { if ((await fetch(`${base}/healthz`)).ok) return { name, base, port }; } catch { /* starting */ }
    await sleep(250);
  }
  throw new Error('gateway did not start');
}

async function budgetMiB(gateway) {
  const { stdout, stderr } = await run('docker', ['logs', gateway.name]);
  const match = /buffered HTTP result memory budget.*?mib=(\d+)/.exec(stdout + stderr);
  return match ? Number(match[1]) : undefined;
}

async function cgroupPeakMiB(gateway) {
  for (const file of ['memory.peak', 'memory.max_usage_in_bytes']) {
    try {
      const { stdout } = await run('docker', ['exec', gateway.name, 'cat', `/sys/fs/cgroup/${file}`]);
      if (Number(stdout) > 0) return Number(stdout) / 2 ** 20;
    } catch { /* try the next name or a stopped container */ }
  }
  return undefined;
}

async function containerState(gateway) {
  const { stdout } = await run('docker', ['inspect', '-f', '{{.State.OOMKilled}} {{.State.Running}} {{.RestartCount}}', gateway.name]);
  const [oom, running] = stdout.trim().split(' ');
  return { oomKilled: oom === 'true', running: running === 'true' };
}

function percentile(sorted, p) { return sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))] : 0; }

async function drive(gateway, scenario) {
  const deadline = Date.now() + seconds * 1000;
  const statuses = new Map();
  const latencies = [];
  let bytes = 0;
  let counter = 0;
  const record = (status, ms) => { statuses.set(status, (statuses.get(status) ?? 0) + 1); latencies.push(ms); };

  const clients = scenario.clients.flatMap((group) => Array.from({ length: group.count }, () => async () => {
    while (Date.now() < deadline) {
      const { body, headers } = group.request(counter++);
      const started = performance.now();
      try {
        const response = await fetch(`${gateway.base}/sql`, {
          method: 'POST', signal: AbortSignal.timeout(60_000),
          headers: { 'Content-Type': 'application/json', 'Neon-Connection-String': database, ...headers },
          body: JSON.stringify(body),
        });
        const reader = response.body.getReader();
        for (;;) { const { done, value } = await reader.read(); if (done) break; bytes += value.length; }
        record(response.status, performance.now() - started);
      } catch (error) {
        record(error.name === 'TimeoutError' ? 'timeout' : 'connection error', performance.now() - started);
        await sleep(200);
      }
    }
  }));

  const sessions = Array.from({ length: scenario.websocket ?? 0 }, () => async () => {
    neonConfig.webSocketConstructor = WebSocket;
    neonConfig.useSecureWebSocket = false;
    neonConfig.wsProxy = () => `127.0.0.1:${gateway.port}/v2`;
    neonConfig.pipelineConnect = false;
    const client = new Client(database);
    client.on('error', () => {});
    try {
      await within(client.connect(), 15_000, 'WebSocket connect');
      while (Date.now() < deadline) {
        const started = performance.now();
        try { await within(client.query('select * from orders order by id limit 100'), 15_000, 'WebSocket query'); record('ws 200', performance.now() - started); }
        catch { record('ws error', performance.now() - started); break; }
      }
    } catch { record('ws connect error', 0); } finally { await within(client.end(), 5_000, 'WebSocket close').catch(() => {}); }
  });

  await Promise.all([...clients, ...sessions].map((client) => client()));
  const sorted = latencies.sort((a, b) => a - b);
  const total = [...statuses.values()].reduce((a, b) => a + b, 0);
  const ok = [...statuses].filter(([status]) => status === 200 || status === 'ws 200').reduce((a, [, n]) => a + n, 0);
  return { statuses: Object.fromEntries(statuses), total, ok, okRatio: total ? ok / total : 0, p50: percentile(sorted, 0.5), p95: percentile(sorted, 0.95), mbPerSecond: bytes / 2 ** 20 / seconds };
}

function verdict(scenario, result, state, peak, limit) {
  const problems = [];
  if (state.oomKilled || !state.running) problems.push('gateway was OOM-killed or stopped');
  if (peak && peak > 0.85 * limit) problems.push(`peak memory ${peak.toFixed(0)} MiB is over 85% of the ${limit} MiB limit`);
  const wantOk = limit >= 512 ? (scenario.expect.okAt512 ?? scenario.expect.ok) : scenario.expect.ok;
  if (result.okRatio < wantOk) problems.push(`only ${(result.okRatio * 100).toFixed(0)}% succeeded, expected at least ${wantOk * 100}%`);
  const allowed = new Set([200, 'ws 200', ...(scenario.expect.statuses ?? [])]);
  const unexpected = Object.keys(result.statuses).filter((status) => !allowed.has(Number.isNaN(Number(status)) ? status : Number(status)));
  if (unexpected.length) problems.push(`unexpected outcomes: ${unexpected.join(', ')}`);
  return problems;
}

const rows = [];
let failed = false;
try {
  await run('docker', ['build', '-q', '-t', image, repo]);
  await startDatabase();
  for (const scenario of scenarios) {
    for (const tier of tiers) {
      const gateway = await startGateway(tier);
      let result;
      try {
        result = await drive(gateway, scenario);
        const state = await containerState(gateway);
        const peak = await cgroupPeakMiB(gateway);
        const problems = verdict(scenario, result, state, peak, tier.limit);
        const row = { scenario: scenario.name, tier: tier.name, budgetMiB: await budgetMiB(gateway), peakMiB: peak, ...result, oomKilled: state.oomKilled, problems };
        rows.push(row);
        if (problems.length) failed = true;
        const outcomes = Object.entries(result.statuses).map(([status, n]) => `${status}:${n}`).join(' ');
        console.log(`${scenario.name.padEnd(24)} ${tier.name.padEnd(18)} budget ${String(row.budgetMiB ?? '?').padStart(3)} MiB  peak ${peak ? peak.toFixed(0).padStart(3) : '  ?'}/${tier.limit} MiB  ` +
          `p50 ${result.p50.toFixed(0).padStart(5)} ms  p95 ${result.p95.toFixed(0).padStart(6)} ms  ${result.mbPerSecond.toFixed(1).padStart(6)} MiB/s  ${outcomes}${state.oomKilled ? '  OOM-KILLED' : ''}`);
        for (const problem of problems) console.log(`    ! ${problem}`);
      } finally {
        await run('docker', ['rm', '-f', gateway.name]).catch(() => {});
      }
    }
  }
} finally {
  await run('docker', ['rm', '-f', pg]).catch(() => {});
  await run('docker', ['network', 'rm', network]).catch(() => {});
}
if (jsonOut) await writeFile(jsonOut, JSON.stringify(rows, null, 2));
console.log(`\nProfile: GOMEMLIMIT ${goMemFraction * 100}% of the limit, budget ${budgetFraction === undefined ? 'derived (GOMEMLIMIT - 64 MiB)' : `${budgetFraction * 100}%`} of the limit.`);
console.log(failed ? '\nSome expectations failed (see "!" lines).' : '\nAll expectations met.');
if (check && failed) process.exit(1);
