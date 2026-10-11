// Opt-in benchmark: each request authenticates a fresh PostgreSQL connection.
import http from 'node:http';
import { performance } from 'node:perf_hooks';

if (!process.env.TEST_DATABASE_URL) throw new Error('Set TEST_DATABASE_URL');
const base = new URL(process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080');
const agent = new http.Agent({ keepAlive: true, maxSockets: 8 });
const bodies = {
  simple: { query: 'select 1 as answer', params: [] },
  scalars: {
    query: 'select $1::int, $2::text, $3::bool, $4::bigint',
    params: [42, 'hello', true, '9007199254740993'],
  },
  structured: {
    query: 'select $1::jsonb, $2::int[]',
    params: [{ x: 1, y: [1, 2, 3], z: 'hello' }, [[1, 2], [3, 4]]],
  },
  text64KiB: { query: 'select length($1::text)', params: ['x'.repeat(65536)] },
  batch10: { queries: Array.from({ length: 10 }, () => ({ query: 'select $1::int', params: [42] })) },
};

function query(body) {
  return new Promise((resolve, reject) => {
    const start = performance.now();
    const req = http.request(new URL('/sql', base), {
      method: 'POST',
      agent,
      headers: {
        'Neon-Connection-String': process.env.TEST_DATABASE_URL,
        'Content-Length': Buffer.byteLength(body),
      },
    }, (response) => {
      let data = '';
      response.on('data', (chunk) => { data += chunk; });
      response.on('error', reject);
      response.on('end', () => {
        if (response.statusCode === 200) resolve(performance.now() - start);
        else reject(new Error(`${response.statusCode} ${data}`));
      });
    });
    req.on('error', reject);
    req.end(body);
  });
}

async function run(body, count, concurrency) {
  let next = 0;
  const samples = [];
  const start = performance.now();
  await Promise.all(Array.from({ length: concurrency }, async () => {
    while (next++ < count) samples.push(await query(body));
  }));
  samples.sort((a, b) => a - b);
  return {
    rps: count * 1000 / (performance.now() - start),
    p50: samples[Math.floor(samples.length * 0.5)],
    p95: samples[Math.floor(samples.length * 0.95)],
  };
}

try {
  const result = {};
  for (const [name, value] of Object.entries(bodies)) {
    const body = JSON.stringify(value);
    for (const concurrency of [1, 8]) {
      await run(body, 16, concurrency);
      result[`${name}/c${concurrency}`] = await run(body, concurrency === 1 ? 120 : 320, concurrency);
    }
  }
  console.log(JSON.stringify(result));
} finally {
  agent.destroy();
}
