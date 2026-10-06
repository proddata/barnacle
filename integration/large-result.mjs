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
const rows = Number(options.rows || 100);
const rowMiB = Number(options['row-mib'] || 1);
if (!['http', 'ws'].includes(transport) || !Number.isInteger(rows) ||
    !Number.isInteger(rowMiB) || rows < 1 || rowMiB < 1) {
  throw new Error('Use --transport=http|ws [--rows=100] [--row-mib=1]');
}

const base = process.env.BARNACLE_BASE_URL || 'http://127.0.0.1:8080';
const databaseUrl = process.env.TEST_DATABASE_URL ||
  'postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle';
const endpoint = new URL(base);
neonConfig.fetchEndpoint = `${base}/sql`;
neonConfig.webSocketConstructor = WebSocket;
neonConfig.useSecureWebSocket = endpoint.protocol === 'https:';
neonConfig.wsProxy = () => `${endpoint.host}/v2`;
neonConfig.pipelineConnect = false;
neonConfig.forceDisablePgSSL = true;

const { stdout: idOutput } = await exec('docker', ['compose', 'ps', '-q', 'barnacle'], { cwd: root });
const containerId = idOutput.trim();
if (!containerId) throw new Error('Start the Compose stack first: docker compose up --build -d');

async function cgroupValue(name) {
  const { stdout } = await exec('docker', ['exec', containerId, 'cat', `/sys/fs/cgroup/${name}`]);
  const value = Number(stdout.trim());
  if (!Number.isFinite(value)) throw new Error(`Invalid ${name}: ${stdout.trim()}`);
  return value;
}

const baseline = await cgroupValue('memory.current');
let sampledPeak = baseline;
let clientPeak = process.memoryUsage().rss;
let samplePending = false;
const sampler = setInterval(async () => {
  clientPeak = Math.max(clientPeak, process.memoryUsage().rss);
  if (samplePending) return;
  samplePending = true;
  try {
    sampledPeak = Math.max(sampledPeak, await cgroupValue('memory.current'));
  } catch {
    // A memory-killed container cannot be sampled. Inspect its exit state below.
  } finally {
    samplePending = false;
  }
}, 200);

const query = 'select repeat(md5(i::text), $1::int) as payload from generate_series(1, $2::int) as i';
const params = [rowMiB * 32768, rows]; // md5 produces 32 ASCII bytes.
let success = false;
let error;
let resultBytes = 0;
let resultRows = 0;
let client;
const started = performance.now();

try {
  let result;
  if (transport === 'http') {
    result = await neon(databaseUrl).query(query, params);
  } else {
    client = new Client(databaseUrl);
    await client.connect();
    result = (await client.query(query, params)).rows;
  }
  clientPeak = Math.max(clientPeak, process.memoryUsage().rss);
  resultRows = result.length;
  for (const row of result) resultBytes += Buffer.byteLength(row.payload);
  success = resultRows === rows && resultBytes === rows * rowMiB * 1024 ** 2;
  if (!success) error = `Unexpected result: ${resultRows} rows, ${resultBytes} bytes`;
} catch (cause) {
  error = String(cause?.cause || cause);
} finally {
  clearInterval(sampler);
  if (client) await client.end().catch(() => {});
}

const durationSeconds = (performance.now() - started) / 1000;
let cgroupPeak = null;
try {
  cgroupPeak = await cgroupValue('memory.peak');
} catch {
  // OOM-killed containers cannot answer docker exec.
}
const { stdout: stateOutput } = await exec('docker', ['inspect', '--format',
  '{{.HostConfig.Memory}} {{.State.OOMKilled}} {{.State.ExitCode}} {{.State.Running}}', containerId]);
const [limit, oomKilled, exitCode, running] = stateOutput.trim().split(/\s+/);
const mib = (bytes) => bytes === null ? null : Number((bytes / 1024 ** 2).toFixed(1));
console.log(JSON.stringify({ transport, requestedMiB: rows * rowMiB,
  resultMiB: mib(resultBytes), resultRows, success, durationSeconds: Number(durationSeconds.toFixed(2)),
  barnacleLimitMiB: mib(Number(limit)), barnacleBaselineMiB: mib(baseline),
  barnacleSampledPeakMiB: mib(sampledPeak), barnacleCgroupPeakMiB: mib(cgroupPeak),
  clientPeakMiB: mib(clientPeak), oomKilled: oomKilled === 'true',
  containerExitCode: Number(exitCode), containerRunning: running === 'true',
  ...(error ? { error } : {}) }));
if (!success) process.exitCode = 1;
