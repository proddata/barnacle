import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer } from 'node:net';
import test, { after, before } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { neon, neonConfig } from '@neondatabase/serverless';

const base = process.env.HERMIT_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
neonConfig.fetchEndpoint = `${base}/sql`;
const sql = neon(databaseUrl);
const table = `hermit_batch_atomicity_${process.pid}_${Math.random().toString(36).slice(2, 10)}`;

async function batch(queries, options = {}) {
  const response = await fetch(`${options.base || base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': options.connectionString || databaseUrl },
    body: JSON.stringify({ queries: queries.map((query) => ({ query, params: [] })) }),
    signal: options.signal,
  });
  return { status: response.status, body: await response.json() };
}

async function count(label) {
  const rows = await sql.query(`select count(*)::int as count from ${table} where label = $1`, [label]);
  return rows[0].count;
}

async function waitUntil(check, message, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await delay(50);
  }
  assert.fail(message);
}

async function configuredGateway(baseEnv, overrides) {
  if (process.env[baseEnv]) {
    return { base: process.env[baseEnv], close: async () => {} };
  }
  const binary = process.env.HERMIT_BATCH_BINARY;
  if (!binary) return null;
  const reservation = createServer();
  await new Promise((resolve, reject) => reservation.listen(0, '127.0.0.1', (error) => error ? reject(error) : resolve()));
  const port = reservation.address().port;
  await new Promise((resolve) => reservation.close(resolve));
  const base = `http://127.0.0.1:${port}`;
  const child = spawn(binary, [], {
    env: { ...process.env, HERMIT_LISTEN: `127.0.0.1:${port}`, ...overrides },
    stdio: 'ignore',
  });
  const exit = new Promise((resolve) => child.once('exit', resolve));
  try {
    await waitUntil(async () => {
      try { return (await fetch(`${base}/healthz`)).ok; } catch { return false; }
    }, 'short-deadline Hermit did not start');
  } catch (error) {
    child.kill('SIGTERM');
    await exit;
    throw error;
  }
  return { base, close: async () => { child.kill('SIGTERM'); await exit; } };
}

before(async () => { await sql.query(`create table ${table} (label text primary key)`); });
after(async () => { await sql.query(`drop table if exists ${table}`); });

test('a later SQL error rolls back an earlier batch write', async () => {
  const label = 'later_error';
  const response = await batch([`insert into ${table} values ('${label}')`, 'select 1 / 0']);
  assert.equal(response.status, 400);
  assert.equal(response.body.code, '22012');
  assert.equal(await count(label), 0);
});

test('a statement timeout rolls back an earlier batch write', async () => {
  const label = 'statement_timeout';
  const response = await batch([
    `insert into ${table} values ('${label}')`,
    "select set_config('statement_timeout', '500', true)",
    'select pg_sleep(5)',
  ]);
  assert.equal(response.status, 400);
  assert.equal(response.body.code, '57014');
  assert.equal(await count(label), 0);
});

test('Hermit request deadline rolls back an earlier batch write', {
  skip: !process.env.HERMIT_BATCH_BINARY && !process.env.HERMIT_BATCH_TIMEOUT_BASE_URL,
}, async () => {
  const gateway = await configuredGateway('HERMIT_BATCH_TIMEOUT_BASE_URL', { HERMIT_QUERY_TIMEOUT: '1500ms' });
  const label = 'gateway_timeout';
  const name = `hermit_batch_deadline_${process.pid}`;
  const url = new URL(databaseUrl);
  url.searchParams.set('application_name', name);
  try {
    const request = batch([`insert into ${table} values ('${label}')`, 'select pg_sleep(5)'], {
      base: gateway.base, connectionString: url.toString(),
    });
    await waitUntil(async () => {
      const rows = await sql.query('select count(*)::int as count from pg_stat_activity where application_name = $1 and state = $2', [name, 'active']);
      return rows[0].count === 1;
    }, 'second batch query did not start before Hermit deadline');
    const response = await request;
    assert.notEqual(response.status, 200);
    await waitUntil(async () => {
      const rows = await sql.query('select count(*)::int as count from pg_stat_activity where application_name = $1', [name]);
      return rows[0].count === 0;
    }, 'timed out batch connection stayed open');
    assert.equal(await count(label), 0);
  } finally {
    await gateway.close();
  }
});

test('final JSON response limit rolls back an earlier batch write', {
  skip: !process.env.HERMIT_BATCH_BINARY && !process.env.HERMIT_BATCH_RESPONSE_BASE_URL,
}, async () => {
  const gateway = await configuredGateway('HERMIT_BATCH_RESPONSE_BASE_URL', { HERMIT_HTTP_MAX_RESPONSE_MIB: '1' });
  const label = 'response_limit';
  try {
    const response = await batch([
      `insert into ${table} values ('${label}')`,
      "select repeat('x', 2 * 1048576) as payload",
    ], { base: gateway.base });
    assert.equal(response.status, 413);
    assert.equal(response.body.code, 'HERMIT_ERROR');
    assert.equal(await count(label), 0);
  } finally {
    await gateway.close();
  }
});

test('client abort during a later batch query rolls back an earlier write', async () => {
  const label = 'client_abort';
  const name = `hermit_batch_abort_${process.pid}`;
  const url = new URL(databaseUrl);
  url.searchParams.set('application_name', name);
  const controller = new AbortController();
  const request = batch([`insert into ${table} values ('${label}')`, 'select pg_sleep(20)'], {
    connectionString: url.toString(), signal: controller.signal,
  });
  try {
    await waitUntil(async () => {
      const rows = await sql.query('select count(*)::int as count from pg_stat_activity where application_name = $1 and state = $2', [name, 'active']);
      return rows[0].count === 1;
    }, 'second batch query did not start');
    controller.abort();
    await assert.rejects(request, (error) => error.name === 'AbortError');
    await waitUntil(async () => {
      const rows = await sql.query('select count(*)::int as count from pg_stat_activity where application_name = $1', [name]);
      return rows[0].count === 0;
    }, 'aborted batch connection stayed open');
    assert.equal(await count(label), 0);
  } finally {
    controller.abort();
    await request.catch(() => {});
  }
});

test('result-limit failure rolls back an earlier batch write', async () => {
  const label = 'result_limit';
  const response = await batch([
    `insert into ${table} values ('${label}')`,
    ...Array.from({ length: 5 }, () => "select repeat('x', 1048576) as payload"),
  ]);
  assert.equal(response.status, 413);
  assert.equal(response.body.code, 'HERMIT_ERROR');
  assert.equal(await count(label), 0);
});

test('explicit transaction control follows PostgreSQL semantics inside a batch', async () => {
  for (const [label, control, expected] of [
    ['explicit_commit', 'COMMIT', 1],
    ['explicit_rollback', 'ROLLBACK', 0],
    ['explicit_begin', 'BEGIN', 0],
  ]) {
    const response = await batch([`insert into ${table} values ('${label}')`, control, 'select 1 / 0']);
    assert.equal(response.status, 400, control);
    assert.equal(response.body.code, '22012', control);
    assert.equal(await count(label), expected, control);
  }
  const label = 'rollback_then_insert';
  const response = await batch(['ROLLBACK', `insert into ${table} values ('${label}')`, 'select 1 / 0']);
  assert.equal(response.status, 400);
  assert.equal(response.body.code, '22012');
  assert.equal(await count(label), 1, 'write after explicit ROLLBACK used an implicit transaction');
});

test('multiple SQL statements in one batch element are rejected and rolled back', async () => {
  const label = 'multi_statement';
  const response = await batch([`insert into ${table} values ('${label}')`, 'select 1; select 2']);
  assert.equal(response.status, 400);
  assert.equal(response.body.code, '42601');
  assert.equal(await count(label), 0);
});

test('HTTP rejects a connection-string request for simple protocol', async () => {
  const url = new URL(databaseUrl);
  url.searchParams.set('default_query_exec_mode', 'simple_protocol');
  const response = await batch(['select 1'], { connectionString: url.toString() });
  assert.equal(response.status, 400);
  assert.equal(response.body.code, 'HERMIT_ERROR');
  assert.match(response.body.message, /unsupported PostgreSQL connection option/);
});
