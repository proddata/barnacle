import assert from 'node:assert/strict';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.HERMIT_BASE_URL || 'http://127.0.0.1:8080';
const endpoint = new URL(base);
const databaseUrl = process.env.TEST_DATABASE_URL ||
  'postgres://hermit:hermit_dev_password@postgres:5432/hermit';

neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = `${endpoint.host}/v2`; // Driver appends ?address=host:port.
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;

const databaseUrls = [databaseUrl];
if (process.env.TEST_SECOND_DATABASE_URL) databaseUrls.push(process.env.TEST_SECOND_DATABASE_URL);
for (const url of databaseUrls) {
  const sql = neon(url);
  const rows = await sql`select 42::int as answer`;
  assert.equal(rows[0].answer, 42);

  const client = new Client(url);
  try {
    await client.connect();
    const result = await client.query('select 43::int as answer');
    assert.equal(result.rows[0].answer, 43);
  } finally {
    await client.end();
  }
}

const forbidden = new URL(databaseUrl);
forbidden.hostname = 'not-allowed.internal';
const response = await fetch(`${base}/sql`, {
  method: 'POST',
  headers: { 'Neon-Connection-String': forbidden.toString() },
  body: JSON.stringify({ query: 'select 1' }),
});
assert.equal(response.status, 400);

const wsStatus = await new Promise((resolve, reject) => {
  const protocol = endpoint.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${protocol}//${endpoint.host}/v2?address=not-allowed.internal:5432`, {
    handshakeTimeout: 5000,
  });
  ws.on('unexpected-response', (_request, rejected) => {
    rejected.resume();
    ws.terminate();
    resolve(rejected.statusCode);
  });
  ws.on('error', reject);
  ws.on('open', () => reject(new Error('unlisted WebSocket address was accepted')));
});
assert.equal(wsStatus, 400);

console.log(`Neon HTTP and WebSocket routing passed for ${databaseUrls.length} target(s); unlisted address rejected`);
