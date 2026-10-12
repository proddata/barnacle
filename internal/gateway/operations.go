package gateway

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Metrics struct {
	mu                   sync.Mutex
	websocketActive      int64
	websocketConnections uint64
	httpSQLActive        int64
	sqlRequests          uint64
	sqlErrors            uint64
	sqlDurationSeconds   float64
	sqlLatency           [6]uint64
	httpLimitRejects     uint64
	memoryRejects        uint64
	upstreamRejects      uint64
	upstreamFailures     uint64
	streamInterruptions  [4]uint64
	loginRejections      [2]uint64
	loginFailures        [2]uint64
}

func (m *Metrics) OpenWebSocket() {
	m.mu.Lock()
	m.websocketActive++
	m.websocketConnections++
	m.mu.Unlock()
}
func (m *Metrics) CloseWebSocket() {
	m.mu.Lock()
	m.websocketActive--
	m.mu.Unlock()
}
func (m *Metrics) RejectHTTPQuery() {
	if m != nil {
		m.mu.Lock()
		m.httpLimitRejects++
		m.mu.Unlock()
	}
}
func (m *Metrics) RejectHTTPMemory() {
	if m != nil {
		m.mu.Lock()
		m.memoryRejects++
		m.mu.Unlock()
	}
}
func (m *Metrics) RejectUpstream() {
	if m != nil {
		m.mu.Lock()
		m.upstreamRejects++
		m.mu.Unlock()
	}
}
func (m *Metrics) UpstreamFailure() {
	if m != nil {
		m.mu.Lock()
		m.upstreamFailures++
		m.mu.Unlock()
	}
}
func (m *Metrics) InterruptHTTPStream(kind string) {
	if m == nil {
		return
	}
	index := 3 // Unknown kinds stay in the fixed query_or_transport series.
	switch kind {
	case "result_limit":
		index = 0
	case "canceled":
		index = 1
	case "timeout":
		index = 2
	}
	m.mu.Lock()
	m.streamInterruptions[index]++
	m.mu.Unlock()
}

// RejectLogin counts a WebSocket session closed before login completed.
func (m *Metrics) RejectLogin(reason string) {
	if m == nil {
		return
	}
	index := 1 // Unknown reasons stay in the fixed malformed series.
	if reason == "unauthenticated" {
		index = 0
	}
	m.mu.Lock()
	m.loginRejections[index]++
	m.mu.Unlock()
}

// LoginFailure counts a login PostgreSQL refused for bad credentials or an
// unauthorized role (SQLSTATE class 28), which the ingress cannot see: over
// WebSocket the login happens inside the tunnel, and over HTTP a bad password
// and a malformed query share a status code.
func (m *Metrics) LoginFailure(transport string) {
	if m == nil {
		return
	}
	index := 0
	if transport == "websocket" {
		index = 1
	}
	m.mu.Lock()
	m.loginFailures[index]++
	m.mu.Unlock()
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
		m.mu.Lock()
		m.httpSQLActive++
		m.mu.Unlock()
		defer func() {
			elapsed := time.Since(start)
			m.mu.Lock()
			m.httpSQLActive--
			m.sqlDurationSeconds += elapsed.Seconds()
			for i, bound := range sqlLatencyBounds {
				if elapsed <= bound {
					m.sqlLatency[i]++
				}
			}
			if observed.status >= 400 {
				m.sqlErrors++
			}
			m.sqlRequests++
			m.mu.Unlock()
		}()
		next(observed, r)
	}
}

func (m *Metrics) Serve(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	websocketActive := m.websocketActive
	websocketConnections := m.websocketConnections
	httpSQLActive := m.httpSQLActive
	sqlRequests := m.sqlRequests
	sqlErrors := m.sqlErrors
	sqlDurationSeconds := m.sqlDurationSeconds
	sqlLatency := m.sqlLatency
	httpLimitRejects := m.httpLimitRejects
	memoryRejects := m.memoryRejects
	upstreamRejects := m.upstreamRejects
	upstreamFailures := m.upstreamFailures
	streamInterruptions := m.streamInterruptions
	loginRejections := m.loginRejections
	loginFailures := m.loginFailures
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP barnacle_websocket_active Currently open WebSocket connections.\n# TYPE barnacle_websocket_active gauge\nbarnacle_websocket_active %d\n", websocketActive)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_websocket_connections_total Accepted WebSocket upgrades since startup.\n# TYPE barnacle_websocket_connections_total counter\nbarnacle_websocket_connections_total %d\n", websocketConnections)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_http_sql_active HTTP SQL requests currently in progress.\n# TYPE barnacle_http_sql_active gauge\nbarnacle_http_sql_active %d\n", httpSQLActive)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_sql_requests_total Completed HTTP SQL requests since startup.\n# TYPE barnacle_sql_requests_total counter\nbarnacle_sql_requests_total %d\n", sqlRequests)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_sql_errors_total HTTP SQL responses with status 400 or higher.\n# TYPE barnacle_sql_errors_total counter\nbarnacle_sql_errors_total %d\n", sqlErrors)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_limit_rejections_total Requests rejected by a configured concurrency limit.\n# TYPE barnacle_limit_rejections_total counter\nbarnacle_limit_rejections_total{limit=\"http_queries\"} %d\nbarnacle_limit_rejections_total{limit=\"upstream_connections\"} %d\nbarnacle_limit_rejections_total{limit=\"http_memory\"} %d\n", httpLimitRejects, upstreamRejects, memoryRejects)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_upstream_connection_failures_total Failed PostgreSQL connection attempts.\n# TYPE barnacle_upstream_connection_failures_total counter\nbarnacle_upstream_connection_failures_total %d\n", upstreamFailures)
	_, _ = fmt.Fprintf(w, "# HELP barnacle_websocket_login_rejections_total WebSocket sessions closed before PostgreSQL login completed.\n# TYPE barnacle_websocket_login_rejections_total counter\nbarnacle_websocket_login_rejections_total{reason=\"unauthenticated\"} %d\nbarnacle_websocket_login_rejections_total{reason=\"malformed\"} %d\n", loginRejections[0], loginRejections[1])
	_, _ = fmt.Fprintf(w, "# HELP barnacle_login_failures_total Logins PostgreSQL refused for bad credentials or an unauthorized role (SQLSTATE class 28).\n# TYPE barnacle_login_failures_total counter\nbarnacle_login_failures_total{transport=\"http\"} %d\nbarnacle_login_failures_total{transport=\"websocket\"} %d\n", loginFailures[0], loginFailures[1])
	_, _ = fmt.Fprintln(w, "# HELP barnacle_sql_stream_interruptions_total HTTP SQL result streams interrupted after streaming began.\n# TYPE barnacle_sql_stream_interruptions_total counter")
	for i, kind := range [...]string{"result_limit", "canceled", "timeout", "query_or_transport"} {
		_, _ = fmt.Fprintf(w, "barnacle_sql_stream_interruptions_total{kind=%q} %d\n", kind, streamInterruptions[i])
	}
	_, _ = fmt.Fprintln(w, "# HELP barnacle_sql_duration_seconds Duration of completed HTTP SQL requests in seconds.\n# TYPE barnacle_sql_duration_seconds histogram")
	for i, label := range [...]string{"0.01", "0.05", "0.1", "0.5", "1", "5"} {
		_, _ = fmt.Fprintf(w, "barnacle_sql_duration_seconds_bucket{le=%q} %d\n", label, sqlLatency[i])
	}
	_, _ = fmt.Fprintf(w, "barnacle_sql_duration_seconds_bucket{le=\"+Inf\"} %d\nbarnacle_sql_duration_seconds_sum %.9f\nbarnacle_sql_duration_seconds_count %d\n", sqlRequests, sqlDurationSeconds, sqlRequests)
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
