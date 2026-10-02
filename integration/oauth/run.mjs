import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { neon, neonConfig } from '@neondatabase/serverless';

const compose = fileURLToPath(new URL('compose.yaml', import.meta.url));
const validatorRef = 'cadfe7cecee671abf6a9e65727db7089760af513';
const issuer = 'https://issuer:8443';
const audience = 'https://postgres.example.internal/';
const project = `hermit-oauth-${process.pid}`;

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
  const header = { alg: 'RS256', typ: 'at+jwt', kid: 'hermit-test-key' };
  const claims = {
    iss: issuer, aud: audience, sub: 'appuser', client_id: 'hermit-test',
    jti: `hermit-${now}-${Math.random()}`, iat: now, nbf: now - 1, exp: now + 300,
    scope: 'connect:postgres', ...overrides,
  };
  const b64 = (value) => Buffer.from(JSON.stringify(value)).toString('base64url');
  const input = `${b64(header)}.${b64(claims)}`;
  return `${input}.${sign('RSA-SHA256', Buffer.from(input), privateKey).toString('base64url')}`;
}

const fixture = await mkdtemp(join(tmpdir(), 'hermit-oauth-'));
const source = join(fixture, 'validator-source');
const port = await freePort();
const image = process.env.HERMIT_OAUTH_VALIDATOR_IMAGE || `hermit-pg-oauth-validator:${validatorRef.slice(0, 12)}`;
const env = {
  ...process.env,
  HERMIT_OAUTH_FIXTURE_DIR: fixture,
  HERMIT_OAUTH_VALIDATOR_IMAGE: image,
  HERMIT_OAUTH_PORT: String(port),
};
const composeArgs = ['compose', '-p', project, '-f', compose];

try {
  if (!process.env.HERMIT_OAUTH_VALIDATOR_IMAGE) {
    await run('git', ['init', '-q', source]);
    await run('git', ['-C', source, 'remote', 'add', 'origin', 'https://github.com/proddata/pg_oauth_validator.git']);
    await run('git', ['-C', source, 'fetch', '--depth', '1', 'origin', validatorRef]);
    await run('git', ['-C', source, 'checkout', '-q', 'FETCH_HEAD']);
    await run('docker', ['build', '-f', 'playground/Containerfile', '-t', image, '.'], { cwd: source });
  }

  await run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
    '-keyout', join(fixture, 'ca.key'), '-out', join(fixture, 'ca.crt'),
    '-subj', '/CN=Hermit OAuth test CA', '-addext', 'basicConstraints=critical,CA:TRUE'], { stdio: 'ignore' });
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
    ...jwk, kid: 'hermit-test-key', use: 'sig', alg: 'RS256',
  }] }));
  await writeFile(join(fixture, 'init.sql'),
    `CREATE ROLE appuser LOGIN;\nCREATE ROLE otheruser LOGIN;\nCREATE ROLE fallbackuser LOGIN PASSWORD '${valid}';\n`);
  await writeFile(join(fixture, 'pg_hba.conf'), [
    'local all all trust',
    `host all appuser,otheruser 0.0.0.0/0 oauth issuer=${issuer} scope="connect:postgres" validator=pg_oauth_validator`,
    'host all fallbackuser 0.0.0.0/0 scram-sha-256',
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
  assert.ok(ready, 'Hermit did not start with the OIDC gate');

  neonConfig.fetchEndpoint = `${base}/sql`;
  const databaseUrl = 'postgres://appuser@localhost/hermit';
  const rows = await neon(databaseUrl, { authToken: valid })`select current_user as username, current_database() as database`;
  assert.deepEqual(rows[0], { username: 'appuser', database: 'hermit' });

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
  assert.equal((await post(badSignature)).status, 401, 'Hermit accepted an invalid signature');
  assert.equal((await post(jwt(privateKey, { aud: 'other' }))).status, 401, 'Hermit accepted a wrong audience');
  const badScope = await post(jwt(privateKey, { scope: 'read:metadata' }));
  assert.equal(badScope.status, 401, `PostgreSQL validator did not reject the wrong scope: ${JSON.stringify(badScope)}`);
  assert.equal(badScope.body.code, 'HERMIT_ERROR');
  const wrongRole = await post(valid, 'postgres://otheruser@localhost/hermit');
  assert.equal(wrongRole.status, 401, `PostgreSQL validator did not reject the wrong role: ${JSON.stringify(wrongRole)}`);
  const passwordFallback = await post(valid, 'postgres://fallbackuser@localhost/hermit');
  assert.equal(passwordFallback.status, 502,
    `OIDC request fell back to PostgreSQL password authentication: ${JSON.stringify(passwordFallback)}`);
  assert.equal((await post(valid)).status, 200, 'valid token failed after rejection cases');
  process.stdout.write('OAuth end-to-end: valid access token accepted; bad signature, audience, scope, role, and password fallback rejected.\n');
} catch (error) {
  try { await run('docker', [...composeArgs, 'logs', '--no-color'], { env }); } catch { /* Preserve the test failure. */ }
  throw error;
} finally {
  try { await run('docker', [...composeArgs, 'down', '-v', '--remove-orphans'], { env }); } finally {
    await rm(fixture, { recursive: true, force: true });
  }
}
