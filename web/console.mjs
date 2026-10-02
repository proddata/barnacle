import { runWebSocketQuery } from './pgwire.mjs';

const form = document.querySelector('#query-form');
const output = document.querySelector('#result');
const buttons = [...form.querySelectorAll('button')];

async function runHTTP(connection, token, sql) {
  const headers = { 'Content-Type': 'application/json' };
  if (connection) headers['Neon-Connection-String'] = connection;
  if (token) headers.Authorization = `Bearer ${token}`;
  const response = await fetch('/sql', { method: 'POST', headers, body: JSON.stringify({ query: sql, params: [] }) });
  return response.json();
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  const mode = event.submitter?.value || 'http';
  const connection = document.querySelector('#connection').value.trim();
  const token = document.querySelector('#token').value.trim();
  const sql = document.querySelector('#sql').value;
  output.textContent = `Running via ${mode.toUpperCase()}…`;
  buttons.forEach(button => button.disabled = true);
  try {
    if (mode === 'ws' && token) throw new Error('WebSocket uses the username and password in the connection string. Clear the bearer token field.');
    const result = mode === 'ws' ? await runWebSocketQuery(connection, sql) : await runHTTP(connection, token, sql);
    output.textContent = JSON.stringify(result, null, 2);
  } catch (error) {
    output.textContent = String(error);
  } finally {
    buttons.forEach(button => button.disabled = false);
  }
});
