import assert from 'node:assert/strict';
import test from 'node:test';

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL');

async function request(body, connection = databaseUrl, authorization) {
  return fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': connection, ...(authorization ? { Authorization: authorization } : {}) },
    body,
  });
}

test('PostgreSQL authentication happens before JSON decoding', async () => {
  const wrong = new URL(databaseUrl);
  wrong.password = 'intentionally_wrong_request_limit_password';
  const response = await request('{"query":', wrong.toString());
  const body = await response.json();
  assert.equal(response.status, 400);
  // PgBouncer reports a failed SCRAM check as 08P01 instead of PostgreSQL's 28P01.
  const expected = process.env.BARNACLE_TEST_PGBOUNCER === '1' ? ['28P01', '08P01'] : ['28P01'];
  assert.ok(expected.includes(body.code), `unexpected authentication error code ${body.code}`);
  assert.equal((await request('{"query":')).status, 400);
});

test('dense and deeply nested JSON are rejected before executing SQL', async () => {
  for (const ignored of [Array.from({ length: 6000 }, () => ({ x: 0 })), JSON.parse('['.repeat(33) + '0' + ']'.repeat(33))]) {
    const response = await request(JSON.stringify({ query: 'select 1/0', ignored }));
    assert.equal(response.status, 413, await response.text());
  }
  const params = Array.from({ length: 4097 }, () => 0);
  const response = await request(JSON.stringify({ query: 'select 1/0', params }));
  assert.equal(response.status, 413, await response.text());
});

test('credential byte limits reject passwords and bearer tokens without echoing them', async () => {
  const oversized = new URL(databaseUrl);
  oversized.password = 'p'.repeat(4097);
  for (const [connection, authorization] of [[oversized.toString(), undefined], [databaseUrl, `Bearer ${'t'.repeat(16385)}`]]) {
    const response = await request('{"query":"select 1"}', connection, authorization);
    assert.equal(response.status, 431);
    assert.doesNotMatch(await response.text(), /p{20}|t{20}/);
  }
});
