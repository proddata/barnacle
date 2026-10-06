import assert from 'node:assert/strict';
import test from 'node:test';
import { NeonDbError, neon, neonConfig } from '@neondatabase/serverless';

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
neonConfig.fetchEndpoint = `${base}/sql`;

async function rawQuery(body) {
  return fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

test('oversized request uses Neon 413 behavior in raw fetch and the published driver', async () => {
  const sqlText = `select 1 /* ${'x'.repeat(1 << 20)} */`;
  const response = await rawQuery({ query: sqlText });
  assert.equal(response.status, 413);
  const body = await response.json();
  assert.equal(body.code, 'BARNACLE_ERROR');
  assert.match(body.message, /request is too large/);

  const sql = neon(databaseUrl);
  await assert.rejects(sql.query(sqlText), (error) => {
    assert.ok(error instanceof NeonDbError);
    assert.match(error.message, /Server error \(HTTP status 413\)/);
    assert.equal(error.code, undefined);
    assert.equal(error.severity, undefined);
    return true;
  });
});

test('PostgreSQL query error retains Neon 400 fields in raw fetch and the published driver', async () => {
  const response = await rawQuery({ query: 'select 1 / 0' });
  assert.equal(response.status, 400);
  const body = await response.json();
  assert.equal(body.message, 'division by zero');
  assert.equal(body.code, '22012');
  assert.equal(body.severity, 'ERROR');
  assert.ok(Object.hasOwn(body, 'detail'));

  const sql = neon(databaseUrl);
  await assert.rejects(sql.query('select 1 / 0'), (error) => {
    assert.ok(error instanceof NeonDbError);
    assert.equal(error.message, body.message);
    assert.equal(error.code, body.code);
    assert.equal(error.severity, body.severity);
    assert.equal(error.detail, undefined);
    return true;
  });
});
