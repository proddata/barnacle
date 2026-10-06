import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { neonConfig } from '@neondatabase/serverless';
import { queryMatrix, typeMatrix } from './type-matrix.mjs';

// A disposable Neon URL can live in integration/.env, which is gitignored.
const envPath = fileURLToPath(new URL('.env', import.meta.url));
const settings = { ...process.env };
try {
  const contents = await readFile(envPath, 'utf8');
  for (const line of contents.split(/\r?\n/)) {
    const match = line.match(/^\s*(?:export\s+)?(NEON_COMPARE_DATABASE_URL|NEON_COMPARE_ENDPOINT)=(.*)\s*$/);
    if (!match || settings[match[1]]) continue;
    let value = match[2].trim();
    if ((value.startsWith('"') && value.endsWith('"')) ||
        (value.startsWith("'") && value.endsWith("'"))) value = value.slice(1, -1);
    settings[match[1]] = value;
  }
} catch (error) {
  if (error.code !== 'ENOENT') throw error;
}

const remoteDatabaseUrl = settings.NEON_COMPARE_DATABASE_URL;
if (!remoteDatabaseUrl) {
  throw new Error('Put NEON_COMPARE_DATABASE_URL in the gitignored integration/.env');
}
const parsed = new URL(remoteDatabaseUrl);
const defaultEndpoint = neonConfig.fetchEndpoint;
const remoteEndpoint = settings.NEON_COMPARE_ENDPOINT ||
  defaultEndpoint(parsed.hostname, parsed.port);
const localEndpoint = `${settings.BARNACLE_BASE_URL || 'http://127.0.0.1:8080'}/sql`;
const localDatabaseUrl = settings.TEST_DATABASE_URL ||
  'postgres://barnacle:barnacle_dev_password@localhost:5432/barnacle';

const differences = [];
const knownNeonDifferences = [];
let matchingVariants = 0;
for (const item of typeMatrix) {
  for (const raw of [false, true]) {
    const label = `${item.name} (${raw ? 'raw' : 'plain'})`;
    let local;
    try {
      local = await queryMatrix(localEndpoint, localDatabaseUrl, item, raw);
    } catch (error) {
      differences.push(`${label}: Barnacle failed: ${error.message}`);
      continue;
    }
    let remote;
    try {
      remote = await queryMatrix(remoteEndpoint, remoteDatabaseUrl, item, raw);
    } catch (error) {
      if (!raw && ['empty integer array', 'parameter empty integer array'].includes(item.name) &&
          error.message.includes('HTTP 500') && error.message.includes('could not parse postgres response')) {
        knownNeonDifferences.push(`${label}: Neon returns HTTP 500 for an empty array`);
      } else {
        differences.push(`${label}: Neon failed: ${error.message}`);
      }
      continue;
    }
    let matched = true;
    for (const key of ['rows', 'fields', 'command', 'rowCount', 'rowAsArray']) {
      try {
        assert.deepEqual(local[key], remote[key]);
      } catch {
        matched = false;
        const detail = `${label}: ${key} differs\n  Barnacle: ${JSON.stringify(local[key])}\n  Neon:   ${JSON.stringify(remote[key])}`;
        if (!raw && item.name === 'box array delimiter' && key === 'rows') {
          knownNeonDifferences.push(detail);
        } else {
          differences.push(detail);
        }
      }
    }
    if (matched) matchingVariants++;
  }
}

process.stdout.write(`${matchingVariants}/${typeMatrix.length * 2} raw/plain variants matched Neon.\n`);
if (knownNeonDifferences.length) {
  process.stdout.write(`${knownNeonDifferences.length} known Neon plain-array differences:\n${knownNeonDifferences.join('\n')}\n`);
}
if (differences.length) {
  process.stderr.write(`${differences.length} unexpected differences across ${typeMatrix.length} type cases:\n`);
  process.stderr.write(differences.join('\n') + '\n');
  process.exitCode = 1;
}
