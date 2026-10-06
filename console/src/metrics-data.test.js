import test from 'node:test';
import assert from 'node:assert/strict';
import { durationRanges, parseMetrics } from './metrics-data.js';

test('reads Barnacle exposition and turns cumulative buckets into disjoint ranges', () => {
  const metrics = parseMetrics(`# TYPE barnacle_websocket_active gauge
barnacle_websocket_active 2
barnacle_websocket_connections_total 7
barnacle_http_sql_active 1
barnacle_sql_requests_total 5
barnacle_sql_errors_total 1
barnacle_sql_stream_interruptions_total{kind="result_limit"} 1
barnacle_sql_stream_interruptions_total{kind="canceled"} 0
barnacle_sql_stream_interruptions_total{kind="timeout"} 0
barnacle_sql_stream_interruptions_total{kind="query_or_transport"} 2
barnacle_limit_rejections_total{limit="http_queries"} 3
barnacle_limit_rejections_total{limit="upstream_connections"} 4
barnacle_upstream_connection_failures_total 1
barnacle_sql_duration_seconds_bucket{le="0.01"} 1
barnacle_sql_duration_seconds_bucket{le="0.05"} 3
barnacle_sql_duration_seconds_bucket{le="0.1"} 3
barnacle_sql_duration_seconds_bucket{le="0.5"} 4
barnacle_sql_duration_seconds_bucket{le="1"} 4
barnacle_sql_duration_seconds_bucket{le="5"} 4
barnacle_sql_duration_seconds_bucket{le="+Inf"} 5
barnacle_sql_duration_seconds_sum 0.42
barnacle_sql_duration_seconds_count 5
`);
  assert.equal(metrics.websocketActive, 2);
  assert.equal(metrics.websocketConnections, 7);
  assert.equal(metrics.httpSQLActive, 1);
  assert.equal(metrics.sqlRequests, 5);
  assert.equal(metrics.sqlErrors, 1);
  assert.deepEqual(metrics.streamInterruptions, [1, 0, 0, 2]);
  assert.equal(metrics.httpRejections, 3);
  assert.equal(metrics.upstreamRejections, 4);
  assert.equal(metrics.upstreamFailures, 1);
  assert.equal(metrics.durationSum, 0.42);
  assert.deepEqual(durationRanges(metrics.buckets).map(({ amount }) => amount), [1, 2, 0, 1, 0, 0, 1]);
});
