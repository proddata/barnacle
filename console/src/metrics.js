import './style.css';
import { durationRanges, parseMetrics } from './metrics-data.js';

const $ = (id) => document.getElementById(id);
const number = new Intl.NumberFormat();
const formatCount = (value) => Number.isFinite(value) ? number.format(value) : '—';
const bucketLabels = ['≤ 10 ms', '10–50 ms', '50–100 ms', '100–500 ms', '0.5–1 s', '1–5 s', '> 5 s'];
let loading = false;

function render(data) {
  for (const [id, value] of Object.entries({
    'websocket-active': data.websocketActive,
    'websocket-connections': data.websocketConnections,
    'http-sql-active': data.httpSQLActive,
    'sql-requests': data.sqlRequests,
    'sql-errors': data.sqlErrors,
    'stream-interruptions': data.streamInterruptions.every(Number.isFinite)
      ? data.streamInterruptions.reduce((sum, value) => sum + value, 0) : undefined,
    'http-rejections': data.httpRejections,
    'upstream-rejections': data.upstreamRejections,
    'upstream-failures': data.upstreamFailures,
  })) $(id).textContent = formatCount(value);
  $('sql-average').textContent = Number.isFinite(data.durationSum) && data.durationCount > 0
    ? `${(data.durationSum / data.durationCount * 1000).toFixed(1)} ms` : '—';
  $('duration-total').textContent = `${formatCount(data.durationCount)} completed requests`;

  const ranges = durationRanges(data.buckets);
  const max = Math.max(1, ...ranges.map((range) => range.amount ?? 0));
  const container = $('duration-buckets');
  container.replaceChildren();
  ranges.forEach(({ amount }, index) => {
    const row = document.createElement('div'); row.className = 'duration-row';
    const label = document.createElement('span'); label.textContent = bucketLabels[index];
    const track = document.createElement('div'); track.className = 'duration-track';
    const bar = document.createElement('div'); bar.className = 'duration-bar';
    bar.style.width = `${((amount ?? 0) / max) * 100}%`;
    track.append(bar);
    const count = document.createElement('strong'); count.textContent = formatCount(amount);
    row.append(label, track, count); container.append(row);
  });
}

async function refresh() {
  if (loading || document.hidden || ($('metrics-view') && $('metrics-view').hidden)) return;
  loading = true;
  $('refresh-metrics').disabled = true;
  try {
    const response = await fetch('/metrics', { cache: 'no-store' });
    if (!response.ok) throw new Error([404, 502].includes(response.status)
      ? 'Metrics are unavailable. Enable BARNACLE_METRICS or check the metrics listener.'
      : `Metrics request failed (HTTP ${response.status}).`);
    const body = await response.text();
    const data = parseMetrics(body);
    if (!Number.isFinite(data.websocketActive) || !Number.isFinite(data.sqlRequests)) {
      throw new Error('Barnacle returned metrics in an unexpected format.');
    }
    render(data);
    $('metrics-raw').textContent = body;
    $('metrics-updated').textContent = `Updated ${new Date().toLocaleTimeString()}`;
    $('metrics-error').hidden = true;
  } catch (error) {
    $('metrics-error').textContent = error.message || 'Could not load metrics.';
    $('metrics-error').hidden = false;
    $('metrics-updated').textContent = 'Refresh failed';
  } finally {
    loading = false;
    $('refresh-metrics').disabled = false;
  }
}

export function startMetrics() {
  $('refresh-metrics').addEventListener('click', refresh);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
  setInterval(refresh, 5000);
  refresh();
}

if (document.body.classList.contains('metrics-page')) startMetrics();
