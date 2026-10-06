import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createServer } from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const binary = process.env.BARNACLE_SHUTDOWN_BINARY;
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!binary || !databaseUrl) throw new Error('Set BARNACLE_SHUTDOWN_BINARY and TEST_DATABASE_URL');

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}

async function within(promise, label, ms = 5000) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${label} timed out`)), ms);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

const port = await freePort();
const base = `http://127.0.0.1:${port}`;
const child = spawn(binary, [], {
  env: { ...process.env, BARNACLE_LISTEN: `127.0.0.1:${port}` },
  stdio: ['ignore', 'ignore', 'pipe'],
});
let stderr = '';
child.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-2000); });
const exited = new Promise((resolve, reject) => {
  child.once('exit', (code, signal) => resolve({ code, signal }));
  child.once('error', reject);
});

try {
  let ready = false;
  for (let i = 0; i < 50; i++) {
    try {
      const response = await fetch(`${base}/healthz`);
      if (response.ok) { ready = true; break; }
    } catch { /* The listener is still starting. */ }
    await delay(50);
  }
  assert.ok(ready, `Barnacle did not start: ${stderr}`);

  neonConfig.fetchEndpoint = `${base}/sql`;
  neonConfig.webSocketConstructor = WebSocket;
  neonConfig.useSecureWebSocket = false;
  neonConfig.wsProxy = () => `127.0.0.1:${port}/v2`;
  neonConfig.pipelineConnect = false;
  neonConfig.forceDisablePgSSL = true;

  const wsName = `barnacle_shutdown_ws_${process.pid}`;
  const wsURL = new URL(databaseUrl);
  wsURL.searchParams.set('application_name', wsName);
  const client = new Client(wsURL.toString());
  client.on('error', () => {});
  await client.connect();
  const socket = client.connection.stream.ws;
  const closed = new Promise((resolve) => socket.once('close', (code) => resolve(code)));
  const wsQuery = client.query('select pg_sleep(20)');
  void wsQuery.catch(() => {});

  const httpName = `barnacle_shutdown_http_${process.pid}`;
  const httpURL = new URL(databaseUrl);
  httpURL.searchParams.set('application_name', httpName);
  const query = fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': httpURL.toString() },
    body: JSON.stringify({ query: 'select pg_sleep(2), 42::int as answer' }),
  });
  void query.catch(() => {});
  neonConfig.fetchEndpoint = `${process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080'}/sql`;
  const observer = neon(databaseUrl);
  let active = false;
  for (let i = 0; i < 60; i++) {
    const rows = await observer.query(
      'select application_name from pg_stat_activity where application_name in ($1, $2) and state = $3',
      [httpName, wsName, 'active'],
    );
    if (rows.some((row) => row.application_name === httpName) && rows.some((row) => row.application_name === wsName)) {
      active = true;
      break;
    }
    await delay(50);
  }
  assert.ok(active, 'HTTP and WebSocket queries did not start before SIGTERM');

  assert.ok(child.kill('SIGTERM'), 'could not send SIGTERM');
  assert.equal(await within(closed, 'WebSocket going-away close'), 1001);
  await assert.rejects(within(wsQuery, 'running WebSocket query'),
    (error) => error.message !== 'running WebSocket query timed out');
  const response = await within(query, 'in-flight HTTP query');
  assert.equal(response.status, 200);
  const result = await response.json();
  assert.equal(result.rows[0].answer, 42);
  assert.deepEqual(await within(exited, 'Barnacle process exit'), { code: 0, signal: null });
  const rows = await observer.query(
    'select count(*)::int as count from pg_stat_activity where application_name = $1', [wsName],
  );
  assert.equal(rows[0].count, 0, 'WebSocket retained a PostgreSQL session');
  console.log('SIGTERM drained HTTP and closed WebSocket with code 1001');
} finally {
  if (child.exitCode === null) {
    child.kill('SIGKILL');
    await within(exited, 'forced Barnacle exit').catch(() => {});
  }
}
