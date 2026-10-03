import assert from 'node:assert/strict';
import test from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
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

async function within(promise, label) {
  return Promise.race([
    promise,
    delay(5000).then(() => { throw new Error(`${label} timed out`); }),
  ]);
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
  const name = `hermit_ws_close_${process.pid}`;
  const url = new URL(databaseUrl);
  url.searchParams.set('application_name', name);
  const client = new Client(url.toString());
  client.on('error', () => {});
  await client.connect();
  const observer = neon(databaseUrl);
  const count = async () => {
    const rows = await observer.query(
      'select count(*)::int as count from pg_stat_activity where application_name = $1', [name],
    );
    return rows[0].count;
  };
  assert.equal(await count(), 1);
  const socket = client.connection.stream.ws;
  const closed = new Promise((resolve) => socket.once('close', resolve));
  socket.close(1000, 'done');
  await within(closed, 'client close');
  for (let i = 0; i < 40 && await count() !== 0; i++) await delay(50);
  assert.equal(await count(), 0);
});
