import assert from 'node:assert/strict';
import test from 'node:test';
import { Pool, neonConfig } from '@neondatabase/serverless';

const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';

test('Neon Pool.query uses HTTP and preserves node-postgres result options', async () => {
  const previous = {
    fetchEndpoint: neonConfig.fetchEndpoint,
    fetchFunction: neonConfig.fetchFunction,
    poolQueryViaFetch: neonConfig.poolQueryViaFetch,
    wsProxy: neonConfig.wsProxy,
  };
  let fetches = 0;
  neonConfig.fetchEndpoint = `${base}/sql`;
  neonConfig.poolQueryViaFetch = true;
  neonConfig.wsProxy = () => { throw new Error('Pool.query unexpectedly used WebSocket'); };
  neonConfig.fetchFunction = (...args) => {
    fetches++;
    return fetch(...args);
  };
  const pool = new Pool({ connectionString: databaseUrl });
  try {
    const object = await pool.query('select $1::int as answer', [42]);
    assert.equal(object.rows[0].answer, 42);
    assert.equal(object.rowCount, 1);
    assert.equal(object.fields[0].name, 'answer');
    assert.equal(object.viaNeonFetch, true);

    const array = await pool.query({ text: 'select $1::int as answer', values: [43], rowMode: 'array' });
    assert.deepEqual(array.rows, [[43]]);
    assert.equal(array.rowAsArray, true);
    assert.equal(array.viaNeonFetch, true);
    assert.equal(fetches, 2);
  } finally {
    await pool.end();
    Object.assign(neonConfig, previous);
  }
});
