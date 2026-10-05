import assert from 'node:assert/strict';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { spawn } from 'node:child_process';
import { createServer } from 'node:net';
import { Client, Pool, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.HERMIT_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
const endpoint = new URL(base);
const wsURL = `${endpoint.protocol === 'https:' ? 'wss:' : 'ws:'}//${endpoint.host}/v2`;

neonConfig.webSocketConstructor = WebSocket;
neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;
neonConfig.poolQueryViaFetch = false;
const observer = neon(databaseUrl);
let sequence = 0;
function unique(prefix) { return `hermit_${prefix}_${process.pid}_${++sequence}`; }
function namedUrl(name) {
  const url = new URL(databaseUrl);
  url.searchParams.set('application_name', name);
  return url.toString();
}
async function activityCount(name) {
  const rows = await observer.query('select count(*)::int as count from pg_stat_activity where application_name = $1', [name]);
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

async function within(promise, label) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${label} timed out`)), 5000); }),
    ]);
  } finally { clearTimeout(timer); }
}

test('WebSocket responds to ping and completes the close handshake', async () => {
  const socket = new WebSocket(wsURL, { handshakeTimeout: 5000 });
  try {
    await within(new Promise((resolve, reject) => {
      socket.once('open', resolve);
      socket.once('error', reject);
    }), 'WebSocket upgrade');
    const pong = new Promise((resolve, reject) => {
      socket.once('pong', resolve);
      socket.once('error', reject);
    });
    socket.ping('hermit');
    assert.deepEqual(await within(pong, 'pong'), Buffer.from('hermit'));
    const closed = new Promise((resolve, reject) => {
      socket.once('close', (code, reason) => resolve({ code, reason: reason.toString() }));
      socket.once('error', reject);
    });
    socket.close(1000, 'done');
    assert.deepEqual(await within(closed, 'close handshake'), { code: 1000, reason: 'done' });
  } finally {
    socket.terminate();
  }
});

test('large WebSocket query frame reaches PostgreSQL intact', async () => {
  const client = new Client(databaseUrl);
  await client.connect();
  try {
    const result = await client.query(`select 42::int as value /*${'x'.repeat(100_000)}*/`);
    assert.equal(result.rows[0].value, 42);
  } finally {
    await client.end();
  }
});

test('clean WebSocket close releases its PostgreSQL session', async () => {
  const name = unique('ws_close');
  const client = new Client(namedUrl(name));
  client.on('error', () => {});
  await client.connect();
  await client.query('select 1'); // PgBouncer assigns a backend on the first query.
  assert.equal(await activityCount(name), 1);
  const socket = client.connection.stream.ws;
  const closed = new Promise((resolve) => socket.once('close', resolve));
  socket.close(1000, 'done');
  await within(closed, 'client close');
  await waitUntil(async () => await activityCount(name) === 0, 'clean close retained a PostgreSQL backend');
});

test('Client.end releases its PostgreSQL backend', async () => {
  const name = unique('client_end');
  const client = new Client(namedUrl(name));
  await client.connect();
  await client.query('select 1'); // PgBouncer assigns a backend on the first query.
  assert.equal(await activityCount(name), 1);
  await client.end();
  await waitUntil(async () => await activityCount(name) === 0, 'Client.end retained a PostgreSQL backend');
});

test('Client keeps transaction state across query calls and commits atomically', async () => {
  const table = unique('tx');
  await observer.query(`create table ${table} (value int)`);
  const client = new Client(databaseUrl);
  try {
    await client.connect();
    await client.query('BEGIN');
    await client.query(`insert into ${table} values (42)`);
    assert.equal((await client.query(`select count(*)::int as count from ${table}`)).rows[0].count, 1);
    assert.equal((await observer.query(`select count(*)::int as count from ${table}`))[0].count, 0);
    await client.query('COMMIT');
    assert.equal((await observer.query(`select value from ${table}`))[0].value, 42);
    await client.query('BEGIN');
    await client.query(`insert into ${table} values (99)`);
    await client.query('ROLLBACK');
    assert.deepEqual((await observer.query(`select value from ${table}`)).map(row => row.value), [42]);
  } finally {
    await client.end().catch(() => {});
    await observer.query(`drop table ${table}`);
  }
});

test('Client preserves SET, named prepared statements, and cursors', async () => {
  const client = new Client(databaseUrl);
  await client.connect();
  try {
    await client.query("SET TIME ZONE 'Pacific/Honolulu'");
    assert.equal((await client.query('SHOW TIME ZONE')).rows[0].TimeZone, 'Pacific/Honolulu');
    const query = { name: unique('prepared'), text: 'select $1::int + 2 as value', values: [3] };
    assert.equal((await client.query(query)).rows[0].value, 5);
    query.values = [8];
    assert.equal((await client.query(query)).rows[0].value, 10);
    await client.query('BEGIN');
    await client.query('DECLARE hermit_cursor CURSOR FOR SELECT generate_series(1, 3) AS value');
    assert.deepEqual((await client.query('FETCH 2 FROM hermit_cursor')).rows.map(row => row.value), [1, 2]);
    assert.deepEqual((await client.query('FETCH ALL FROM hermit_cursor')).rows.map(row => row.value), [3]);
    await client.query('COMMIT');
  } finally {
    await client.end();
  }
});

test('Pool reuses its backend and session state, then releases it on end', async () => {
  const name = unique('pool');
  const pool = new Pool({ connectionString: namedUrl(name), max: 1 });
  try {
    const first = await pool.connect();
    let pid;
    try {
      pid = (await first.query('select pg_backend_pid() as pid')).rows[0].pid;
      await first.query("SET TIME ZONE 'Pacific/Honolulu'");
    } finally { first.release(); }
    const second = await pool.connect();
    try {
      assert.equal((await second.query('select pg_backend_pid() as pid')).rows[0].pid, pid);
      assert.equal((await second.query('SHOW TIME ZONE')).rows[0].TimeZone, 'Pacific/Honolulu');
      await second.query('RESET TIME ZONE');
    } finally { second.release(); }
    assert.equal(await activityCount(name), 1);
  } finally { await pool.end(); }
  await waitUntil(async () => await activityCount(name) === 0, 'Pool.end retained a PostgreSQL backend');
});

test('LISTEN receives NOTIFY across WebSocket sessions', async () => {
  const channel = unique('notify');
  const listener = new Client(databaseUrl);
  await listener.connect();
  try {
    await listener.query(`LISTEN ${channel}`);
    const notification = within(new Promise(resolve => listener.once('notification', resolve)), 'NOTIFY');
    await observer.query('select pg_notify($1, $2)', [channel, 'hello']);
    const received = await notification;
    assert.equal(received.channel, channel);
    assert.equal(received.payload, 'hello');
    await listener.query(`UNLISTEN ${channel}`);
  } finally { await listener.end(); }
});

test('published Client has no COPY stream API and stays usable after COPY', async () => {
  const table = unique('copy');
  await observer.query(`create table ${table} (value int)`);
  const client = new Client(databaseUrl);
  try {
    await client.connect();
    await assert.rejects(within(client.query(`COPY ${table} FROM STDIN`), 'COPY FROM STDIN'), /No source stream defined/);
    const copyOut = await within(client.query('COPY (SELECT 42 AS value) TO STDOUT'), 'COPY TO STDOUT');
    assert.deepEqual(copyOut.rows, []); // Published Client.query discards CopyData frames.
    assert.equal((await client.query('select 1::int as value')).rows[0].value, 1);
    assert.equal((await observer.query(`select count(*)::int as count from ${table}`))[0].count, 0);
  } finally {
    await client.end().catch(() => {});
    await observer.query(`drop table ${table}`);
  }
});

test('idle timeout disconnects Client and releases its PostgreSQL backend', { skip: !process.env.HERMIT_WS_IDLE_BINARY && !process.env.HERMIT_WS_IDLE_BASE_URL }, async () => {
  let child;
  let idleBase = process.env.HERMIT_WS_IDLE_BASE_URL;
  if (!idleBase) {
    const server = createServer();
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    const port = server.address().port;
    await new Promise(resolve => server.close(resolve));
    idleBase = `http://127.0.0.1:${port}`;
    child = spawn(process.env.HERMIT_WS_IDLE_BINARY, [], {
      env: { ...process.env, HERMIT_LISTEN: `127.0.0.1:${port}`, HERMIT_WS_IDLE_TIMEOUT: '700ms' },
      stdio: 'ignore',
    });
  }
  const idleEndpoint = new URL(idleBase);
  const originalProxy = neonConfig.wsProxy;
  const originalSecure = neonConfig.useSecureWebSocket;
  const name = unique('idle');
  let client;
  try {
    await waitUntil(async () => {
      if (child && child.exitCode !== null) throw new Error(`idle gateway exited: ${child.exitCode}`);
      try { return (await fetch(`${idleBase}/healthz`)).ok; } catch { return false; }
    }, 'idle gateway did not start');
    neonConfig.wsProxy = () => `${idleEndpoint.host}/v2`;
    neonConfig.useSecureWebSocket = idleEndpoint.protocol === 'https:';
    client = new Client(namedUrl(name));
    client.on('error', () => {}); // Idle disconnect is expected.
    const ended = new Promise(resolve => client.once('end', resolve));
    await within(client.connect(), 'idle gateway Client connect');
    assert.equal(await activityCount(name), 1);
    await within(ended, 'idle WebSocket disconnect');
    await waitUntil(async () => await activityCount(name) === 0, 'idle timeout retained a PostgreSQL backend');
  } finally {
    neonConfig.wsProxy = originalProxy;
    neonConfig.useSecureWebSocket = originalSecure;
    client?.connection?.stream?.ws?.terminate();
    if (child) {
      child.kill();
      if (child.exitCode === null) await new Promise(resolve => child.once('exit', resolve));
    }
  }
});
