import assert from 'node:assert/strict';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.HERMIT_BASE_URL;
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!base || !databaseUrl) throw new Error('Set HERMIT_BASE_URL and TEST_DATABASE_URL');

const endpoint = new URL(base);
neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false; // The release fixture uses SCRAM.
neonConfig.forceDisablePgSSL = true;

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

console.log('Release artifact: HTTP query, HTTP batch, and WebSocket query passed');
