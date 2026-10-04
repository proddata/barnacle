import { Client, neon, neonConfig } from '@neondatabase/serverless';

neonConfig.fetchEndpoint = `${location.origin}/sql`;
neonConfig.wsProxy = `${location.host}/v2`;
neonConfig.useSecureWebSocket = location.protocol === 'https:';
neonConfig.forceDisablePgSSL = true; // Hermit verifies TLS on its upstream connection.
neonConfig.pipelineConnect = false; // Let PostgreSQL challenge SCRAM or MD5 logins.

let nextTraceId = 0;
const pendingHTTP = [];
neonConfig.fetchFunction = async (input, init) => {
  const body = String(init?.body ?? '');
  let query;
  try { query = JSON.parse(body).query; } catch { /* Let fetch handle an unexpected body. */ }
  const sentURL = new URL(String(init?.headers?.['Neon-Connection-String'] ?? 'postgres://invalid/'));
  const capture = pendingHTTP.find((item) => !item.captured && item.bodyQuery === query &&
    item.url.host === sentURL.host && item.url.pathname === sentURL.pathname &&
    item.url.username === sentURL.username && item.url.password === sentURL.password);
  if (capture) {
    capture.captured = true;
    capture.trace.endpoint = String(input);
    capture.trace.request = {
      method: init.method ?? 'GET',
      headers: Object.fromEntries(Object.entries(init.headers ?? {}).map(([name, value]) => [name,
        name.toLowerCase() === 'authorization' ? 'Bearer [redacted]' :
        name.toLowerCase() === 'neon-connection-string' ? visibleConnectionString(String(value)) : String(value),
      ])),
      body,
    };
    capture.report({ ...capture.trace });
  }
  return fetch(input, init);
};

export const TABLES_SQL = `select n.nspname as schema_name, c.relname as table_name,
       c.relkind as relation_kind
from pg_catalog.pg_class c
join pg_catalog.pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p', 'v', 'm', 'f')
  and n.nspname not in ('pg_catalog', 'information_schema')
  and n.nspname not like 'pg_toast%'
order by n.nspname, c.relname`;

export const COLUMNS_SQL = `select a.attname as column_name,
       pg_catalog.format_type(a.atttypid, a.atttypmod) as data_type,
       a.attnotnull as not_null
from pg_catalog.pg_attribute a
join pg_catalog.pg_class c on c.oid = a.attrelid
join pg_catalog.pg_namespace n on n.oid = c.relnamespace
where n.nspname = $1 and c.relname = $2
  and a.attnum > 0 and not a.attisdropped
order by a.attnum`;

export function quoteIdentifier(value) {
  return `"${String(value).replaceAll('"', '""')}"`;
}

export function connectionString(settings) {
  const host = settings.host.trim();
  const username = settings.username.trim();
  const database = settings.database.trim();
  if (!host || !username || !database) throw new Error('Host, database, and user are required.');
  if (settings.method !== 'http-bearer' && !settings.password) throw new Error('Enter a password.');
  if (settings.method === 'http-bearer' && !settings.token.trim()) throw new Error('Enter a bearer token.');
  let url;
  try { url = new URL(`postgres://${host}/`); }
  catch { throw new Error('Enter a PostgreSQL host and port, such as postgres:5432.'); }
  if (!url.hostname || !url.port || url.username || url.password || url.pathname !== '/' || url.search || url.hash) {
    throw new Error('Enter only a PostgreSQL host and port, such as postgres:5432.');
  }
  url.username = username;
  url.password = settings.method === 'http-bearer' ? '' : settings.password;
  url.pathname = `/${encodeURIComponent(database)}`;
  if (settings.method === 'http-password' && settings.authMethod) {
    if (!['scram-sha-256', 'md5'].includes(settings.authMethod)) throw new Error('Unsupported required auth method.');
    url.searchParams.set('require_auth', settings.authMethod);
  }
  return url.toString();
}

function visibleConnectionString(raw) {
  const url = new URL(raw);
  if (url.password) url.password = '[redacted]';
  return url.toString();
}

export function safeJSON(value, space = 2) {
  return JSON.stringify(value, (_key, item) => typeof item === 'bigint' ? item.toString() : item, space);
}

export function safeError(error, settings) {
  let message = error?.message || String(error);
  for (const secret of [settings.password, settings.token]) {
    if (secret) message = message.replaceAll(secret, '[redacted]');
  }
  return message;
}

export async function executeQuery(settings, sql, params = [], report = () => {}, purpose = 'query') {
  const url = connectionString(settings);
  const websocket = settings.method === 'ws-password';
  const address = new URL(url);
  const trace = {
    id: ++nextTraceId,
    purpose,
    state: 'running',
    startedAt: new Date().toLocaleTimeString(),
    durationMs: null,
    transport: websocket ? 'WebSocket' : 'HTTP',
    api: websocket ? 'Client.connect() → Client.query() → Client.end()' : 'neon().query()',
    endpoint: websocket ? `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/v2?address=${encodeURIComponent(address.host)}` : `${location.origin}/sql`,
    request: websocket ? {
      startup: { user: settings.username.trim(), database: settings.database.trim(), password: '[redacted]' },
      query: sql, params,
    } : null,
    response: null,
  };
  report({ ...trace });
  const capture = websocket ? null : { bodyQuery: sql, url: address, trace, report, captured: false };
  if (capture) pendingHTTP.push(capture);
  const started = performance.now();
  try {
    let result;
    if (websocket) {
      const client = new Client({ connectionString: url });
      try {
        await client.connect();
        result = await client.query(sql, params);
      } finally {
        await client.end().catch(() => {});
      }
    } else {
      const query = neon(url, settings.method === 'http-bearer' ? { authToken: settings.token.trim() } : {});
      result = await query.query(sql, params, { fullResults: true });
    }
    trace.state = 'success';
    trace.durationMs = Math.round(performance.now() - started);
    trace.response = {
      command: result.command ?? null,
      rowCount: result.rowCount ?? result.rows?.length ?? null,
      fields: (result.fields ?? []).map((field) => ({ name: field.name, dataTypeID: field.dataTypeID })),
      rows: (result.rows ?? []).slice(0, 20),
      rowsTruncated: (result.rows?.length ?? 0) > 20,
    };
    report({ ...trace });
    return result;
  } catch (error) {
    trace.state = 'error';
    trace.durationMs = Math.round(performance.now() - started);
    trace.response = { error: safeError(error, settings), code: error?.code ?? null };
    report({ ...trace });
    throw error;
  } finally {
    if (capture) {
      const index = pendingHTTP.indexOf(capture);
      if (index >= 0) pendingHTTP.splice(index, 1);
    }
  }
}
