import * as monaco from 'monaco-editor/editor/editor.api';
import 'monaco-editor/languages/definitions/pgsql/register';
import EditorWorker from 'monaco-editor/editor/editor.worker?worker';
import './style.css';
import { COLUMNS_SQL, TABLES_SQL, closeQuerySession, executeBatch, executeQuery, quoteIdentifier, safeError, safeJSON } from './db.js';
import { splitStatements } from './statements.js';

self.MonacoEnvironment = { getWorker() { return new EditorWorker(); } };

const $ = (id) => document.getElementById(id);
const workbench = $('workbench');
const editorNode = $('sql-editor');
const tabBar = $('query-tabs');
const treeNode = $('schema-tree');
const startupDetails = $('startup-details');
const startupState = $('startup-state');
const traceList = $('trace-list');
const traceDetail = $('trace-detail');
const resultData = $('result-data');
const resultJSON = $('result-json');
const resultSummary = $('result-summary');
const runButton = $('run-query');
const batchToggle = $('http-batch');
const statusMessage = $('status-message');
const statusMeta = $('status-meta');
const connectionBadge = $('connection-badge');
const fields = Object.fromEntries(['connection-name', 'host', 'database', 'username', 'password', 'token', 'method', 'auth-method'].map((id) => [id, $(id)]));

const PROFILE_KEY = 'hermit-console-connections-v1';
const defaultProfile = { id: 'local-dev', name: 'Local development', host: 'postgres:5432', database: 'hermit', username: 'hermit', method: 'http-password', authMethod: '' };
const examples = {
  'transaction-delay': {
    name: 'Transaction with delay',
    websocket: true,
    sql: `-- Use WebSocket · password, then Run all. Results appear after each statement.
BEGIN;
SELECT 1 AS first_result;
SELECT pg_sleep(5), 2 AS delayed_result;
SELECT 3 AS last_result;
COMMIT;`,
  },
  'failed-transaction': {
    name: 'Failed transaction',
    websocket: true,
    sql: `-- Use WebSocket · password, then Run all. It stops at the expected error.
BEGIN;
SELECT 1 AS before_error;
SELECT 1 / 0 AS intentional_error;
-- Select and run this to see PostgreSQL reject work in the aborted transaction:
SELECT 2 AS blocked_while_aborted;
-- Select and run this statement to recover:
ROLLBACK;
-- Then select and run this statement to verify recovery:
SELECT 3 AS after_rollback;`,
  },
  'server-info': {
    name: 'Server and session info',
    sql: `SELECT current_setting('server_version') AS server_version,
       current_database() AS database_name,
       current_user AS user_name,
       current_setting('search_path') AS search_path,
       current_schemas(true) AS effective_schemas;`,
  },
};
function loadProfiles() {
  try {
    const data = JSON.parse(localStorage.getItem(PROFILE_KEY));
    if (Array.isArray(data) && data.length) return data.filter((item) => item && typeof item.id === 'string' && typeof item.name === 'string');
  } catch { /* Invalid local data falls back to the development profile. */ }
  return [{ ...defaultProfile }];
}
let profiles = loadProfiles();
if (!profiles.length) profiles = [{ ...defaultProfile }];
let activeConnectionId = profiles[0].id;
const secrets = new Map();
const localDevelopment = profiles.find((profile) => profile.id === 'local-dev');
if (localDevelopment?.host === defaultProfile.host && localDevelopment.database === defaultProfile.database && localDevelopment.username === defaultProfile.username) {
  secrets.set('local-dev', { password: 'hermit_dev_password', token: '' });
}

let tabs = [];
let activeTabId = null;
let nextTabId = 0;
const lastTabByConnection = new Map();
let traces = [];
let selectedTraceId = null;
let schemaRows = [];
const columnsCache = new Map();
const openSchemas = new Set(['public']);
const openTables = new Set();
let busy = false;

monaco.editor.defineTheme('hermit', {
  base: 'vs-dark', inherit: true,
  rules: [
    { token: 'keyword', foreground: '77cfc2', fontStyle: 'bold' },
    { token: 'string', foreground: 'e6bd83' },
    { token: 'number', foreground: 'b8a5eb' },
    { token: 'comment', foreground: '6b8a94', fontStyle: 'italic' },
  ],
  colors: {
    'editor.background': '#101a22', 'editor.foreground': '#d8e8eb',
    'editorLineNumber.foreground': '#536e78', 'editorLineNumber.activeForeground': '#a3c1c7',
    'editor.selectionBackground': '#33596188', 'editor.lineHighlightBackground': '#1a2c36',
    'editorCursor.foreground': '#71d5c6', 'editorIndentGuide.background1': '#2a3d46',
  },
});
const editor = monaco.editor.create(editorNode, {
  theme: 'hermit', language: 'pgsql', automaticLayout: true,
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
  fontSize: 13, lineHeight: 21, minimap: { enabled: false },
  scrollBeyondLastLine: false, smoothScrolling: true,
  roundedSelection: false, padding: { top: 17, bottom: 15 },
  wordWrap: 'on', tabSize: 2,
});
editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.Enter, () => runCurrent());
editor.onDidChangeCursorSelection(updateRunLabel);

function updateRunLabel() {
  const selected = editor.getModel()?.getValueInRange(editor.getSelection()).trim();
  $('run-label').textContent = selected ? 'Run selection' : 'Run all';
}

function settings() {
  return {
    host: fields.host.value, database: fields.database.value,
    username: fields.username.value, password: fields.password.value,
    token: fields.token.value, method: fields.method.value, authMethod: fields['auth-method'].value,
  };
}

function activeProfile() { return profiles.find((profile) => profile.id === activeConnectionId); }

function saveProfileFromFields() {
  const profile = activeProfile();
  if (!profile) return;
  Object.assign(profile, {
    name: fields['connection-name'].value.trim() || 'Unnamed connection',
    host: fields.host.value, database: fields.database.value, username: fields.username.value,
    method: fields.method.value, authMethod: fields['auth-method'].value,
    httpBatch: batchToggle.checked,
  });
  secrets.set(profile.id, { password: fields.password.value, token: fields.token.value });
  localStorage.setItem(PROFILE_KEY, JSON.stringify(profiles));
}

function renderConnectionSelect() {
  const select = $('connection-select'); select.replaceChildren();
  for (const profile of profiles) {
    const option = document.createElement('option'); option.value = profile.id; option.textContent = profile.name;
    select.append(option);
  }
  select.value = activeConnectionId;
  $('delete-connection').disabled = profiles.length === 1;
}

function showConnection(id) {
  const profile = profiles.find((item) => item.id === id);
  if (!profile) return;
  activeConnectionId = id;
  const secret = secrets.get(id) ?? { password: '', token: '' };
  fields['connection-name'].value = profile.name;
  for (const name of ['host', 'database', 'username', 'method']) fields[name].value = profile[name] ?? '';
  fields['auth-method'].value = profile.authMethod ?? '';
  batchToggle.checked = profile.httpBatch === true;
  fields.password.value = secret.password;
  fields.token.value = secret.token;
  updateMethodFields(); renderConnectionSelect();
  schemaRows = []; columnsCache.clear(); openTables.clear(); renderTree();
  const tab = tabs.find((item) => item.id === lastTabByConnection.get(id)) ?? tabs.find((item) => item.connectionId === id);
  if (tab) activateTab(tab.id);
  else createTab('Query 1', 'select now() as server_time, current_user as db_user;');
  setConnectionState('', `${profile.name} selected`);
  setStatus(`Switched to ${profile.name}`, `${profile.host} / ${profile.database}`);
}

function setStatus(message, meta = '') {
  statusMessage.textContent = message;
  statusMeta.textContent = meta || 'PostgreSQL via Hermit';
}

function setConnectionState(state, text) {
  connectionBadge.className = `connection-badge ${state}`;
  connectionBadge.lastChild.textContent = text;
}

function updateMethodFields() {
  const method = fields.method.value;
  $('password-field').hidden = method === 'http-bearer';
  $('token-field').hidden = method !== 'http-bearer';
  $('auth-method-field').hidden = method !== 'http-password';
  $('http-batch-field').hidden = method === 'ws-password';
}

function setRunBusy(value) {
  busy = value;
  runButton.disabled = value;
}

function createTab(name = `Query ${nextTabId + 1}`, sql = '') {
  const id = ++nextTabId;
  const model = monaco.editor.createModel(sql, 'pgsql', monaco.Uri.parse(`inmemory://hermit/query-${id}.pgsql`));
  tabs.push({ id, name, model, connectionId: activeConnectionId, session: { onChange: renderConnectionInfo }, results: [], selectedResultIndex: 0, resultView: 'data' });
  activateTab(id);
  return id;
}

function activateTab(id) {
  const tab = tabs.find((item) => item.id === id);
  if (!tab) return;
  activeTabId = id;
  lastTabByConnection.set(tab.connectionId, id);
  editor.setModel(tab.model);
  updateRunLabel();
  renderTabs();
  renderResult();
  renderConnectionInfo();
  showResultView(tab.resultView);
  editor.focus();
}

function closeTab(id) {
  if (tabs.filter((tab) => tab.connectionId === activeConnectionId).length === 1) return;
  const index = tabs.findIndex((tab) => tab.id === id);
  if (index < 0) return;
  const [tab] = tabs.splice(index, 1);
  void closeQuerySession(tab.session);
  if (activeTabId === id) activateTab((tabs.find((item) => item.connectionId === activeConnectionId) ?? tabs[Math.min(index, tabs.length - 1)]).id);
  tab.model.dispose();
  renderTabs();
}

function renderTabs() {
  tabBar.replaceChildren();
  for (const tab of tabs.filter((item) => item.connectionId === activeConnectionId)) {
    const button = document.createElement('button');
    button.className = `query-tab ${tab.id === activeTabId ? 'active' : ''}`;
    button.type = 'button'; button.role = 'tab'; button.ariaSelected = String(tab.id === activeTabId);
    const label = document.createElement('span'); label.className = 'tab-label'; label.textContent = tab.name;
    const close = document.createElement('span'); close.className = 'close-tab'; close.textContent = '×'; close.title = 'Close tab';
    close.addEventListener('click', (event) => { event.stopPropagation(); closeTab(tab.id); });
    button.append(label, close);
    button.addEventListener('click', () => activateTab(tab.id));
    tabBar.append(button);
  }
}

function activeTab() { return tabs.find((tab) => tab.id === activeTabId); }

function renderConnectionInfo() {
  startupDetails.replaceChildren();
  const session = activeTab()?.session;
  if (fields.method.value !== 'ws-password' || !session?.client || !session.connection) {
    startupState.textContent = 'No session';
    const empty = document.createElement('p'); empty.className = 'startup-empty';
    empty.textContent = fields.method.value === 'ws-password'
      ? 'Run a query to open this tab’s WebSocket session.'
      : 'Select WebSocket transport to see startup parameters.';
    startupDetails.append(empty);
    return;
  }
  startupState.textContent = 'Connected';
  const table = document.createElement('table'); table.className = 'startup-table';
  const head = document.createElement('thead');
  const header = document.createElement('tr');
  for (const label of ['Parameter', 'Value']) {
    const cell = document.createElement('th'); cell.textContent = label; header.append(cell);
  }
  head.append(header); table.append(head);
  const body = document.createElement('tbody');
  const rows = [
    ...(session.connection.backendProcessID === null ? [] : [['Backend PID', String(session.connection.backendProcessID)]]),
    ...session.connection.parameters.map(({ name, value }) => [name, value])
      .sort(([a], [b]) => a.localeCompare(b)),
  ];
  for (const [name, value] of rows) {
    const row = document.createElement('tr');
    const key = document.createElement('th'); key.scope = 'row'; key.textContent = name;
    const cell = document.createElement('td'); cell.textContent = value;
    row.append(key, cell); body.append(row);
  }
  table.append(body); startupDetails.append(table);
}

function selectedStatements() {
  const model = editor.getModel();
  const selection = editor.getSelection();
  const selected = model.getValueInRange(selection);
  return splitStatements(selected.trim() ? selected : model.getValue());
}

function reportTrace(trace) {
  const index = traces.findIndex((item) => item.id === trace.id);
  if (index < 0) traces.unshift(trace); else traces[index] = trace;
  traces = traces.slice(0, 40);
  if (selectedTraceId === null || index < 0) selectedTraceId = trace.id;
  renderTraceList();
  renderTraceDetail();
}

function renderTraceList() {
  traceList.replaceChildren();
  if (!traces.length) {
    const note = document.createElement('p'); note.className = 'empty-note';
    note.textContent = 'Run a query or refresh the explorer to inspect a call.';
    traceList.append(note); return;
  }
  for (const trace of traces) {
    const item = document.createElement('button');
    item.type = 'button'; item.className = `trace-item ${trace.id === selectedTraceId ? 'active' : ''}`;
    const copy = document.createElement('span');
    const title = document.createElement('strong'); title.textContent = `${trace.transport} · ${trace.purpose === 'query' ? 'Query' : trace.purpose}`;
    const detail = document.createElement('small'); detail.textContent = `${trace.startedAt} · ${trace.durationMs ?? '…'} ms${trace.connectionReused ? ' · reused connection' : ''}`;
    const state = document.createElement('span'); state.className = `trace-status ${trace.state}`;
    state.textContent = trace.state === 'success' ? '✓' : trace.state === 'error' ? '!' : '…';
    copy.append(title, detail); item.append(copy, state);
    item.addEventListener('click', () => { selectedTraceId = trace.id; renderTraceList(); renderTraceDetail(); });
    traceList.append(item);
  }
}

function appendDetailSection(parent, title, value, raw = false) {
  const section = document.createElement('section'); section.className = 'detail-section';
  const heading = document.createElement('h3'); heading.textContent = title;
  const code = document.createElement('pre');
  const content = raw ? String(value) : safeJSON(value);
  code.textContent = content.length > 50000 ? `${content.slice(0, 50000)}\n… debug preview truncated` : content;
  section.append(heading, code); parent.append(section);
}

function appendHeaders(parent, headers) {
  const section = document.createElement('section'); section.className = 'detail-section';
  const heading = document.createElement('h3'); heading.textContent = 'Request headers';
  const table = document.createElement('table'); table.className = 'headers-table';
  for (const [name, value] of Object.entries(headers)) {
    const row = document.createElement('tr');
    const key = document.createElement('th'); key.textContent = name;
    const cell = document.createElement('td'); cell.textContent = value;
    row.append(key, cell); table.append(row);
  }
  section.append(heading, table); parent.append(section);
}

function renderTraceDetail() {
  const trace = traces.find((item) => item.id === selectedTraceId);
  traceDetail.replaceChildren(); traceDetail.hidden = !trace;
  if (!trace) return;
  const title = document.createElement('p'); title.className = 'detail-title'; title.textContent = trace.api;
  const meta = document.createElement('p'); meta.className = 'detail-meta';
  meta.textContent = `${trace.transport} · ${trace.endpoint} · ${trace.state}${trace.durationMs === null ? '' : ` · ${trace.durationMs} ms`}`;
  traceDetail.append(title, meta);
  if (trace.request?.headers) {
    appendHeaders(traceDetail, trace.request.headers);
    appendDetailSection(traceDetail, 'Request body (sent)', trace.request.body, true);
  } else if (trace.request) appendDetailSection(traceDetail, 'PostgreSQL operation', trace.request);
  else appendDetailSection(traceDetail, 'Request', 'Preparing request…');
  if (trace.response) appendDetailSection(traceDetail, 'Response', trace.response);
}

function cellText(value) {
  if (value === null || value === undefined) return 'NULL';
  if (typeof value === 'object') return safeJSON(value, 0);
  return String(value);
}

function selectedResult() {
  const tab = activeTab();
  return tab?.results[tab.selectedResultIndex] ?? null;
}

function renderStatementResults() {
  const strip = $('statement-results'); strip.replaceChildren();
  const tab = activeTab();
  strip.hidden = !tab?.results.length;
  if (!tab?.results.length) return;
  tab.results.forEach((result, index) => {
    const button = document.createElement('button');
    button.type = 'button'; button.role = 'tab';
    button.className = `statement-result ${index === tab.selectedResultIndex ? 'active' : ''} ${result.error ? 'failed' : ''}`;
    button.ariaSelected = String(index === tab.selectedResultIndex);
    button.textContent = `${index + 1}. ${result.command ?? (result.error ? 'Error' : 'Result')}`;
    button.title = result.sql;
    button.addEventListener('click', () => { tab.selectedResultIndex = index; renderResult(); });
    strip.append(button);
  });
}

function renderResult() {
  renderStatementResults();
  const lastResult = selectedResult();
  resultData.replaceChildren();
  resultJSON.textContent = '';
  if (!lastResult) {
    $('export-csv').disabled = true;
    resultSummary.textContent = 'No results yet';
    const empty = document.createElement('div'); empty.className = 'result-empty';
    empty.textContent = 'Write SQL above, then run it to see results here.';
    resultData.append(empty); return;
  }
  if (lastResult.error) {
    $('export-csv').disabled = true;
    resultSummary.textContent = 'Query failed';
    const error = document.createElement('div'); error.className = 'error-box'; error.textContent = lastResult.error;
    resultData.append(error);
    resultJSON.textContent = safeJSON({ error: lastResult.error });
    return;
  }
  const result = lastResult;
  const rows = result.rows ?? [];
  $('export-csv').disabled = rows.length === 0;
  const columns = result.fields?.map((field) => field.name) ?? Object.keys(rows[0] ?? {});
  resultSummary.textContent = `${result.command ?? 'QUERY'} · ${result.rowCount ?? rows.length} row${(result.rowCount ?? rows.length) === 1 ? '' : 's'} · ${lastResult.durationMs} ms${rows.length > 500 ? ' · showing first 500' : ''}`;
  if (!columns.length) {
    const empty = document.createElement('div'); empty.className = 'result-empty'; empty.textContent = 'Query completed. No result columns.';
    resultData.append(empty);
  } else {
    const table = document.createElement('table'); table.className = 'data-grid';
    const thead = document.createElement('thead'); const header = document.createElement('tr');
    const indexHead = document.createElement('th'); indexHead.className = 'row-number'; indexHead.textContent = '#'; header.append(indexHead);
    for (const name of columns) { const th = document.createElement('th'); th.textContent = name; header.append(th); }
    thead.append(header); table.append(thead);
    const tbody = document.createElement('tbody');
    rows.slice(0, 500).forEach((row, index) => {
      const tr = document.createElement('tr'); const number = document.createElement('td'); number.className = 'row-number'; number.textContent = String(index + 1); tr.append(number);
      for (const name of columns) {
        const td = document.createElement('td'); const value = row?.[name];
        td.textContent = cellText(value); td.title = td.textContent;
        if (value === null || value === undefined) td.className = 'null-value';
        tr.append(td);
      }
      tbody.append(tr);
    });
    table.append(tbody); resultData.append(table);
  }
  const json = safeJSON({ rows: rows.slice(0, 500), rowCount: result.rowCount, command: result.command, fields: result.fields });
  resultJSON.textContent = json.length > 250000 ? `${json.slice(0, 250000)}\n… JSON preview truncated` : json;
}

function exportCSV() {
  const lastResult = selectedResult();
  if (!lastResult || lastResult.error || !lastResult.rows?.length) return;
  const columns = lastResult.fields?.map((field) => field.name) ?? Object.keys(lastResult.rows[0]);
  const csvCell = (value) => {
    let text = value === null || value === undefined ? '' : cellText(value);
    if (typeof value === 'string' && /^[\t\r\n ]*[=+\-@]/.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
  };
  const lines = [columns.map(csvCell).join(',')];
  for (const row of lastResult.rows) lines.push(columns.map((name) => csvCell(row[name])).join(','));
  const blob = new Blob([lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a'); link.href = url; link.download = 'hermit-results.csv';
  document.body.append(link); link.click(); link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function showResultView(view) {
  const tab = activeTab();
  if (tab) tab.resultView = view;
  $('view-data').classList.toggle('active', view === 'data');
  $('view-json').classList.toggle('active', view === 'json');
  resultData.hidden = view !== 'data'; resultJSON.hidden = view !== 'json';
}

async function runCurrent() {
  if (busy) return;
  const statements = selectedStatements();
  if (!statements.length) { setStatus('Write a query first.'); return; }
  const tab = activeTab();
  const current = settings();
  if (current.method !== 'ws-password' && batchToggle.checked) {
    await runHTTPBatch(statements, tab, current);
    return;
  }
  setRunBusy(true);
  tab.results = []; tab.selectedResultIndex = 0;
  setStatus(`Running ${statements.length} statement${statements.length === 1 ? '' : 's'}…`);
  for (const [index, statement] of statements.entries()) {
    const started = performance.now();
    try {
      const result = await executeQuery(current, statement.sql, [], reportTrace, 'query', tab.session);
      tab.results.push({ ...result, sql: statement.sql, durationMs: Math.round(performance.now() - started) });
      setConnectionState('connected', `${current.host} / ${current.database}`);
    } catch (error) {
      const message = safeError(error, current);
      tab.results.push({ sql: statement.sql, error: message, durationMs: Math.round(performance.now() - started) });
      tab.selectedResultIndex = index;
      setStatus(`Statement ${index + 1} failed`, message);
      setConnectionState('failed', `${current.host} / ${current.database}`);
      break;
    }
    tab.selectedResultIndex = index;
    setStatus(`Completed ${index + 1} of ${statements.length}`, `${current.host} / ${current.database}`);
    if (activeTabId === tab.id) renderResult();
  }
  setRunBusy(false);
  if (activeTabId === tab.id) { renderResult(); showResultView('data'); }
}

async function runHTTPBatch(statements, tab, current) {
  if (statements.length > 100) { setStatus('HTTP batch limit is 100 statements.'); return; }
  setRunBusy(true);
  tab.results = []; tab.selectedResultIndex = 0;
  setStatus(`Running ${statements.length} statements in one HTTP transaction…`);
  const started = performance.now();
  try {
    const results = await executeBatch(current, statements.map((statement) => statement.sql), reportTrace);
    const durationMs = Math.round(performance.now() - started);
    tab.results = results.map((result, index) => ({ ...result, sql: statements[index].sql, durationMs }));
    tab.selectedResultIndex = results.length - 1;
    setStatus(`HTTP transaction completed · ${results.length} results`, `${current.host} / ${current.database}`);
    setConnectionState('connected', `${current.host} / ${current.database}`);
  } catch (error) {
    const message = safeError(error, current);
    tab.results = [{ sql: 'HTTP transaction', error: message, durationMs: Math.round(performance.now() - started) }];
    setStatus('HTTP transaction failed', message);
    setConnectionState('failed', `${current.host} / ${current.database}`);
  } finally {
    setRunBusy(false);
    if (activeTabId === tab.id) { renderResult(); showResultView('data'); }
  }
}

function objectKind(kind) {
  return ({ r: 'TABLE', p: 'PARTITION', v: 'VIEW', m: 'MAT VIEW', f: 'FOREIGN' })[kind] ?? kind;
}

function makeColumnList(key, holder) {
  holder.replaceChildren();
  const columns = columnsCache.get(key);
  if (!columns) { const note = document.createElement('div'); note.className = 'tree-column'; note.textContent = 'Loading columns…'; holder.append(note); return; }
  if (!columns.length) { const note = document.createElement('div'); note.className = 'tree-column'; note.textContent = 'No columns found'; holder.append(note); return; }
  for (const column of columns) {
    const line = document.createElement('div'); line.className = 'tree-column';
    const name = document.createElement('span'); name.textContent = column.column_name;
    const type = document.createElement('span'); type.textContent = `${column.data_type}${column.not_null ? ' · NOT NULL' : ''}`;
    line.append(name, type); holder.append(line);
  }
}

async function loadColumns(schema, table, holder) {
  const key = `${schema}\0${table}`;
  if (columnsCache.has(key)) { makeColumnList(key, holder); return; }
  makeColumnList(key, holder);
  try {
    const connectionId = activeConnectionId;
    const result = await executeQuery(settings(), COLUMNS_SQL, [schema, table], reportTrace, 'Columns');
    if (connectionId !== activeConnectionId) return;
    columnsCache.set(key, result.rows ?? []);
    if (holder.isConnected) makeColumnList(key, holder);
  } catch (error) {
    if (holder.isConnected) holder.textContent = safeError(error, settings());
  }
}

function renderTree() {
  treeNode.replaceChildren();
  const filter = $('tree-filter').value.trim().toLowerCase();
  const visible = schemaRows.filter((row) => !filter || `${row.schema_name}.${row.table_name}`.toLowerCase().includes(filter));
  if (!visible.length) {
    const note = document.createElement('p'); note.className = 'empty-note';
    note.textContent = schemaRows.length ? 'No tables match this filter.' : 'Enter a password, then refresh to browse tables.';
    treeNode.append(note); return;
  }
  const groups = new Map();
  for (const row of visible) {
    if (!groups.has(row.schema_name)) groups.set(row.schema_name, []);
    groups.get(row.schema_name).push(row);
  }
  for (const [schema, tables] of groups) {
    const group = document.createElement('details'); group.className = 'tree-schema'; group.open = filter !== '' || openSchemas.has(schema);
    const summary = document.createElement('summary'); summary.textContent = `${schema}  ·  ${tables.length}`;
    group.addEventListener('toggle', () => { if (group.open) openSchemas.add(schema); else openSchemas.delete(schema); });
    group.append(summary);
    const children = document.createElement('div'); children.className = 'tree-children';
    for (const row of tables) {
      const key = `${schema}\0${row.table_name}`;
      const entry = document.createElement('details'); entry.className = 'tree-table'; entry.open = openTables.has(key);
      const tableSummary = document.createElement('summary');
      const label = document.createElement('span'); label.textContent = row.table_name; label.style.flex = '1'; label.style.overflow = 'hidden'; label.style.textOverflow = 'ellipsis';
      const kind = document.createElement('span'); kind.className = 'object-kind'; kind.textContent = objectKind(row.relation_kind);
      const preview = document.createElement('button'); preview.type = 'button'; preview.className = 'table-preview'; preview.textContent = '↗'; preview.title = `Preview ${schema}.${row.table_name}`; preview.style.flex = 'none';
      preview.addEventListener('click', (event) => {
        event.preventDefault(); event.stopPropagation();
        const sql = `select * from ${quoteIdentifier(schema)}.${quoteIdentifier(row.table_name)} limit 100;`;
        createTab(row.table_name, sql); runCurrent();
      });
      tableSummary.append(label, kind, preview); entry.append(tableSummary);
      const columnList = document.createElement('div'); columnList.className = 'tree-columns'; entry.append(columnList);
      entry.addEventListener('toggle', () => {
        if (entry.open) { openTables.add(key); loadColumns(schema, row.table_name, columnList); }
        else openTables.delete(key);
      });
      if (entry.open) loadColumns(schema, row.table_name, columnList);
      children.append(entry);
    }
    group.append(children); treeNode.append(group);
  }
}

async function refreshSchema() {
  const button = $('refresh-schema'); button.disabled = true;
  setStatus('Loading schema…');
  try {
    const connectionId = activeConnectionId;
    const current = settings();
    const result = await executeQuery(current, TABLES_SQL, [], reportTrace, 'Tables');
    if (connectionId !== activeConnectionId) return;
    schemaRows = result.rows ?? []; columnsCache.clear();
    renderTree();
    setStatus(`Loaded ${schemaRows.length} tables and views`, `${current.host} / ${current.database}`);
    setConnectionState('connected', `${current.host} / ${current.database}`);
  } catch (error) {
    setStatus('Schema refresh failed', safeError(error, settings()));
    setConnectionState('failed', `${fields.host.value} / ${fields.database.value}`);
  } finally { button.disabled = false; }
}

monaco.languages.registerCompletionItemProvider('pgsql', {
  provideCompletionItems(model, position) {
    const word = model.getWordUntilPosition(position);
    const range = { startLineNumber: position.lineNumber, endLineNumber: position.lineNumber, startColumn: word.startColumn, endColumn: word.endColumn };
    return { suggestions: schemaRows.map((row) => ({
      label: `${row.schema_name}.${row.table_name}`,
      kind: monaco.languages.CompletionItemKind.Class,
      insertText: `${quoteIdentifier(row.schema_name)}.${quoteIdentifier(row.table_name)}`,
      detail: objectKind(row.relation_kind), range,
    })) };
  },
});

function setupResize(handleId, side) {
  const handle = $(handleId);
  const key = `hermit-console-${side}-size`;
  const saved = Number(localStorage.getItem(key));
  if (saved > 0) workbench.style.setProperty(`--${side === 'results' ? 'results-height' : `${side === 'left' ? 'explorer' : 'debug'}-width`}`, `${saved}px`);
  function apply(value) {
    const size = Math.round(Math.min(side === 'results' ? 600 : 520, Math.max(side === 'results' ? 130 : 170, value)));
    workbench.style.setProperty(`--${side === 'results' ? 'results-height' : `${side === 'left' ? 'explorer' : 'debug'}-width`}`, `${size}px`);
    localStorage.setItem(key, String(size)); editor.layout();
  }
  handle.addEventListener('pointerdown', (event) => {
    event.preventDefault(); handle.setPointerCapture(event.pointerId); handle.classList.add('dragging');
    const rect = workbench.getBoundingClientRect();
    const move = (e) => apply(side === 'left' ? e.clientX - rect.left : side === 'right' ? rect.right - e.clientX : rect.bottom - e.clientY);
    const end = () => { handle.classList.remove('dragging'); handle.removeEventListener('pointermove', move); handle.removeEventListener('pointerup', end); handle.removeEventListener('pointercancel', end); };
    handle.addEventListener('pointermove', move); handle.addEventListener('pointerup', end); handle.addEventListener('pointercancel', end);
  });
  handle.addEventListener('keydown', (event) => {
    const arrows = side === 'results' ? ['ArrowUp', 'ArrowDown'] : ['ArrowLeft', 'ArrowRight'];
    if (!arrows.includes(event.key)) return;
    event.preventDefault();
    const property = side === 'results' ? '--results-height' : side === 'left' ? '--explorer-width' : '--debug-width';
    const current = Number.parseInt(getComputedStyle(workbench).getPropertyValue(property), 10) || (side === 'results' ? 245 : 240);
    const delta = event.key === 'ArrowLeft' || event.key === 'ArrowUp' ? -20 : 20;
    apply(current + (side === 'right' ? -delta : delta));
  });
}

function togglePanel(side) {
  const mobile = matchMedia('(max-width: 760px)').matches;
  if (mobile) {
    const target = side === 'left' ? 'mobile-explorer' : 'mobile-debug';
    const other = side === 'left' ? 'mobile-debug' : 'mobile-explorer';
    workbench.classList.remove(other); workbench.classList.toggle(target);
  } else workbench.classList.toggle(side === 'left' ? 'explorer-collapsed' : 'debug-collapsed');
  requestAnimationFrame(() => editor.layout());
}

$('run-query').addEventListener('click', () => runCurrent());
$('refresh-schema').addEventListener('click', refreshSchema);
$('tree-filter').addEventListener('input', renderTree);
$('new-tab').addEventListener('click', () => createTab());
$('example-query').addEventListener('change', (event) => {
  const example = examples[event.target.value];
  event.target.value = '';
  if (!example) return;
  createTab(example.name, example.sql);
  setStatus(example.websocket && fields.method.value !== 'ws-password'
    ? 'Select WebSocket · password before running this example.'
    : `Opened ${example.name}`);
});
batchToggle.addEventListener('change', () => {
  saveProfileFromFields();
  setStatus(batchToggle.checked ? 'HTTP queries will run as one transaction.' : 'HTTP queries will run separately.');
});
$('clear-debug').addEventListener('click', () => { traces = []; selectedTraceId = null; renderTraceList(); renderTraceDetail(); });
$('view-data').addEventListener('click', () => showResultView('data'));
$('view-json').addEventListener('click', () => showResultView('json'));
$('export-csv').addEventListener('click', exportCSV);
$('toggle-explorer').addEventListener('click', () => togglePanel('left'));
$('toggle-debug').addEventListener('click', () => togglePanel('right'));
$('toggle-connection').addEventListener('click', () => {
  const panel = $('connection-panel'); panel.hidden = !panel.hidden;
  $('toggle-connection').ariaExpanded = String(!panel.hidden);
  requestAnimationFrame(() => editor.layout());
});
function connectionChanged(event) {
  if (event.target.id !== 'connection-name') {
    for (const tab of tabs.filter((item) => item.connectionId === activeConnectionId)) void closeQuerySession(tab.session);
  }
  if (activeConnectionId === 'local-dev' &&
      (fields.host.value !== 'postgres:5432' || fields.database.value !== 'hermit' || fields.username.value !== 'hermit') &&
      fields.password.value === 'hermit_dev_password') fields.password.value = '';
  saveProfileFromFields(); renderConnectionSelect();
  setConnectionState('', 'Connection changed');
  renderConnectionInfo();
  schemaRows = []; columnsCache.clear(); renderTree();
}
$('connection-select').addEventListener('change', (event) => {
  saveProfileFromFields(); showConnection(event.target.value);
});
$('save-connection').addEventListener('click', () => {
  saveProfileFromFields(); renderConnectionSelect(); setStatus(`Saved ${activeProfile().name}`);
});
$('new-connection').addEventListener('click', () => {
  saveProfileFromFields();
  const id = crypto.randomUUID();
  profiles.push({ id, name: `Connection ${profiles.length + 1}`, host: '', database: '', username: '', method: 'http-password', authMethod: '' });
  localStorage.setItem(PROFILE_KEY, JSON.stringify(profiles));
  showConnection(id);
  fields['connection-name'].focus(); fields['connection-name'].select();
});
$('delete-connection').addEventListener('click', () => {
  if (profiles.length === 1) return;
  const deleted = activeConnectionId;
  profiles = profiles.filter((profile) => profile.id !== deleted);
  secrets.delete(deleted);
  for (const tab of tabs.filter((item) => item.connectionId === deleted)) {
    void closeQuerySession(tab.session);
    tab.model.dispose();
  }
  tabs = tabs.filter((item) => item.connectionId !== deleted);
  lastTabByConnection.delete(deleted);
  localStorage.setItem(PROFILE_KEY, JSON.stringify(profiles));
  showConnection(profiles[0].id);
});
fields.method.addEventListener('change', updateMethodFields);
for (const field of Object.values(fields)) field.addEventListener('change', connectionChanged);
setupResize('resize-left', 'left');
setupResize('resize-right', 'right');
setupResize('resize-results', 'results');
showConnection(activeConnectionId);
