import assert from 'node:assert/strict';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.BARNACLE_BASE_URL;
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!base || !databaseUrl) throw new Error('Set BARNACLE_BASE_URL and TEST_DATABASE_URL');

const endpoint = new URL(base);
neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false; // The release fixture uses SCRAM.
neonConfig.forceDisablePgSSL = true;

if (process.env.BARNACLE_SMOKE_UNREACHABLE === '1') {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ query: 'select 1', params: [] }),
    signal: AbortSignal.timeout(5000),
  });
  assert.equal(response.status, 502, 'HTTP should report an unavailable upstream');
  assert.equal((await response.json()).message, 'postgres connection or query failed');

  await new Promise((resolve, reject) => {
    const socket = new WebSocket(`${endpoint.protocol === 'https:' ? 'wss:' : 'ws:'}//${endpoint.host}/v2`);
    const timeout = setTimeout(() => reject(new Error('WebSocket upstream failure timed out')), 5000);
    const finish = (error) => {
      clearTimeout(timeout);
      if (error) reject(error);
      else resolve();
    };
    socket.on('open', () => finish(new Error('WebSocket connected to an unavailable upstream')));
    socket.on('error', finish);
    socket.on('unexpected-response', (_, upstreamResponse) => {
      upstreamResponse.destroy();
      try {
        assert.equal(upstreamResponse.statusCode, 502, 'WebSocket upgrade should report an unavailable upstream');
        finish();
      } catch (error) { finish(error); }
    });
  });
  assert.equal((await fetch(`${base}/healthz`)).status, 200, 'Barnacle should remain alive');
  console.log('Release artifact: unavailable PostgreSQL fails cleanly over HTTP and WebSocket');
} else {
const sql = neon(databaseUrl);
const rows = await sql`select 42::int as answer, current_user as username`;
assert.deepEqual(rows[0], { answer: 42, username: new URL(databaseUrl).username });

const [first, second] = await sql.transaction([
  sql`select 1::int as value`,
  sql`select 2::int as value`,
]);
assert.equal(first[0].value, 1);
assert.equal(second[0].value, 2);

const client = new Client(databaseUrl);
try {
  await client.connect();
  const result = await client.query('select 43::int as answer, current_user as username');
  assert.deepEqual(result.rows[0], { answer: 43, username: new URL(databaseUrl).username });
} finally {
  await client.end();
}

const wrongPassword = new URL(databaseUrl);
wrongPassword.password = 'incorrect-password';
await assert.rejects(neon(wrongPassword.toString()).query('select 1'), (error) => error.code === '28P01');
const rejectedClient = new Client(wrongPassword.toString());
await assert.rejects(rejectedClient.connect(), (error) => error.code === '28P01');
assert.equal((await fetch(`${base}/healthz`)).status, 200, 'Barnacle should remain alive');

console.log('Release artifact: HTTP query, batch, WebSocket query, and rejected credentials passed');
}
