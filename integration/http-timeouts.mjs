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
    await delay(800); // Exceeds both HTTP deadlines; the upgrade must clear them.
    const pong = new Promise((resolve, reject) => {
      ws.once('pong', resolve);
      ws.once('error', reject);
    });
    ws.ping('still-open');
    assert.deepEqual(await within(pong, 'WebSocket pong'), Buffer.from('still-open'));
  } finally {
    ws.terminate();
  }
  console.log('Slow HTTP body and idle keep-alive expired; WebSocket stayed active');
} finally {
  child.kill('SIGTERM');
  await within(exited, 'Hermit exit').catch(() => child.kill('SIGKILL'));
  for (const socket of backendSockets) socket.destroy();
  await close(backend);
}
