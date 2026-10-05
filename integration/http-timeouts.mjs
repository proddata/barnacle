import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createConnection, createServer } from 'node:net';
import { setTimeout as delay } from 'node:timers/promises';
import WebSocket from 'ws';

const binary = process.env.HERMIT_TIMEOUT_BINARY;
if (!binary) throw new Error('Set HERMIT_TIMEOUT_BINARY');

async function listen(server) {
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  return server.address().port;
}
async function close(server) {
  await new Promise((resolve) => server.close(resolve));
}
async function within(promise, label, ms = 3000) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${label} timed out`)), ms); }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

const backendSockets = new Set();
const backend = createServer((socket) => {
  backendSockets.add(socket);
  socket.once('close', () => backendSockets.delete(socket));
});
const backendPort = await listen(backend);
const reservation = createServer();
const port = await listen(reservation);
await close(reservation);
const base = `http://127.0.0.1:${port}`;
const child = spawn(binary, [], {
  env: {
    ...process.env,
    HERMIT_LISTEN: `127.0.0.1:${port}`,
    HERMIT_PG_ADDR: `127.0.0.1:${backendPort}`,
    HERMIT_PG_SSLMODE: 'disable',
    HERMIT_HTTP_READ_TIMEOUT: '350ms',
    HERMIT_HTTP_WRITE_TIMEOUT: '2s',
    HERMIT_HTTP_IDLE_TIMEOUT: '350ms',
    HERMIT_WS_IDLE_TIMEOUT: '5s',
    HERMIT_OIDC_ISSUER: '',
    HERMIT_OIDC_AUDIENCE: '',
  },
  stdio: ['ignore', 'ignore', 'pipe'],
});
let stderr = '';
child.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-2000); });
const exited = new Promise((resolve, reject) => {
  child.once('exit', (code, signal) => resolve({ code, signal }));
  child.once('error', reject);
});

async function requestUntilClosed(request, label, drip = false) {
  const socket = createConnection(port, '127.0.0.1');
  let response = '';
  let dripping;
  socket.on('data', (chunk) => { response += chunk.toString(); });
  socket.on('error', () => {}); // A read deadline may end with FIN or reset.
  try {
    await within(new Promise((resolve) => socket.once('connect', resolve)), `${label} connect`);
    const closed = new Promise((resolve) => socket.once('close', resolve));
    socket.write(request);
    if (drip) dripping = setInterval(() => socket.write(' '), 50);
    await within(closed, `${label} close`);
    return response;
  } finally {
    clearInterval(dripping);
    socket.destroy();
  }
}

try {
  let ready = false;
  for (let i = 0; i < 50; i++) {
    try {
      if ((await fetch(`${base}/healthz`)).ok) { ready = true; break; }
    } catch { /* Listener is starting. */ }
    await delay(50);
  }
  assert.ok(ready, `Hermit did not start: ${stderr}`);

  const slowBody = await requestUntilClosed(
    `POST /sql HTTP/1.1\r\nHost: 127.0.0.1:${port}\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{`,
    'slow HTTP body', true,
  );
  assert.match(slowBody, /HTTP\/1\.1 400/, 'incomplete body should fail after its read deadline');

  const idleResponses = await Promise.all(Array.from({ length: 24 }, (_, index) => requestUntilClosed(
    `GET /healthz HTTP/1.1\r\nHost: 127.0.0.1:${port}\r\n\r\n`,
    `idle keep-alive ${index}`,
  )));
  for (const idle of idleResponses) {
    assert.match(idle, /HTTP\/1\.1 200/, 'keep-alive request did not succeed before idle close');
    assert.match(idle, /ok\r?\n/, 'health response missing');
  }

  const ws = new WebSocket(`ws://127.0.0.1:${port}/v2`);
  try {
    await within(new Promise((resolve, reject) => {
      ws.once('open', resolve);
      ws.once('error', reject);
    }), 'WebSocket upgrade');
    await delay(2500); // Exceeds all HTTP deadlines; the upgrade must clear them.
    const pong = new Promise((resolve, reject) => {
      ws.once('pong', resolve);
      ws.once('error', reject);
    });
    ws.ping('still-open');
    assert.deepEqual(await within(pong, 'WebSocket pong'), Buffer.from('still-open'));
  } finally {
    ws.terminate();
  }
  console.log('Slow HTTP body and idle keep-alive expired; WebSocket stayed active past HTTP deadlines');
} finally {
  child.kill('SIGTERM');
  await within(exited, 'Hermit exit').catch(() => child.kill('SIGKILL'));
  for (const socket of backendSockets) socket.destroy();
  await close(backend);
}

// A real query produces far more output than a paused socket can receive.
// The write deadline must release the HTTP slot while the client stays open.
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL');
const slowReservation = createServer();
const slowPort = await listen(slowReservation);
await close(slowReservation);
const slowBase = `http://127.0.0.1:${slowPort}`;
const slowChild = spawn(binary, [], {
  env: {
    ...process.env,
    HERMIT_LISTEN: `127.0.0.1:${slowPort}`,
    HERMIT_HTTP_WRITE_TIMEOUT: '500ms',
    HERMIT_QUERY_TIMEOUT: '5s',
    HERMIT_MAX_HTTP_QUERIES: '1',
    HERMIT_MAX_CONNECTIONS: '2',
    HERMIT_METRICS: 'true',
    HERMIT_OIDC_ISSUER: '',
    HERMIT_OIDC_AUDIENCE: '',
  },
  stdio: ['ignore', 'ignore', 'pipe'],
});
let slowStderr = '';
slowChild.stderr.on('data', (chunk) => { slowStderr = (slowStderr + chunk.toString()).slice(-2000); });
const slowExited = new Promise((resolve, reject) => {
  slowChild.once('exit', (code, signal) => resolve({ code, signal }));
  slowChild.once('error', reject);
});
let stalled;
try {
  let ready = false;
  for (let i = 0; i < 50; i++) {
    try {
      if ((await fetch(`${slowBase}/healthz`)).ok) { ready = true; break; }
    } catch { /* Listener is starting. */ }
    await delay(50);
  }
  assert.ok(ready, `Slow-reader gateway did not start: ${slowStderr}`);

  stalled = createConnection(slowPort, '127.0.0.1');
  stalled.on('error', () => {});
  await within(new Promise((resolve) => stalled.once('connect', resolve)), 'slow reader connect');
  stalled.pause();
  const body = JSON.stringify({ query: "select repeat('x', 8192) from generate_series(1, 10000)" });
  stalled.write(`POST /sql HTTP/1.1\r\nHost: 127.0.0.1:${slowPort}\r\nConnection: close\r\nNeon-Connection-String: ${databaseUrl}\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`);

  await delay(1200);
  const metrics = await within(fetch(`${slowBase}/metrics`).then((response) => response.text()), 'slow reader metrics');
  assert.match(metrics, /hermit_sql_requests_total 1\n/, `stalled response did not finish: ${metrics}`);
  const probe = await within(fetch(`${slowBase}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ query: 'select 1 as answer' }),
  }), 'query after slow reader');
  assert.equal(probe.status, 200, `slow reader kept the only HTTP slot: ${await probe.text()}`);
  console.log('Paused HTTP reader released its query slot after the write deadline');
} finally {
  stalled?.destroy();
  slowChild.kill('SIGTERM');
  await within(slowExited, 'slow-reader Hermit exit').catch(() => slowChild.kill('SIGKILL'));
}
