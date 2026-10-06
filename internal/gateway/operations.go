package gateway

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type Metrics struct {
	websocketActive  atomic.Int64
	sqlRequests      atomic.Uint64
	sqlErrors        atomic.Uint64
	sqlDurationNS    atomic.Uint64
	sqlLatency       [6]atomic.Uint64
	httpLimitRejects atomic.Uint64
	upstreamRejects  atomic.Uint64
	upstreamFailures atomic.Uint64
}

func (m *Metrics) AddWebSocket(delta int64) { m.websocketActive.Add(delta) }
func (m *Metrics) RejectHTTPQuery() {
	if m != nil {
		m.httpLimitRejects.Add(1)
	}
}
func (m *Metrics) RejectUpstream() {
	if m != nil {
		m.upstreamRejects.Add(1)
	}
}
func (m *Metrics) UpstreamFailure() {
	if m != nil {
		m.upstreamFailures.Add(1)
	}
}

var sqlLatencyBounds = [...]time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (m *Metrics) MeasureSQL(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		observed := &statusWriter{ResponseWriter: w}
		next(observed, r)
		elapsed := time.Since(start)
		m.sqlDurationNS.Add(uint64(elapsed.Nanoseconds()))
		for i, bound := range sqlLatencyBounds {
			if elapsed <= bound {
				m.sqlLatency[i].Add(1)
			}
		}
		if observed.status >= 400 {
			m.sqlErrors.Add(1)
		}
		m.sqlRequests.Add(1)
	}
}

func (m *Metrics) Serve(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# TYPE barnacle_websocket_active gauge\nbarnacle_websocket_active %d\n", m.websocketActive.Load())
	_, _ = fmt.Fprintf(w, "# TYPE barnacle_sql_requests_total counter\nbarnacle_sql_requests_total %d\n", m.sqlRequests.Load())
	_, _ = fmt.Fprintf(w, "# TYPE barnacle_sql_errors_total counter\nbarnacle_sql_errors_total %d\n", m.sqlErrors.Load())
	_, _ = fmt.Fprintf(w, "# TYPE barnacle_limit_rejections_total counter\nbarnacle_limit_rejections_total{limit=\"http_queries\"} %d\nbarnacle_limit_rejections_total{limit=\"upstream_connections\"} %d\n", m.httpLimitRejects.Load(), m.upstreamRejects.Load())
	_, _ = fmt.Fprintf(w, "# TYPE barnacle_upstream_connection_failures_total counter\nbarnacle_upstream_connection_failures_total %d\n", m.upstreamFailures.Load())
	_, _ = fmt.Fprintln(w, "# TYPE barnacle_sql_duration_seconds histogram")
	for i, label := range [...]string{"0.01", "0.05", "0.1", "0.5", "1", "5"} {
		_, _ = fmt.Fprintf(w, "barnacle_sql_duration_seconds_bucket{le=%q} %d\n", label, m.sqlLatency[i].Load())
	}
	_, _ = fmt.Fprintf(w, "barnacle_sql_duration_seconds_bucket{le=\"+Inf\"} %d\nbarnacle_sql_duration_seconds_sum %.9f\nbarnacle_sql_duration_seconds_count %d\n", m.sqlRequests.Load(), float64(m.sqlDurationNS.Load())/1e9, m.sqlRequests.Load())
}

func (c Config) Ready(w http.ResponseWriter, _ *http.Request) {
	if c.ReadyPGAddr != "" {
		if c.ReadySlots != nil {
			select {
			case c.ReadySlots <- struct{}{}:
				defer func() { <-c.ReadySlots }()
			default:
				http.Error(w, "readiness probe in progress", http.StatusServiceUnavailable)
				return
			}
		}
		backend, err := c.DialPostgres(c.ReadyPGAddr)
		if err != nil {
			http.Error(w, "postgres unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = backend.Close()
	}
	w.Write([]byte("ok\n"))
}
