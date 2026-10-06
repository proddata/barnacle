export function parseMetrics(body) {
  const values = new Map();
  for (const line of body.split(/\r?\n/)) {
    if (!line || line.startsWith('#')) continue;
    const match = /^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?\s+([^\s]+)(?:\s|$)/.exec(line);
    if (match) values.set(`${match[1]}${match[2] ? `{${match[2]}}` : ''}`, Number(match[3]));
  }
  const get = (name) => values.get(name);
  return {
    websocketActive: get('barnacle_websocket_active'),
    websocketConnections: get('barnacle_websocket_connections_total'),
    httpSQLActive: get('barnacle_http_sql_active'),
    sqlRequests: get('barnacle_sql_requests_total'),
    sqlErrors: get('barnacle_sql_errors_total'),
    streamInterruptions: ['result_limit', 'canceled', 'timeout', 'query_or_transport']
      .map((kind) => get(`barnacle_sql_stream_interruptions_total{kind="${kind}"}`)),
    httpRejections: get('barnacle_limit_rejections_total{limit="http_queries"}'),
    upstreamRejections: get('barnacle_limit_rejections_total{limit="upstream_connections"}'),
    upstreamFailures: get('barnacle_upstream_connection_failures_total'),
    durationSum: get('barnacle_sql_duration_seconds_sum'),
    durationCount: get('barnacle_sql_duration_seconds_count'),
    buckets: ['0.01', '0.05', '0.1', '0.5', '1', '5', '+Inf'].map((bound) => ({
      bound, count: get(`barnacle_sql_duration_seconds_bucket{le="${bound}"}`),
    })),
  };
}

export function durationRanges(buckets) {
  let previous = 0;
  return buckets.map(({ bound, count }) => {
    const amount = Number.isFinite(count) ? Math.max(0, count - previous) : undefined;
    if (Number.isFinite(count)) previous = count;
    return { bound, amount };
  });
}
