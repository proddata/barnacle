import { Client, neon, neonConfig } from '@neondatabase/serverless';

neonConfig.fetchEndpoint = `${location.origin}/sql`;
neonConfig.wsProxy = `${location.host}/v2`;
neonConfig.useSecureWebSocket = location.protocol === 'https:';
neonConfig.forceDisablePgSSL = true; // Barnacle verifies TLS on its upstream connection.
neonConfig.pipelineConnect = false; // Let PostgreSQL challenge SCRAM or MD5 logins.

let nextTraceId = 0;
const pendingHTTP = [];
neonConfig.fetchFunction = async (input, init) => {
  const body = String(init?.body ?? '');
  let query;
  let queries;
  try {
    const parsed = JSON.parse(body);
    query = parsed.query;
    queries = parsed.queries?.map((item) => item.query);
  } catch { /* Let fetch handle an unexpected body. */ }
  const sentURL = new URL(String(init?.headers?.['Neon-Connection-String'] ?? 'postgres://invalid/'));
  const capture = pendingHTTP.find((item) => !item.captured &&
    (item.bodyQueries ? Array.isArray(queries) && item.bodyQueries.length === queries.length &&
      item.bodyQueries.every((value, index) => value === queries[index]) : item.bodyQuery === query) &&
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

function resultPreview(result) {
  return {
    command: result.command ?? null,
    rowCount: result.rowCount ?? result.rows?.length ?? null,
    fields: (result.fields ?? []).map((field) => ({ name: field.name, dataTypeID: field.dataTypeID })),
    rows: (result.rows ?? []).slice(0, 20),
    rowsTruncated: (result.rows?.length ?? 0) > 20,
  };
}

export async function closeQuerySession(session) {
  const client = session?.client;
  if (!session) return;
  session.client = null;
  session.url = null;
  session.connection = null;
  session.status = 'disconnected';
  session.error = '';
  session.onChange?.();
  if (client) await client.end().catch(() => {});
}

export async function openQuerySession(settings, session) {
  if (!session) throw new Error('A query tab is required for a WebSocket connection.');
  if (settings.method !== 'ws-password') throw new Error('Select WebSocket transport to connect.');
  const url = connectionString(settings);
  if (session.client && session.url === url && session.status === 'connected') return session.client;
  if (session.client) await closeQuerySession(session);
  const client = new Client({ connectionString: url });
  const connection = { parameters: [], backendProcessID: null };
  session.client = client;
  session.url = url;
  session.connection = null;
  session.status = 'connecting';
  session.error = '';
  session.onChange?.();
  const onParameterStatus = ({ parameterName, parameterValue }) => {
    connection.parameters.push({ name: parameterName, value: parameterValue });
  };
  const onBackendKeyData = ({ processID }) => { connection.backendProcessID = processID; };
  client.connection.on('parameterStatus', onParameterStatus);
  client.connection.on('backendKeyData', onBackendKeyData);
  const forgetClosedSession = () => {
    if (session.client !== client) return;
    if (session.status === 'connecting') return;
    session.client = null;
    session.url = null;
    session.connection = null;
    session.status = 'disconnected';
    session.onChange?.();
  };
  client.on('error', forgetClosedSession);
  client.on('end', forgetClosedSession);
  try {
    await client.connect();
    if (session.client !== client) {
      await client.end().catch(() => {});
      throw new Error('Connection was closed while connecting.');
    }
    session.connection = connection;
    session.status = 'connected';
    session.onChange?.();
    return client;
  } catch (error) {
    if (session.client === client) {
      session.client = null;
      session.url = null;
      session.connection = null;
      session.status = 'failed';
      session.error = safeError(error, settings);
      session.onChange?.();
    }
    await client.end().catch(() => {});
    throw error;
  } finally {
    client.connection.off('parameterStatus', onParameterStatus);
    client.connection.off('backendKeyData', onBackendKeyData);
  }
}

export async function executeQuery(settings, sql, params = [], report = () => {}, purpose = 'query', session = null) {
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
    api: websocket ? 'Client.connect() → Client.query()' : 'neon().query()',
    endpoint: websocket ? `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/v2?address=${encodeURIComponent(address.host)}` : `${location.origin}/sql`,
    request: websocket ? { query: sql, params } : null,
    connectionReused: false,
    response: null,
  };
  report({ ...trace });
  const capture = websocket ? null : { bodyQuery: sql, url: address, trace, report, captured: false };
  if (capture) pendingHTTP.push(capture);
  const started = performance.now();
  try {
    let result;
    if (websocket) {
      const existingConnection = session?.client && session.url === url && session.status === 'connected';
      const client = session ? await openQuerySession(settings, session) : await openQuerySession(settings, {
        onChange: null,
      });
      if (existingConnection) {
        trace.api = 'Client.query() (existing session)';
        trace.connectionReused = true;
      }
      report({ ...trace });
      try {
        result = await client.query(sql, params);
      } finally {
        if (!session) await client.end().catch(() => {});
      }
    } else {
      const query = neon(url, settings.method === 'http-bearer' ? { authToken: settings.token.trim() } : {});
      result = await query.query(sql, params, { fullResults: true });
    }
    trace.state = 'success';
    trace.durationMs = Math.round(performance.now() - started);
    trace.response = resultPreview(result);
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

export async function executeBatch(settings, statements, report = () => {}) {
  if (settings.method === 'ws-password') throw new Error('Select an HTTP transport for batch queries.');
  if (statements.length < 1 || statements.length > 100) throw new Error('An HTTP batch needs 1 to 100 statements.');
  const url = connectionString(settings);
  const address = new URL(url);
  const trace = {
    id: ++nextTraceId,
    purpose: 'batch',
    state: 'running',
    startedAt: new Date().toLocaleTimeString(),
    durationMs: null,
    transport: 'HTTP',
    api: 'neon().transaction()',
    endpoint: `${location.origin}/sql`,
    request: null,
    response: null,
  };
  report({ ...trace });
  const capture = { bodyQueries: statements, url: address, trace, report, captured: false };
  pendingHTTP.push(capture);
  const started = performance.now();
  try {
    const query = neon(url, settings.method === 'http-bearer' ? { authToken: settings.token.trim() } : {});
    const results = await query.transaction(
      statements.map((sql) => query.query(sql, [], { fullResults: true })),
      { fullResults: true },
    );
    trace.state = 'success';
    trace.durationMs = Math.round(performance.now() - started);
    trace.response = { results: results.map(resultPreview) };
    report({ ...trace });
    return results;
  } catch (error) {
    trace.state = 'error';
    trace.durationMs = Math.round(performance.now() - started);
    trace.response = { error: safeError(error, settings), code: error?.code ?? null };
    report({ ...trace });
    throw error;
  } finally {
    const index = pendingHTTP.indexOf(capture);
    if (index >= 0) pendingHTTP.splice(index, 1);
  }
}
