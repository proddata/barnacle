// Built-in PostgreSQL types only: these OIDs and expressions can be compared
// across independent databases without installing schema objects.
const scalar = (name, expression, plain, raw) => ({
  name, query: `select ${expression} as value`, params: [], plain, raw,
});
const parameter = (name, type, value, plain, raw) => ({
  name: `parameter ${name}`, query: `select $1::${type} as value`,
  params: [value], plain, raw,
});

export const typeMatrix = [
  scalar('boolean true', 'true::boolean', true, 't'),
  scalar('boolean false', 'false::boolean', false, 'f'),
  scalar('smallint minimum', '(-32768)::int2', -32768, '-32768'),
  scalar('integer maximum', '2147483647::int4', 2147483647, '2147483647'),
  scalar('bigint maximum', '9223372036854775807::int8', '9223372036854775807', '9223372036854775807'),
  scalar('numeric precision', '12345678901234567890.123456789::numeric', '12345678901234567890.123456789', '12345678901234567890.123456789'),
  scalar('real', '1.5::float4', 1.5, '1.5'),
  scalar('double precision', '-2.25::float8', -2.25, '-2.25'),
  scalar('float NaN', "'NaN'::float8", 'NaN', 'NaN'),
  scalar('float positive infinity', "'Infinity'::float8", 'Infinity', 'Infinity'),
  scalar('float negative infinity', "'-Infinity'::float8", '-Infinity', '-Infinity'),
  scalar('text quoting', String.raw`$q$a,b"\z$q$::text`, 'a,b"\\z', 'a,b"\\z'),
  scalar('empty text', "''::text", '', ''),
  scalar('UUID', "'123e4567-e89b-12d3-a456-426614174000'::uuid", '123e4567-e89b-12d3-a456-426614174000', '123e4567-e89b-12d3-a456-426614174000'),
  scalar('date', "'2024-02-29'::date", '2024-02-29', '2024-02-29'),
  scalar('time', "'23:59:59.123456'::time", '23:59:59.123456', '23:59:59.123456'),
  scalar('timestamp', "'2024-02-29 12:34:56.123456'::timestamp", '2024-02-29 12:34:56.123456', '2024-02-29 12:34:56.123456'),
  scalar('interval', "'1 day 02:03:04'::interval", '1 day 02:03:04', '1 day 02:03:04'),
  scalar('bytea', "decode('00ff5c', 'hex')::bytea", String.raw`\x00ff5c`, String.raw`\x00ff5c`),
  scalar('bit', "B'1010'::bit(4)", '1010', '1010'),
  scalar('varbit', "B'101001'::varbit", '101001', '101001'),
  scalar('inet', "'192.0.2.1/24'::inet", '192.0.2.1/24', '192.0.2.1/24'),
  scalar('cidr', "'192.0.2.0/24'::cidr", '192.0.2.0/24', '192.0.2.0/24'),
  scalar('macaddr', "'08:00:2b:01:02:03'::macaddr", '08:00:2b:01:02:03', '08:00:2b:01:02:03'),
  scalar('JSON', String.raw`'{"x":[1,null,true]}'::json`, { x: [1, null, true] }, '{"x":[1,null,true]}'),
  scalar('JSONB', String.raw`'{"b":2,"a":[1,null]}'::jsonb`, { a: [1, null], b: 2 }),
  scalar('point', 'point(1, 2)', '(1,2)', '(1,2)'),
  scalar('integer range', "'[1,5)'::int4range", '[1,5)', '[1,5)'),
  scalar('integer multirange', "'{[1,5)}'::int4multirange", '{[1,5)}', '{[1,5)}'),
  scalar('boolean array', 'array[true,false,null]::bool[]', [true, false, null]),
  scalar('integer array', 'array[1,null,-2]::int4[]', [1, null, -2]),
  scalar('bigint array', 'array[9223372036854775807::int8,null]::int8[]', ['9223372036854775807', null]),
  scalar('numeric array', 'array[1.25::numeric,null,-2.5::numeric]::numeric[]', ['1.25', null, '-2.5']),
  scalar('float array', "array[1.5::float8,'NaN'::float8,'Infinity'::float8,'-Infinity'::float8]", [1.5, 'NaN', 'Infinity', '-Infinity']),
  scalar('quoted text array', String.raw`array[$q$a,b$q$,$q$NULL$q$,null,$q$a"b$q$,$q$x\y$q$,'']::text[]`, ['a,b', 'NULL', null, 'a"b', 'x\\y', '']),
  scalar('UUID array', "array['123e4567-e89b-12d3-a456-426614174000'::uuid,null]", ['123e4567-e89b-12d3-a456-426614174000', null]),
  scalar('JSONB array', String.raw`array['{"a":1}'::jsonb,null,'[true,2]'::jsonb]`, [{ a: 1 }, null, [true, 2]]),
  scalar('nested integer array', 'array[[1,2],[3,4]]::int4[][]', [[1, 2], [3, 4]]),
  scalar('empty integer array', 'array[]::int4[]', []),
  scalar('array bounds', "'[2:3]={10,20}'::int4[]", [10, 20]),
  scalar('box array delimiter', 'array[box(point(0,0),point(1,1)),box(point(2,2),point(3,3))]::box[]', ['(1,1),(0,0)', '(3,3),(2,2)']),
  parameter('null integer', 'int4', null, null, null),
  parameter('boolean', 'boolean', true, true, 't'),
  parameter('integer', 'int4', 42, 42, '42'),
  parameter('precise numeric', 'numeric', '12345678901234567890.123456789', '12345678901234567890.123456789', '12345678901234567890.123456789'),
  parameter('quoted text', 'text', 'a,b"\\z', 'a,b"\\z', 'a,b"\\z'),
  parameter('UUID', 'uuid', '123e4567-e89b-12d3-a456-426614174000', '123e4567-e89b-12d3-a456-426614174000', '123e4567-e89b-12d3-a456-426614174000'),
  parameter('bytea', 'bytea', String.raw`\x00ff5c`, String.raw`\x00ff5c`, String.raw`\x00ff5c`),
  parameter('JSONB object', 'jsonb', { a: [1, null], b: true }, { a: [1, null], b: true }),
  parameter('boolean array', 'bool[]', [true, false, null], [true, false, null]),
  parameter('integer array', 'int4[]', [1, null, -2], [1, null, -2]),
  parameter('empty integer array', 'int4[]', [], []),
  parameter('nested integer array', 'int4[][]', [[1, 2], [3, 4]], [[1, 2], [3, 4]]),
  parameter('quoted text array', 'text[]', ['a,b', 'NULL', null, 'a"b', 'x\\y', ''], ['a,b', 'NULL', null, 'a"b', 'x\\y', '']),
  parameter('JSONB array', 'jsonb[]', [{ a: 1 }, null, { b: [2] }], [{ a: 1 }, null, { b: [2] }]),
];

export async function queryMatrix(endpoint, databaseUrl, item, raw) {
  const response = await fetch(endpoint, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Neon-Connection-String': databaseUrl,
      ...(raw ? { 'Neon-Raw-Text-Output': 'true' } : {}),
    },
    body: JSON.stringify({ query: item.query, params: item.params }),
  });
  const body = await response.json();
  if (!response.ok) {
    throw new Error(`${item.name} ${raw ? 'raw' : 'plain'}: HTTP ${response.status}, ${body.code || 'unknown code'}: ${body.message || 'no message'}`);
  }
  return body;
}
