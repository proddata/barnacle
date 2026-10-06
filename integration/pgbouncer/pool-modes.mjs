import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:18080';
neonConfig.fetchEndpoint = `${base}/sql`;
const endpoint = new URL(base);
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;
const password = 'barnacle_dev_password';
const url = (mode, name) => {
  const value = new URL(`postgres://barnacle:${password}@pgbouncer_${mode}:6432/barnacle`);
  if (name) value.searchParams.set('application_name', name);
  return value.toString();
};
const observer = neon(url('session'));

async function activeCount(name) {
  const rows = await observer.query(
    'select count(*)::int as count from pg_stat_activity where application_name = $1 and state = $2',
    [name, 'active'],
  );
  return rows[0].count;
}
async function waitUntil(check, description) {
  const deadline = Date.now() + 8000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await delay(50);
  }
  assert.fail(description);
}
async function rawQuery(mode, query, headers = {}, signal) {
  return fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': url(mode), ...headers },
    body: JSON.stringify(query),
    signal,
  });
}

for (const mode of ['session', 'transaction', 'statement']) {
  const sql = neon(url(mode));
  const rows = await sql`select ${mode}::text as mode, ${7}::int as value`;
  assert.deepEqual(rows.map(({ mode: resultMode, value }) => [resultMode, value]), [[mode, 7]]);
  const types = await sql.query(
    'select $1::jsonb as document, $2::int4[] as numbers, $3::bytea as bytes',
    [{ enabled: true }, [1, null, 3], Buffer.from([0, 255])],
  );
  assert.deepEqual(types[0].document, { enabled: true });
  assert.deepEqual(types[0].numbers, [1, null, 3]);
  assert.deepEqual(types[0].bytes, Buffer.from([0, 255]));
  const raw = await rawQuery(mode, { query: 'select $1::bytea as value', params: ['\\x0123'] }, { 'Neon-Raw-Text-Output': 'true' });
  const rawBody = await raw.json();
  assert.equal(raw.status, 200, `${mode} raw query: ${JSON.stringify(rawBody)}`);
  assert.deepEqual(rawBody.rows[0], { value: '\\x0123' });

  const batch = { queries: [
    { query: 'select current_setting(\'transaction_read_only\') as readonly' },
    { query: 'select $1::int as value', params: [11] },
  ] };
  const headers = { 'Neon-Batch-Read-Only': 'true', 'Neon-Batch-Isolation-Level': 'serializable' };
  const response = await rawQuery(mode, batch, headers);
  const body = await response.json();
  if (mode === 'statement') {
    assert.notEqual(response.status, 200, 'statement pooling must reject multi-statement transactions');
    assert.match(body.message, /transaction|statement pooling/i);
  } else {
    assert.equal(response.status, 200, `${mode} batch: ${JSON.stringify(body)}`);
    assert.equal(body.results[0].rows[0].readonly, 'on');
    assert.equal(body.results[1].rows[0].value, 11);
    const [driverResult] = await sql.transaction([
      sql`select current_setting('transaction_read_only') as readonly, ${13}::int as value`,
    ], { readOnly: true, isolationLevel: 'Serializable' });
    assert.equal(driverResult[0].readonly, 'on');
    assert.equal(driverResult[0].value, 13);
  }

  const name = `barnacle_pgbouncer_cancel_${mode}_${process.pid}`;
  const controller = new AbortController();
  const request = fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': url(mode, name) },
    body: JSON.stringify({ query: 'select pg_sleep(20)' }),
    signal: controller.signal,
  });
  try {
    await waitUntil(async () => await activeCount(name) === 1, `${mode} query did not start`);
    controller.abort();
    await assert.rejects(request, (error) => error.name === 'AbortError');
    await waitUntil(async () => await activeCount(name) === 0, `${mode} query stayed active`);
  } finally {
    controller.abort();
    await request.catch(() => {});
  }
  console.log(`${mode}: HTTP single, raw text, batch, and cancellation passed`);
}

const client = new Client(url('session'));
await client.connect();
try {
  await client.query("set application_name = 'barnacle_pgbouncer_session_state'");
  const state = await client.query("select current_setting('application_name') as name");
  assert.equal(state.rows[0].name, 'barnacle_pgbouncer_session_state');
  await client.query('prepare barnacle_pooltest(int) as select $1::int as value');
  const prepared = await client.query('execute barnacle_pooltest(19)');
  assert.equal(prepared.rows[0].value, 19);
} finally {
  await client.end();
}
console.log('session: WebSocket SET and SQL PREPARE passed');
