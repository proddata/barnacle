import assert from 'node:assert/strict';
import test from 'node:test';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

globalThis.WebSocket = WebSocket;

const base = process.env.HERMIT_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL;
if (!databaseUrl) throw new Error('Set TEST_DATABASE_URL for the integration suite');
const endpoint = new URL(base);

neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false; // The test database uses SCRAM.
neonConfig.forceDisablePgSSL = true;

test('Neon HTTP query accepts parameters and returns parsed rows', async () => {
  const sql = neon(databaseUrl);
  const rows = await sql.query('select $1::int as answer, current_user as username', [42]);
  assert.equal(rows[0].answer, 42);
  assert.equal(rows[0].username, new URL(databaseUrl).username);
});

test('Neon HTTP authToken is forwarded as the PostgreSQL password', async () => {
  const url = new URL(databaseUrl);
  const password = decodeURIComponent(url.password);
  url.password = '';
  const sql = neon(url.toString(), { authToken: password });
  const rows = await sql`select current_user as username`;
  assert.equal(rows[0].username, url.username);
});

test('Neon HTTP transaction returns both results', async () => {
  const sql = neon(databaseUrl);
  const [first, second] = await sql.transaction([
    sql`select 1::int as value`,
    sql`select 2::int as value`,
  ]);
  assert.equal(first[0].value, 1);
  assert.equal(second[0].value, 2);
});

test('Neon transaction applies mixed per-query arrayMode and fullResults options', async () => {
  const sql = neon(databaseUrl);
  const [object, array] = await sql.transaction([
    sql.query('select 41::int as value', [], { arrayMode: false, fullResults: true }),
    sql.query('select 42::int as value', [], { arrayMode: true, fullResults: true }),
  ]);
  assert.deepEqual(object.rows, [{ value: 41 }]);
  assert.equal(object.rowAsArray, false);
  assert.deepEqual(array.rows, [[42]]);
  assert.equal(array.rowAsArray, true);
});

test('HTTP request body arrayMode overrides the request header per query', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      'Neon-Connection-String': databaseUrl,
      'Neon-Array-Mode': 'true',
      'Neon-Raw-Text-Output': 'true',
    },
    body: JSON.stringify({
      queries: [
        { query: 'select 41::int as value', params: [], arrayMode: false },
        { query: 'select 42::int as value', params: [], arrayMode: true },
        { query: 'select 43::int as value', params: [] },
      ],
    }),
  });
  assert.equal(response.status, 200);
  const { results } = await response.json();
  assert.deepEqual(results.map((result) => result.rows), [
    [{ value: '41' }], [['42']], [['43']],
  ]);
  assert.deepEqual(results.map((result) => result.rowAsArray), [false, true, true]);

  const single = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      'Neon-Connection-String': databaseUrl,
      'Neon-Array-Mode': 'true',
      'Neon-Raw-Text-Output': 'true',
    },
    body: JSON.stringify({ query: 'select 44::int as value', params: [], arrayMode: false }),
  });
  assert.equal(single.status, 200);
  const result = await single.json();
  assert.deepEqual(result.rows, [{ value: '44' }]);
  assert.equal(result.rowAsArray, false);
});

test('Neon HTTP accepts all batch isolation values', async () => {
  const sql = neon(databaseUrl);
  for (const [isolationLevel, expected] of [
    ['ReadUncommitted', 'read uncommitted'],
    ['ReadCommitted', 'read committed'],
    ['RepeatableRead', 'repeatable read'],
    ['Serializable', 'serializable'],
  ]) {
    const [rows] = await sql.transaction([
      sql`select current_setting('transaction_isolation') as level`,
    ], { isolationLevel });
    assert.equal(rows[0].level, expected);
  }
});

test('Neon fullResults preserves command metadata and custom type parsers', async () => {
  const sql = neon(databaseUrl);
  const selected = await sql.query('select 42::int4 as value, 7::int4 as value', [], {
    fullResults: true,
    types: { getTypeParser: (oid) => oid === 23 ? (value) => `int:${value}` : (value) => value },
  });
  assert.equal(selected.command, 'SELECT');
  assert.equal(selected.rowCount, 1);
  assert.equal(selected.rowAsArray, false);
  assert.equal(selected.rows[0].value, 'int:7');
  assert.equal(selected.fields.length, 2);
  assert.equal(selected.fields[0].name, 'value');
  assert.equal(selected.fields[0].dataTypeID, 23);

  const array = await sql.query('select 42::int4 as value', [], { fullResults: true, arrayMode: true });
  assert.equal(array.rowAsArray, true);
  assert.deepEqual(array.rows, [[42]]);

  const table = `hermit_metadata_${process.pid}`;
  const ddl = await sql.query(`create table ${table} (id int)`, [], { fullResults: true });
  try {
    assert.equal(ddl.command, 'CREATE');
    assert.equal(ddl.rowCount, null);
    const inserted = await sql.query(`insert into ${table} values (1), (2)`, [], { fullResults: true });
    assert.equal(inserted.command, 'INSERT');
    assert.equal(inserted.rowCount, 2);
  } finally {
    await sql.query(`drop table ${table}`);
  }
  const empty = await sql.query('select 1::int where false', [], { fullResults: true });
  assert.equal(empty.rowCount, 0);
  assert.deepEqual(empty.rows, []);
});

test('Neon HTTP parses PostgreSQL scalar and array types', async () => {
  const sql = neon(databaseUrl);
  const rows = await sql.query(`select
    true as flag,
    32767::int2 as small,
    42::int4 as normal,
    9223372036854775807::int8 as big,
    1.25::numeric as precise,
    'Infinity'::float8 as infinite,
    '{"a":1}'::jsonb as document,
    array[1, null, 3]::int4[] as numbers,
    array['a,b', 'NULL', null, 'q"z']::text[] as words,
    '2024-01-02 03:04:05+00'::timestamptz as instant,
    '\\x00ff'::bytea as bytes`);
  const row = rows[0];
  assert.equal(row.flag, true);
  assert.equal(row.small, 32767);
  assert.equal(row.normal, 42);
  assert.equal(row.big, '9223372036854775807');
  assert.equal(row.precise, '1.25');
  assert.equal(row.infinite, Infinity);
  assert.deepEqual(row.document, { a: 1 });
  assert.deepEqual(row.numbers, [1, null, 3]);
  assert.deepEqual(row.words, ['a,b', 'NULL', null, 'q"z']);
  assert.equal(row.instant.toISOString(), '2024-01-02T03:04:05.000Z');
  assert.deepEqual(row.bytes, Buffer.from([0, 255]));
});

test('Neon HTTP binds JSON objects and nested PostgreSQL arrays', async () => {
  const sql = neon(databaseUrl);
  const rows = await sql.query(
    'select $1::jsonb as document, $2::int4[][] as numbers, $3::text[] as words, $4::jsonb[] as documents',
    [{ a: 1 }, [[1, 2], [3, 4]], ['a,b', 'NULL', null, 'q"z'], [{ a: 1 }, { b: 2 }]],
  );
  assert.deepEqual(rows[0].document, { a: 1 });
  assert.deepEqual(rows[0].numbers, [[1, 2], [3, 4]]);
  assert.deepEqual(rows[0].words, ['a,b', 'NULL', null, 'q"z']);
  assert.deepEqual(rows[0].documents, [{ a: 1 }, { b: 2 }]);
});

test('Neon HTTP binds Buffer as bytea', async () => {
  const sql = neon(databaseUrl);
  const rows = await sql.query('select $1::bytea as bytes', [Buffer.from([0, 255, 92])]);
  assert.deepEqual(rows[0].bytes, Buffer.from([0, 255, 92]));
});

test('HTTP compresses a large result when gzip is accepted', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      'Neon-Connection-String': databaseUrl,
      'Accept-Encoding': 'gzip',
    },
    body: JSON.stringify({ query: "select repeat('x', 8192) as payload", params: [] }),
  });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('content-encoding'), 'gzip');
  const body = await response.json();
  assert.equal(body.rows[0].payload.length, 8192);
});

test('HTTP reports a PostgreSQL error before streaming rows', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ query: 'select 1 / 0', params: [] }),
  });
  assert.equal(response.status, 400);
  const body = await response.json();
  assert.equal(body.code, '22012');
});

test('Neon HTTP exposes authentication and SQL error details', async () => {
  const wrongPassword = new URL(databaseUrl);
  wrongPassword.password = 'incorrect-password';
  await assert.rejects(neon(wrongPassword.toString()).query('select 1'), (error) => {
    assert.equal(error.code, '28P01');
    assert.equal(error.severity, 'FATAL');
    return true;
  });

  const missingDatabase = new URL(databaseUrl);
  missingDatabase.pathname = '/hermit_missing_database';
  await assert.rejects(neon(missingDatabase.toString()).query('select 1'), (error) => {
    assert.equal(error.code, '3D000');
    return true;
  });

  const missingRole = new URL(databaseUrl);
  missingRole.username = 'hermit_missing_role';
  await assert.rejects(neon(missingRole.toString()).query('select 1'), (error) => {
    assert.equal(error.code, '28P01'); // SCRAM does not disclose whether the role exists.
    return true;
  });

  await assert.rejects(neon(databaseUrl).query('select from'), (error) => {
    assert.equal(error.code, '42601');
    return true;
  });

  await assert.rejects(neon(databaseUrl).query('select missing_identifier'), (error) => {
    assert.equal(error.code, '42703');
    assert.equal(error.position, '8');
    return true;
  });

  const table = `hermit_constraint_${process.pid}`;
  const sql = neon(databaseUrl);
  await sql.query(`create table ${table} (id int primary key)`);
  try {
    await sql.query(`insert into ${table} values (1)`);
    await assert.rejects(sql.query(`insert into ${table} values (1)`), (error) => {
      assert.equal(error.code, '23505');
      assert.equal(error.table, table);
      assert.equal(error.schema, 'public');
      assert.equal(error.constraint, `${table}_pkey`);
      return true;
    });
  } finally {
    await sql.query(`drop table ${table}`);
  }
});

test('HTTP streams a large response without a content length', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: {
      'Neon-Connection-String': databaseUrl,
      'Accept-Encoding': 'identity',
    },
    body: JSON.stringify({ query: "select repeat('x', 1048576) as payload", params: [] }),
  });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('content-length'), null);
  const reader = response.body.getReader();
  const chunks = [];
  for (;;) {
    const next = await reader.read();
    if (next.done) break;
    chunks.push(next.value);
  }
  const body = JSON.parse(Buffer.concat(chunks).toString());
  assert.equal(body.rows[0].payload.length, 1048576);
  assert.ok(chunks.length > 1, `expected multiple response chunks, got ${chunks.length}`);
});

test('HTTP rejects a field above the default row limit', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ query: "select repeat('x', 9 * 1048576) as payload", params: [] }),
  });
  assert.equal(response.status, 413);
  assert.equal((await response.json()).code, 'HERMIT_ERROR');
});

test('HTTP bounds the total buffered batch result', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ queries: Array.from({ length: 5 }, () => ({
      query: "select repeat('x', 1048576) as payload", params: [],
    })) }),
  });
  assert.equal(response.status, 413);
  assert.equal((await response.json()).code, 'HERMIT_ERROR');
});

test('HTTP bounds buffered batches with many empty rows', async () => {
  const response = await fetch(`${base}/sql`, {
    method: 'POST',
    headers: { 'Neon-Connection-String': databaseUrl },
    body: JSON.stringify({ queries: [{
      query: "select ''::text as payload from generate_series(1, 40000)", params: [],
    }] }),
  });
  assert.equal(response.status, 413);
});

test('plain JSON follows Neon type conversion, including a custom enum array', async () => {
  const sql = neon(databaseUrl);
  const typeName = `hermit_compat_enum_${process.pid}`;
  await sql.query(`create type ${typeName} as enum ('red', 'blue')`);
  try {
    const response = await fetch(`${base}/sql`, {
      method: 'POST',
      headers: { 'Neon-Connection-String': databaseUrl },
      body: JSON.stringify({ query: `select
        true as flag, 42::int4 as number, 9223372036854775807::int8 as bigint,
        'NaN'::float8 as unusual_float, '{"a":1}'::jsonb as document,
        array['red', 'blue']::${typeName}[] as colors`, params: [] }),
    });
    assert.equal(response.status, 200);
    const body = await response.json();
    assert.deepEqual(body.rows[0], {
      flag: true,
      number: 42,
      bigint: '9223372036854775807',
      unusual_float: 'NaN',
      document: { a: 1 },
      colors: ['red', 'blue'],
    });
  } finally {
    await sql.query(`drop type ${typeName}`);
  }
});

test('plain and raw HTTP output preserve less common PostgreSQL types', async () => {
  const query = `select
    '123e4567-e89b-12d3-a456-426614174000'::uuid as identifier,
    '1 day 02:03:04'::interval as duration,
    '\\x00ff'::bytea as bytes,
    array[1.25::numeric, null] as decimals,
    array['NaN'::float8, 'Infinity'::float8] as floats,
    array['{"a":1}'::jsonb, null] as documents`;
  const request = async (raw) => {
    const response = await fetch(`${base}/sql`, {
      method: 'POST',
      headers: {
        'Neon-Connection-String': databaseUrl,
        ...(raw ? { 'Neon-Raw-Text-Output': 'true' } : {}),
      },
      body: JSON.stringify({ query, params: [] }),
    });
    assert.equal(response.status, 200);
    return (await response.json()).rows[0];
  };
  const plain = await request(false);
  assert.equal(plain.identifier, '123e4567-e89b-12d3-a456-426614174000');
  assert.equal(plain.duration, '1 day 02:03:04');
  assert.equal(plain.bytes, '\\x00ff');
  assert.deepEqual(plain.decimals, ['1.25', null]);
  assert.deepEqual(plain.floats, ['NaN', 'Infinity']);
  assert.deepEqual(plain.documents, [{ a: 1 }, null]);

  const raw = await request(true);
  assert.equal(raw.identifier, plain.identifier);
  assert.equal(raw.duration, plain.duration);
  assert.equal(raw.bytes, plain.bytes);
  assert.equal(raw.decimals, '{1.25,NULL}');
  assert.equal(raw.floats, '{NaN,Infinity}');
  assert.equal(typeof raw.documents, 'string');
});

test('HTTP handles domains, composites, ranges, array bounds, and box delimiters', async () => {
  const sql = neon(databaseUrl);
  const suffix = `${process.pid}`;
  const domain = `hermit_domain_${suffix}`;
  const composite = `hermit_composite_${suffix}`;
  const range = `hermit_range_${suffix}`;
  await sql.query(`create domain ${domain} as int4`);
  await sql.query(`create type ${composite} as (id int4, label text)`);
  await sql.query(`create type ${range} as range (subtype=int4)`);
  try {
    const query = `select
      17::${domain} as domain_value,
      row(7, 'a,b')::${composite} as composite_value,
      '[1,4)'::${range} as range_value,
      '[2:3]={10,20}'::int4[] as bounded_array,
      array[box(point(0,0),point(1,1)), box(point(2,2),point(3,3))]::box[] as boxes`;
    const request = async (raw) => {
      const response = await fetch(`${base}/sql`, {
        method: 'POST',
        headers: {
          'Neon-Connection-String': databaseUrl,
          ...(raw ? { 'Neon-Raw-Text-Output': 'true' } : {}),
        },
        body: JSON.stringify({ query, params: [] }),
      });
      assert.equal(response.status, 200);
      return (await response.json()).rows[0];
    };
    const plain = await request(false);
    assert.equal(plain.domain_value, 17);
    assert.match(plain.composite_value, /^\(7,/);
    assert.equal(plain.range_value, '[1,4)');
    assert.deepEqual(plain.bounded_array, [10, 20]);
    assert.equal(plain.boxes.length, 2);
    const raw = await request(true);
    assert.equal(raw.domain_value, '17');
    assert.equal(raw.range_value, '[1,4)');
    assert.match(raw.bounded_array, /^\[2:3\]=/);
    assert.match(raw.boxes, /;/);
  } finally {
    await sql.query(`drop type ${range}`);
    await sql.query(`drop type ${composite}`);
    await sql.query(`drop domain ${domain}`);
  }
});

test('Neon WebSocket Client authenticates and runs a query', async () => {
  const client = new Client(databaseUrl);
  await client.connect();
  try {
    const result = await client.query('select $1::int as answer', [42]);
    assert.equal(result.rows[0].answer, 42);
  } finally {
    await client.end();
  }
});
