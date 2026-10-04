import test from 'node:test';
import assert from 'node:assert/strict';
import { splitStatements, statementAt } from './statements.js';

test('splits a script and selects the statement under the cursor', () => {
  const sql = 'select 1;\nselect 2;';
  assert.deepEqual(splitStatements(sql).map((item) => item.sql), ['select 1;', 'select 2;']);
  assert.equal(statementAt(sql, sql.indexOf('2')).sql, 'select 2;');
});

test('ignores semicolons in PostgreSQL quotes and comments', () => {
  const sql = `-- leading ;\nselect ';', "a;b", $$x;y$$, $tag$a;b$tag$, E'a\\';b' /* outer ; /* nested ; */ */;\nselect 3;`;
  assert.equal(splitStatements(sql).length, 2);
});

test('skips comment-only segments', () => {
  const statements = splitStatements('/* hi; */; -- bye;\n select 1;');
  assert.equal(statements.length, 1);
  assert.match(statements[0].sql, /select 1;/);
});
