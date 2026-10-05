import assert from 'node:assert/strict';
import { isIP } from 'node:net';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const base = process.env.HERMIT_BASE_URL || 'http://127.0.0.1:8080';
const endpoint = new URL(base);
const databaseUrl = process.env.TEST_DATABASE_URL ||
  'postgres://hermit:hermit_dev_password@postgres:5432/hermit';
if (process.env.HERMIT_TEST_PG_IP) {
  assert.equal(isIP(new URL(databaseUrl).hostname), 4, 'TLS server-name test must route to a PostgreSQL IPv4 address');
}

neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;

const sql = neon(databaseUrl);
const httpRows = await sql`select ssl from pg_stat_ssl where pid = pg_backend_pid()`;
assert.equal(httpRows[0].ssl, true, 'HTTP reached PostgreSQL without TLS');

const client = new Client(databaseUrl);
try {
  await client.connect();
  const result = await client.query('select ssl from pg_stat_ssl where pid = pg_backend_pid()');
  assert.equal(result.rows[0].ssl, true, 'WebSocket reached PostgreSQL without TLS');
} finally {
  await client.end();
}

console.log(`PostgreSQL reports TLS for both HTTP and WebSocket sessions via ${new URL(databaseUrl).hostname}`);
