import assert from 'node:assert/strict';
import test from 'node:test';
import { queryMatrix, typeMatrix } from './type-matrix.mjs';

const endpoint = `${process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080'}/sql`;
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the type matrix');

for (const item of typeMatrix) {
  test(`HTTP type matrix: ${item.name}`, async () => {
    for (const raw of [false, true]) {
      const body = await queryMatrix(endpoint, databaseUrl, item, raw);
      assert.equal(body.command, 'SELECT');
      assert.equal(body.rowCount, 1);
      assert.equal(body.rowAsArray, false);
      assert.equal(body.fields.length, 1);
      assert.equal(body.fields[0].name, 'value');
      assert.equal(body.fields[0].format, 'text');
      assert.equal(body.rows.length, 1);
      const value = body.rows[0].value;
      if (raw && item.raw !== undefined) {
        assert.deepEqual(value, item.raw);
      } else if (raw) {
        assert.equal(typeof value, 'string');
      } else {
        assert.deepEqual(value, item.plain);
      }
    }
  });
}
