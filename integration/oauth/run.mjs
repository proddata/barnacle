import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { generateKeyPairSync, sign } from 'node:crypto';
import { chmod, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const compose = fileURLToPath(new URL('compose.yaml', import.meta.url));
const validatorRef = 'cadfe7cecee671abf6a9e65727db7089760af513';
const issuer = 'https://issuer:8443';
const audience = 'https://postgres.example.internal/';
const project = `barnacle-oauth-${process.pid}`;

function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: 'inherit', ...options });
    child.on('error', reject);
    child.on('exit', (code) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${code}`)));
  });
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });
}

function jwt(privateKey, overrides = {}) {
  const now = Math.floor(Date.now() / 1000);
  const header = { alg: 'RS256', typ: 'at+jwt', kid: 'barnacle-test-key' };
  const claims = {
    iss: issuer, aud: audience, sub: 'appuser', client_id: 'barnacle-test',
    jti: `barnacle-${now}-${Math.random()}`, iat: now, nbf: now - 1, exp: now + 300,
    scope: 'connect:postgres', ...overrides,
  };
  const b64 = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');
  const input = `${b64(header)}.${b64(claims)}`;
  return `${input}.${sign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url')}`;
}

async function websocketOAuth(base, token) {
  const socket = new WebSocket(`${base.replace(/^http/, 'ws')}/v2`);
  const messages = [];
  let pending;
  let bytes = Buffer.alloc(0);
  let closedError;
  const deliver = (message) => {
    if (pending) {
      const { resolve, timer } = pending;
      pending = undefined;
      clearTimeout(timer);
      resolve(message);
    } else messages.push(message);
  };
  const fail = (error) => {
    closedError = error;
    if (pending) {
      const { reject, timer } = pending;
      pending = undefined;
      clearTimeout(timer);
      reject(error);
    }
  };
  socket.on('error', fail);
  socket.on('close', () => fail(new Error('WebSocket closed during PostgreSQL exchange')));
  socket.on('message', (chunk) => {
    bytes = Buffer.concat([bytes, Buffer.from(chunk)]);
    while (bytes.length >= 5) {
      const length = bytes.readInt32BE(1);
      if (length < 4 || length > 1 << 20) return fail(new Error('invalid PostgreSQL message length'));
      if (bytes.length < length + 1) break;
      deliver({ type: String.fromCharCode(bytes[0]), body: bytes.subarray(5, length + 1) });
      bytes = bytes.subarray(length + 1);
    }
  });
  const next = () => {
    if (messages.length) return Promise.resolve(messages.shift());
    if (closedError) return Promise.reject(closedError);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        pending = undefined;
        reject(new Error('timed out waiting for PostgreSQL over WebSocket'));
      }, 10000);
      pending = { resolve, reject, timer };
    });
  };
  const send = (type, body) => {
    const header = Buffer.alloc(5);
    header[0] = type.charCodeAt(0);
    header.writeInt32BE(body.length + 4, 1);
    socket.send(Buffer.concat([header, body]));
  };
  try {
    await new Promise((resolve, reject) => {
      socket.once('open', resolve);
      socket.once('error', reject);
    });
    const parameters = Buffer.from('user\0appuser\0database\0barnacle\0\0');
    const startup = Buffer.alloc(8 + parameters.length);
    startup.writeInt32BE(startup.length, 0);
    startup.writeInt32BE(196608, 4);
    parameters.copy(startup, 8);
    socket.send(startup);
    let authenticated = false;
    for (;;) {
      const message = await next();
      if (message.type === 'E') throw new Error('PostgreSQL rejected WebSocket OAuth');
      if (message.type === 'R') {
        const method = message.body.readInt32BE(0);
        if (method === 10) {
          assert.ok(message.body.includes(Buffer.from('OAUTHBEARER\0')));
          const response = Buffer.from(`n,,\x01auth=Bearer ${token}\x01\x01`);
          const mechanism = Buffer.from('OAUTHBEARER\0');
          const size = Buffer.alloc(4);
          size.writeInt32BE(response.length);
          send('p', Buffer.concat([mechanism, size, response]));
        } else if (method === 0) authenticated = true;
        else throw new Error(`unexpected PostgreSQL authentication method ${method}`);
      }
      if (message.type === 'Z') break;
    }
    assert.ok(authenticated, 'PostgreSQL did not authenticate the WebSocket client');
    send('Q', Buffer.from('select current_user\0'));
    let currentUser;
    for (;;) {
      const message = await next();
      if (message.type === 'E') throw new Error('PostgreSQL rejected WebSocket query');
      if (message.type === 'D') {
        assert.equal(message.body.readInt16BE(0), 1);
        const length = message.body.readInt32BE(2);
        currentUser = message.body.subarray(6, 6 + length).toString();
      }
      if (message.type === 'Z') break;
    }
    return currentUser;
  } finally {
    socket.terminate();
  }
}

const fixture = await mkdtemp(join(tmpdir(), 'barnacle-oauth-'));
// The PostgreSQL container must traverse this host directory to read pg_hba.conf.
await chmod(fixture, 0o711);
const source = join(fixture, 'validator-source');
const port = await freePort();
const image = process.env.BARNACLE_OAUTH_VALIDATOR_IMAGE || `barnacle-pg-oauth-validator:${validatorRef.slice(0, 12)}`;
const env = {
  ...process.env,
  BARNACLE_OAUTH_FIXTURE_DIR: fixture,
  BARNACLE_OAUTH_VALIDATOR_IMAGE: image,
  BARNACLE_OAUTH_PORT: String(port),
};
const composeArgs = ['compose', '-p', project, '-f', compose];

try {
  if (!process.env.BARNACLE_OAUTH_VALIDATOR_IMAGE) {
    await run('git', ['init', '-q', source]);
    await run('git', ['-C', source, 'remote', 'add', 'origin', 'https://github.com/proddata/pg_oauth_validator.git']);
    await run('git', ['-C', source, 'fetch', '--depth', '1', 'origin', validatorRef]);
    await run('git', ['-C', source, 'checkout', '-q', 'FETCH_HEAD']);
    await run('docker', ['build', '-f', 'playground/Containerfile', '-t', image, '.'], { cwd: source });
  }

  await run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
    '-keyout', join(fixture, 'ca.key'), '-out', join(fixture, 'ca.crt'),
    '-subj', '/CN=Barnacle OAuth test CA', '-addext', 'basicConstraints=critical,CA:TRUE'], { stdio: 'ignore' });
  await run('openssl', ['req', '-new', '-newkey', 'rsa:2048', '-nodes',
    '-keyout', join(fixture, 'issuer.key'), '-out', join(fixture, 'issuer.csr'),
    '-subj', '/CN=issuer'], { stdio: 'ignore' });
  await writeFile(join(fixture, 'issuer.ext'), 'subjectAltName=DNS:issuer\nextendedKeyUsage=serverAuth\n');
  await run('openssl', ['x509', '-req', '-days', '1', '-sha256',
    '-in', join(fixture, 'issuer.csr'), '-CA', join(fixture, 'ca.crt'),
    '-CAkey', join(fixture, 'ca.key'), '-CAcreateserial',
    '-out', join(fixture, 'issuer.crt'), '-extfile', join(fixture, 'issuer.ext')], { stdio: 'ignore' });

  const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
  const valid = jwt(privateKey);
  const jwk = publicKey.export({ format: 'jwk' });
  await writeFile(join(fixture, 'jwks.json'), JSON.stringify({ keys: [{
    ...jwk, kid: 'barnacle-test-key', use: 'sig', alg: 'RS256',
  }] }));
  await writeFile(join(fixture, 'init.sql'),
    `CREATE ROLE appuser LOGIN;\nCREATE ROLE otheruser LOGIN;\nCREATE ROLE fallbackuser LOGIN PASSWORD '${valid}';\nCREATE ROLE trustuser LOGIN;\n`);
  await writeFile(join(fixture, 'pg_hba.conf'), [
    'local all all trust',
    `host all appuser,otheruser 0.0.0.0/0 oauth issuer=${issuer} scope="connect:postgres" validator=pg_oauth_validator`,
    'host all fallbackuser 0.0.0.0/0 scram-sha-256',
    'host all trustuser 0.0.0.0/0 trust',
    '',
  ].join('\n'));

  await run('docker', [...composeArgs, 'up', '--build', '-d'], { env });
  const base = `http://127.0.0.1:${port}`;
  let ready = false;
  for (let attempt = 0; attempt < 40; attempt++) {
    try {
      const response = await fetch(`${base}/healthz`);
      if (response.ok) { ready = true; break; }
    } catch { /* Startup is still in progress. */ }
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
  assert.ok(ready, 'Barnacle did not start');

  neonConfig.fetchEndpoint = `${base}/sql`;
  const databaseUrl = 'postgres://appuser@localhost/barnacle';
  const rows = await neon(databaseUrl, { authToken: valid })`select current_user as username, current_database() as database`;
  assert.deepEqual(rows[0], { username: 'appuser', database: 'barnacle' });
  assert.equal(await websocketOAuth(base, valid), 'appuser');

  async function post(token, connectionString = databaseUrl) {
    const response = await fetch(`${base}/sql`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'Neon-Connection-String': connectionString,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ query: 'select current_user', params: [] }),
    });
    return { status: response.status, body: await response.json() };
  }

  const badSignature = jwt(generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey);
  assert.equal((await post(badSignature)).status, 401, 'PostgreSQL accepted an invalid signature');
  await assert.rejects(websocketOAuth(base, badSignature), 'PostgreSQL accepted an invalid WebSocket token');
  assert.equal((await post(jwt(privateKey, { aud: 'other' }))).status, 401, 'PostgreSQL accepted a wrong audience');
  const badScope = await post(jwt(privateKey, { scope: 'read:metadata' }));
  assert.equal(badScope.status, 401, `PostgreSQL validator did not reject the wrong scope: ${JSON.stringify(badScope)}`);
  assert.equal(badScope.body.code, 'BARNACLE_ERROR');
  const wrongRole = await post(valid, 'postgres://otheruser@localhost/barnacle');
  assert.equal(wrongRole.status, 401, `PostgreSQL validator did not reject the wrong role: ${JSON.stringify(wrongRole)}`);
  const passwordFallback = await post(valid, 'postgres://fallbackuser@localhost/barnacle');
  assert.equal(passwordFallback.status, 502,
    `bearer request fell back to PostgreSQL password authentication: ${JSON.stringify(passwordFallback)}`);
  assert.equal(passwordFallback.body.code, 'BARNACLE_UPSTREAM_OAUTH_UNAVAILABLE',
    `unexpected response when PostgreSQL does not offer OAuth: ${JSON.stringify(passwordFallback)}`);
  const basic = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      Authorization: `Basic ${Buffer.from(`fallbackuser:${valid}`).toString('base64')}`,
      'Neon-Connection-String': 'postgres://localhost/barnacle?require_auth=scram-sha-256',
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ query: 'select current_user', params: [] }),
  });
  assert.equal(basic.status, 200, `explicit Basic/SCRAM request failed: ${await basic.text()}`);
  const trust = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      Authorization: `Basic ${Buffer.from('trustuser:anything').toString('base64')}`,
      'Neon-Connection-String': 'postgres://localhost/barnacle',
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ query: 'select current_user', params: [] }),
  });
  assert.equal(trust.status, 502, `password request bypassed authentication through trust: ${await trust.text()}`);
  assert.equal((await post(valid)).status, 200, 'valid token failed after rejection cases');
  process.stdout.write('OAuth end-to-end: HTTP and WebSocket OAuth accepted; invalid tokens and roles rejected; bearer password fallback rejected; Basic/SCRAM accepted.\n');
} catch (error) {
  try { await run('docker', [...composeArgs, 'logs', '--no-color'], { env }); } catch { /* Preserve the test failure. */ }
  throw error;
} finally {
  try { await run('docker', [...composeArgs, 'down', '-v', '--remove-orphans'], { env }); } finally {
    await rm(fixture, { recursive: true, force: true });
  }
}
