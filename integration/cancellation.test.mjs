import assert from 'node:assert/strict';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
const endpoint = new URL(base);
const wsProtocol = endpoint.protocol === 'https:' ? 'wss:' : 'ws:';

neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;

const observer = neon(databaseUrl);
async function activeCount(name) {
  const rows = await observer.query(
    'select count(*)::int as count from pg_stat_activity where application_name = $1 and state = $2',
    [name, 'active'],
  );
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
function namedUrl(name) {
  const url = new URL(databaseUrl);
  url.searchParams.set('application_name', name);
  return url.toString();
}

test('aborted HTTP request stops its PostgreSQL query', async () => {
  const name = `barnacle_http_cancel_${process.pid}`;
  const controller = new AbortController();
  const request = fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': namedUrl(name) },
    body: JSON.stringify({ query: 'select pg_sleep(20)' }),
    signal: controller.signal,
  });
  try {
    await waitUntil(async () => await activeCount(name) === 1, 'HTTP query did not start');
    controller.abort();
    await assert.rejects(request, (error) => error.name === 'AbortError');
    await waitUntil(async () => await activeCount(name) === 0, 'aborted HTTP query stayed active');
  } finally {
    controller.abort();
    await request.catch(() => {});
  }
});

test('PostgreSQL CancelRequest crosses a second WebSocket connection', async () => {
  const name = `barnacle_ws_cancel_${process.pid}`;
  const client = new Client(namedUrl(name));
  await client.connect();
  try {
    const running = client.query('select pg_sleep(20)');
    void running.catch(() => {});
    await waitUntil(async () => await activeCount(name) === 1, 'WebSocket query did not start');

    const secret = client.secretKey;
    assert.equal(typeof client.processID, 'number');
    assert.equal(typeof secret, 'number');
    const packet = Buffer.alloc(16);
    packet.writeUInt32BE(16, 0);
    packet.writeUInt32BE(80877102, 4);
    packet.writeUInt32BE(client.processID >>> 0, 8);
    packet.writeUInt32BE(secret >>> 0, 12);
    await new Promise((resolve, reject) => {
      const socket = new WebSocket(`${wsProtocol}//${endpoint.host}/v2`, { handshakeTimeout: 5000 });
      const timeout = setTimeout(() => {
        socket.terminate();
        reject(new Error('CancelRequest WebSocket did not close'));
      }, 5000);
      socket.on('open', () => socket.send(packet));
      socket.on('close', () => { clearTimeout(timeout); resolve(); });
      socket.on('error', (error) => { clearTimeout(timeout); reject(error); });
    });
    await assert.rejects(running, (error) => error.code === '57014');
    await waitUntil(async () => await activeCount(name) === 0, 'canceled WebSocket query stayed active');
    const next = await client.query('select 1::int as answer');
    assert.equal(next.rows[0].answer, 1);
  } finally {
    await client.end();
  }
});

test('abrupt WebSocket disconnect releases its PostgreSQL session', {
  skip: process.env.BARNACLE_TEST_PGBOUNCER === '1' && 'PgBouncer may retain the idle PostgreSQL backend after the client disconnects',
}, async () => {
  const name = `barnacle_ws_disconnect_${process.pid}`;
  const client = new Client(namedUrl(name));
  client.on('error', () => {}); // Expected when the test terminates its socket.
  await client.connect();
  const running = client.query('select pg_sleep(5)');
  void running.catch(() => {});
  await waitUntil(async () => await activeCount(name) === 1, 'WebSocket query did not start');
  const socket = client.connection.stream.ws;
  assert.ok(socket, 'Neon Client has no WebSocket to interrupt');
  socket.terminate();
  await assert.rejects(running);
  await waitUntil(async () => {
    const rows = await observer.query(
      'select count(*)::int as count from pg_stat_activity where application_name = $1',
      [name],
    );
    return rows[0].count === 0;
  }, 'disconnected WebSocket retained a PostgreSQL session', 2000);
});
