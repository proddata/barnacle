import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { createServer as createHTTPServer } from 'node:http';
import { createServer as createTCPServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const binary = process.env.HERMIT_BROWSER_BINARY;
if (!binary) throw new Error('Set HERMIT_BROWSER_BINARY');
const chrome = [
  process.env.CHROME_BIN,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
].find((path) => path && existsSync(path));
if (!chrome) throw new Error('Google Chrome or Chromium is required for the browser CORS test');

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
function page(target) {
  return `<!doctype html><html data-result="pending"><body><script>
  (async () => {
    let http = 'blocked';
    try {
      const response = await fetch(${JSON.stringify(target + '/sql')}, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Neon-Connection-String': 'postgres://invalid:invalid@localhost/invalid',
        },
        body: '{}',
        signal: AbortSignal.timeout(8000),
      });
      http = String(response.status);
    } catch {}
    const ws = await new Promise((resolve) => {
      const socket = new WebSocket(${JSON.stringify(target.replace('http:', 'ws:') + '/v2')});
      const timeout = setTimeout(() => { socket.close(); resolve('blocked'); }, 8000);
      socket.onopen = () => { clearTimeout(timeout); socket.close(); resolve('open'); };
      socket.onerror = () => { clearTimeout(timeout); resolve('blocked'); };
    });
    document.documentElement.dataset.result = 'http:' + http + ' ws:' + ws;
  })();
  </script></body></html>`;
}

const mockSockets = new Set();
const mockPostgres = createTCPServer((socket) => {
  mockSockets.add(socket);
  socket.on('close', () => mockSockets.delete(socket));
});
const mockPort = await listen(mockPostgres);
const allowedPage = createHTTPServer();
const allowedPort = await listen(allowedPage);
const deniedPage = createHTTPServer();
const deniedPort = await listen(deniedPage);
const endpoint = createTCPServer();
const hermitPort = await listen(endpoint);
await close(endpoint);
const target = `http://127.0.0.1:${hermitPort}`;
allowedPage.on('request', (_, response) => {
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.end(page(target));
});
deniedPage.on('request', (_, response) => {
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.end(page(target));
});
const child = spawn(binary, [], {
  env: {
    ...process.env,
    HERMIT_LISTEN: `127.0.0.1:${hermitPort}`,
    HERMIT_PG_ADDR: `127.0.0.1:${mockPort}`,
    HERMIT_PG_SSLMODE: 'disable',
    HERMIT_ALLOWED_ORIGIN: `http://127.0.0.1:${allowedPort}`,
    HERMIT_OIDC_ISSUER: '',
    HERMIT_OIDC_AUDIENCE: '',
  },
  stdio: 'ignore',
});
const profiles = [];

async function browserResult(port) {
  const profile = await mkdtemp(join(tmpdir(), 'hermit-chrome-'));
  profiles.push(profile);
  const browser = spawn(chrome, [
    '--headless=new', '--no-first-run', '--disable-gpu',
    '--remote-allow-origins=*', '--remote-debugging-port=0',
    `--user-data-dir=${profile}`, 'about:blank',
  ], { stdio: ['ignore', 'ignore', 'pipe'] });
  const closed = new Promise((resolve) => browser.once('close', resolve));
  let startupError;
  let exitStatus;
  let stderr = '';
  browser.on('error', (error) => { startupError = error; });
  browser.on('exit', (code, signal) => { exitStatus = { code, signal }; });
  browser.stderr.on('data', (chunk) => { stderr = (stderr + chunk).slice(-8192); });
  let socket;
  try {
    let debugPort;
    for (let i = 0; i < 100; i++) {
      try {
        debugPort = Number((await readFile(join(profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]);
        break;
      } catch { await delay(100); }
    }
    assert.ok(debugPort, `Chrome debugging port did not start (exit: ${JSON.stringify(exitStatus)}, error: ${startupError?.message ?? 'none'}, stderr: ${stderr || 'empty'})`);
    const pages = await (await fetch(`http://127.0.0.1:${debugPort}/json/list`)).json();
    socket = new WebSocket(pages.find((page) => page.type === 'page').webSocketDebuggerUrl);
    await new Promise((resolve, reject) => {
      socket.addEventListener('open', resolve, { once: true });
      socket.addEventListener('error', reject, { once: true });
    });
    let nextID = 0;
    const pending = new Map();
    socket.addEventListener('message', ({ data }) => {
      const message = JSON.parse(data);
      if (!message.id) return;
      pending.get(message.id)?.(message);
      pending.delete(message.id);
    });
    function command(method, params = {}) {
      const id = ++nextID;
      return new Promise((resolve, reject) => {
        pending.set(id, (message) => message.error ? reject(new Error(message.error.message)) : resolve(message.result));
        socket.send(JSON.stringify({ id, method, params }));
      });
    }
    await command('Page.navigate', { url: `http://127.0.0.1:${port}/` });
    for (let i = 0; i < 200; i++) {
      const value = (await command('Runtime.evaluate', {
        expression: 'document.documentElement?.dataset.result',
        returnByValue: true,
      })).result.value;
      if (value && value !== 'pending') return value;
      await delay(100);
    }
    throw new Error(`browser page ${port} did not finish`);
  } finally {
    socket?.close();
    browser.kill('SIGTERM');
    await closed;
  }
}

try {
  let ready = false;
  for (let i = 0; i < 50; i++) {
    try {
      const response = await fetch(`${target}/healthz`);
      if (response.ok) { ready = true; break; }
    } catch { /* The listener is still starting. */ }
    await delay(50);
  }
  assert.ok(ready, 'Hermit did not start for browser CORS test');

  for (const [port, expected] of [
    [allowedPort, 'http:400 ws:open'],
    [deniedPort, 'http:blocked ws:blocked'],
  ]) {
    assert.equal(await browserResult(port), expected, `browser result from port ${port}`);
  }
  console.log('Chrome enforced HTTP CORS and WebSocket Origin for allowed and denied origins');
} finally {
  child.kill('SIGTERM');
  await close(allowedPage);
  await close(deniedPage);
  for (const socket of mockSockets) socket.destroy();
  await close(mockPostgres);
  for (const profile of profiles) await rm(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
}
