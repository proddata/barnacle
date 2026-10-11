import { execFile } from 'node:child_process';
import { performance } from 'node:perf_hooks';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { Client, neon, neonConfig } from '@neondatabase/serverless';
import WebSocket from 'ws';

const exec = promisify(execFile);
const root = fileURLToPath(new URL('..', import.meta.url));
const options = Object.fromEntries(process.argv.slice(2).map((arg) => {
  const [key, value] = arg.replace(/^--/, '').split('=');
  return [key, value];
}));
const transport = options.transport || 'http';
const workload = options.workload || 'queries';
const memorySource = options['memory-source'] || 'docker';
const connections = Number(options.connections || 1);
const targetQps = Number(options.qps || 10);
const connectRate = Number(options['connect-rate'] || 20);
const connecting = Number(options.connecting || 8);
const holdMs = Number(options['hold-ms'] || 500);
const seconds = Number(options.seconds || 10);
const payloadBytes = Number(options['payload-bytes'] || 0);
const sleepMs = Number(options['sleep-ms'] || 0);
if (!['http', 'ws'].includes(transport) || !['queries', 'churn'].includes(workload) ||
    !['docker', 'systemd'].includes(memorySource) ||
    ![connections, targetQps, connectRate, connecting, holdMs, seconds, payloadBytes, sleepMs].every(Number.isInteger) ||
    connections < 1 || targetQps < 1 || connectRate < 1 || connecting < 1 ||
    holdMs < 0 || seconds < 1 || payloadBytes < 0 || sleepMs < 0 ||
    (workload === 'churn' && (transport !== 'ws' || payloadBytes !== 0 || sleepMs !== 0))) {
  throw new Error('Use --workload=queries|churn --transport=http|ws --connections=N --seconds=N; queries: --qps=N [--payload-bytes=N] [--sleep-ms=N]; WS churn: --connect-rate=N --connecting=N --hold-ms=N; optional --memory-source=docker|systemd');
}

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL ||
  'postgres://barnacle:barnacle_dev_password@postgres:5432/barnacle';
const endpoint = new URL(base);
neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;

let containerId;
if (memorySource === 'docker') {
  const { stdout: idOutput } = await exec('docker', ['compose', 'ps', '-q', 'barnacle'], { cwd: root });
  containerId = idOutput.trim();
  if (!containerId) throw new Error('Start the Compose stack first: docker compose up --build -d');
}

function bytesFromDocker(value) {
  const match = value.match(/^([\d.]+)\s*(B|KiB|MiB|GiB|kB|MB|GB)/);
  if (!match) throw new Error(`Cannot parse Docker memory usage: ${value}`);
  const factor = { B: 1, KiB: 1024, MiB: 1024 ** 2, GiB: 1024 ** 3,
    kB: 1000, MB: 1000 ** 2, GB: 1000 ** 3 }[match[2]];
  return Number(match[1]) * factor;
}

async function containerMemory() {
  if (memorySource === 'systemd') {
    const { stdout } = await exec('systemctl',
      ['show', 'barnacle', '--property', 'MemoryCurrent', '--value']);
    const bytes = Number(stdout.trim());
    if (!Number.isFinite(bytes) || bytes <= 0) throw new Error(`Cannot read barnacle.service memory: ${stdout.trim()}`);
    return bytes;
  }
  const { stdout } = await exec('docker',
    ['stats', '--no-stream', '--format', '{{.MemUsage}}', containerId]);
  return bytesFromDocker(stdout.trim());
}

async function cgroupPeakMemory() {
  const result = memorySource === 'systemd'
    ? await exec('systemctl', ['show', 'barnacle', '--property', 'MemoryPeak', '--value'])
    : await exec('docker', ['exec', containerId, 'cat', '/sys/fs/cgroup/memory.peak']);
  const bytes = Number(result.stdout.trim());
  if (!Number.isFinite(bytes) || bytes <= 0) throw new Error(`Cannot read cgroup memory peak: ${result.stdout.trim()}`);
  return bytes;
}

const baselineBytes = await containerMemory();
let peakBytes = baselineBytes;
let samples = 1;
let sampleError;
let sampling = false;
const sampler = setInterval(async () => {
  if (sampling) return;
  sampling = true;
  try {
    peakBytes = Math.max(peakBytes, await containerMemory());
    samples++;
  } catch (error) {
    sampleError = String(error);
  } finally {
    sampling = false;
  }
}, 500);

function percentile(latencies, p) {
  return latencies.length ? Number(latencies[Math.ceil(p * latencies.length) - 1].toFixed(1)) : null;
}

async function runQueries() {
  const clients = [];
  let completed = 0;
  let errors = 0;
  let skipped = 0;
  let issued = 0;
  let inFlight = 0;
  let maxInFlight = 0;
  const latencies = [];
  const pending = new Set();
  try {
    if (transport === 'ws') {
      for (let i = 0; i < connections; i++) {
        const client = new Client(databaseUrl);
        await client.connect();
        clients.push(client);
      }
    }
    const sql = transport === 'http' ? neon(databaseUrl) : null;
    const query = `select ${sleepMs > 0 ? 'pg_sleep($1::double precision), ' : ''}${payloadBytes > 0 ? `repeat('x', $${sleepMs > 0 ? 2 : 1}::int) as payload` : '1::int as answer'}`;
    const params = [...(sleepMs > 0 ? [sleepMs / 1000] : []), ...(payloadBytes > 0 ? [payloadBytes] : [])];

    async function perform(index) {
      const start = performance.now();
      try {
        if (transport === 'http') {
          await sql.query(query, params);
        } else {
          await clients[index % clients.length].query(query, params);
        }
        completed++;
        latencies.push(performance.now() - start);
      } catch (error) {
        errors++;
        if (errors <= 3) process.stderr.write(`${error}\n`);
      } finally {
        inFlight--;
      }
    }

    const start = performance.now();
    const deadline = start + seconds * 1000;
    await new Promise((resolve) => {
      const timer = setInterval(() => {
        const now = performance.now();
        const due = Math.min(Math.floor((now - start) * targetQps / 1000), targetQps * seconds);
        while (issued < due) {
          const index = issued++;
          if (inFlight >= connections) {
            skipped++;
            continue;
          }
          inFlight++;
          maxInFlight = Math.max(maxInFlight, inFlight);
          const task = perform(index);
          pending.add(task);
          task.finally(() => pending.delete(task));
        }
        if (now >= deadline) {
          clearInterval(timer);
          resolve();
        }
      }, 10);
    });
    await Promise.allSettled([...pending]);
    const duration = (performance.now() - start) / 1000;
    latencies.sort((a, b) => a - b);
    return { transport, workload, connections, targetQps, payloadBytes, sleepMs, maxInFlight,
      durationSeconds: Number(duration.toFixed(2)), completed, errors, skipped,
      achievedQps: Number((completed / duration).toFixed(1)),
      p50Ms: percentile(latencies, 0.5), p95Ms: percentile(latencies, 0.95), p99Ms: percentile(latencies, 0.99) };
  } finally {
    await Promise.allSettled(clients.map((client) => client.end()));
  }
}

async function runChurn() {
  let opened = 0;
  let errors = 0;
  let rejected = 0;
  let skipped = 0;
  let issued = 0;
  let opening = 0;
  let live = 0;
  let maxOpening = 0;
  let maxOpen = 0;
  const latencies = [];
  const pending = new Set();
  const clients = new Set();
  const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  async function connect() {
    const client = new Client({ connectionString: databaseUrl, connectionTimeoutMillis: 10000 });
    clients.add(client);
    const start = performance.now();
    let connected = false;
    try {
      await client.connect();
      opening--;
      live++;
      connected = true;
      maxOpen = Math.max(maxOpen, live);
      opened++;
      latencies.push(performance.now() - start);
      await pause(holdMs);
    } catch (error) {
      if (!connected) opening--;
      errors++;
      const detail = error?.error?.message || error?.message || String(error);
      if (error?.statusCode === 503 || /(?:response|status).*503/i.test(detail)) rejected++;
      if (errors <= 3) process.stderr.write(`${detail}\n`);
    } finally {
      if (connected) {
        try {
          await client.end();
        } catch (error) {
          if (errors <= 3) process.stderr.write(`close: ${error}\n`);
        }
      }
      clients.delete(client);
      if (connected) live--;
    }
  }

  const start = performance.now();
  const deadline = start + seconds * 1000;
  try {
    await new Promise((resolve) => {
      const timer = setInterval(() => {
        const now = performance.now();
        const due = Math.min(Math.floor((now - start) * connectRate / 1000), connectRate * seconds);
        while (issued < due) {
          issued++;
          if (opening >= connecting || opening + live >= connections) {
            skipped++;
            continue;
          }
          opening++;
          maxOpening = Math.max(maxOpening, opening);
          const task = connect();
          pending.add(task);
          task.finally(() => pending.delete(task));
        }
        if (now >= deadline) {
          clearInterval(timer);
          resolve();
        }
      }, 10);
    });
    await Promise.allSettled([...pending]);
  } finally {
    await Promise.allSettled([...clients].map((client) => client.end()));
  }
  latencies.sort((a, b) => a - b);
  return { transport, workload, connections, connectRate, connecting, holdMs, maxOpening, maxOpen,
    durationSeconds: Number(((performance.now() - start) / 1000).toFixed(2)),
    offered: issued, opened, errors, rejected, skipped,
    achievedConnectionsPerSecond: Number((opened / seconds).toFixed(1)),
    connectP50Ms: percentile(latencies, 0.5), connectP95Ms: percentile(latencies, 0.95),
    connectP99Ms: percentile(latencies, 0.99) };
}

try {
  const result = workload === 'churn' ? await runChurn() : await runQueries();
  peakBytes = Math.max(peakBytes, await containerMemory());
  let cgroupPeakMiB = null;
  try {
    cgroupPeakMiB = Number(((await cgroupPeakMemory()) / 1024 ** 2).toFixed(1));
  } catch (error) {
    sampleError = String(error);
  }
  console.log(JSON.stringify({ ...result, memorySource,
    baselineMiB: Number((baselineBytes / 1024 ** 2).toFixed(1)),
    peakMiB: Number((peakBytes / 1024 ** 2).toFixed(1)), cgroupPeakMiB, memorySamples: samples,
    ...(sampleError ? { sampleError } : {}) }));
} finally {
  clearInterval(sampler);
}
