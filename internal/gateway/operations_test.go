package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadinessChecksConfiguredPostgresTransport(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
		close(accepted)
	}()
	cfg := Config{
		ReadyPGAddr: listener.Addr().String(),
		PGSSLMode:   "disable",
		ReadySlots:  make(chan struct{}, 1),
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	cfg.Ready(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("reachable PostgreSQL status = %d", response.Code)
	}
	<-accepted
	listener.Close()
	response = httptest.NewRecorder()
	cfg.Ready(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable PostgreSQL status = %d", response.Code)
	}
	cfg.ReadySlots <- struct{}{}
	response = httptest.NewRecorder()
	cfg.Ready(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "probe in progress") {
		t.Fatalf("concurrent readiness probe = %d, %q", response.Code, response.Body.String())
	}
	<-cfg.ReadySlots
	response = httptest.NewRecorder()
	(Config{}).Ready(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("readiness without database probe = %d", response.Code)
	}
}

func TestMetricsCountHTTPStatusAndWebSockets(t *testing.T) {
	m := &Metrics{}
	handler := m.MeasureSQL(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("fail") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Write([]byte("ok"))
	})
	for _, path := range []string{"/sql", "/sql?fail"} {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}
	m.websocketActive.Add(2)
	m.RejectHTTPQuery()
	m.RejectUpstream()
	m.UpstreamFailure()
	response := httptest.NewRecorder()
	m.Serve(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{
		"barnacle_websocket_active 2", "barnacle_sql_requests_total 2",
		"barnacle_sql_errors_total 1", "barnacle_sql_duration_seconds_bucket{le=\"+Inf\"} 2",
		"barnacle_sql_duration_seconds_count 2",
		"barnacle_limit_rejections_total{limit=\"http_queries\"} 1",
		"barnacle_limit_rejections_total{limit=\"upstream_connections\"} 1",
		"barnacle_upstream_connection_failures_total 1",
	} {
		if !strings.Contains(response.Body.String(), line+"\n") {
			t.Fatalf("metrics missing %q:\n%s", line, response.Body.String())
		}
	}
}
