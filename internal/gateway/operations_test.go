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
		if !r.URL.Query().Has("fail") {
			inFlight := httptest.NewRecorder()
			m.Serve(inFlight, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if !strings.Contains(inFlight.Body.String(), "barnacle_http_sql_active 1\n") {
				t.Fatalf("active request missing from metrics:\n%s", inFlight.Body.String())
			}
		}
		if r.URL.Query().Has("fail") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Write([]byte("ok"))
	})
	for _, path := range []string{"/sql", "/sql?fail"} {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}
	m.OpenWebSocket()
	m.OpenWebSocket()
	m.CloseWebSocket()
	m.RejectHTTPQuery()
	m.RejectUpstream()
	m.UpstreamFailure()
	m.InterruptHTTPStream("result_limit")
	m.InterruptHTTPStream("unexpected")
	response := httptest.NewRecorder()
	m.Serve(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{
		"barnacle_websocket_active 1", "barnacle_websocket_connections_total 2", "barnacle_http_sql_active 0", "barnacle_sql_requests_total 2",
		"barnacle_sql_errors_total 1", "barnacle_sql_duration_seconds_bucket{le=\"+Inf\"} 2",
		"barnacle_sql_duration_seconds_count 2",
		"barnacle_limit_rejections_total{limit=\"http_queries\"} 1",
		"barnacle_limit_rejections_total{limit=\"upstream_connections\"} 1",
		"barnacle_upstream_connection_failures_total 1",
		"barnacle_sql_stream_interruptions_total{kind=\"result_limit\"} 1",
		"barnacle_sql_stream_interruptions_total{kind=\"query_or_transport\"} 1",
	} {
		if !strings.Contains(response.Body.String(), line+"\n") {
			t.Fatalf("metrics missing %q:\n%s", line, response.Body.String())
		}
	}
}

// What it tests: the upstream connection budget. Expected: acquiring never
// waits; once every slot is taken the next request is refused at once, and a
// released slot can be taken again. Refusing early is what keeps the gateway
// inside BARNACLE_MAX_CONNECTIONS.
func TestUpstreamSlotsRefuseWhenFullAndReuseReleased(t *testing.T) {
	cfg := Config{UpstreamSlots: make(chan struct{}, 2)}
	if !cfg.AcquireUpstream() || !cfg.AcquireUpstream() {
		t.Fatal("slots within the limit were refused")
	}
	if cfg.AcquireUpstream() {
		t.Fatal("slot granted beyond the limit")
	}
	cfg.ReleaseUpstream()
	if !cfg.AcquireUpstream() {
		t.Fatal("released slot was not reusable")
	}
}

// What it tests: the concurrent /sql request budget. Expected: same behavior as
// the upstream slots, and a gateway built without a budget (nil channel) places
// no limit on HTTP queries and tolerates acquire/release calls.
func TestHTTPSlotsRefuseWhenFullAndNilMeansUnlimited(t *testing.T) {
	cfg := Config{HTTPSlots: make(chan struct{}, 1)}
	if !cfg.AcquireHTTP() {
		t.Fatal("slot within the limit was refused")
	}
	if cfg.AcquireHTTP() {
		t.Fatal("slot granted beyond the limit")
	}
	cfg.ReleaseHTTP()
	if !cfg.AcquireHTTP() {
		t.Fatal("released slot was not reusable")
	}

	unlimited := Config{}
	for range 100 {
		if !unlimited.AcquireHTTP() {
			t.Fatal("a gateway without an HTTP budget refused a request")
		}
	}
	unlimited.ReleaseHTTP()
}

// What it tests: each rejection and failure counter, read back from the
// /metrics text the node's metrics scraper sees. Expected: every event moves
// only its own series. Login failures are split by transport, WebSocket login
// rejections by reason, and stream interruptions by kind, where an unknown kind
// or reason falls into a fixed catch-all series so label values stay bounded.
func TestMetricsCountRejectionsFailuresAndInterruptions(t *testing.T) {
	m := &Metrics{}
	m.RejectHTTPMemory()
	m.RejectHTTPMemory()
	m.RejectUpstream()
	m.UpstreamFailure()
	m.LoginFailure("http")
	m.LoginFailure("websocket")
	m.LoginFailure("websocket")
	m.RejectLogin("unauthenticated")
	m.RejectLogin("anything else")
	m.InterruptHTTPStream("result_limit")
	m.InterruptHTTPStream("timeout")
	m.InterruptHTTPStream("never heard of it")

	response := httptest.NewRecorder()
	m.Serve(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`barnacle_limit_rejections_total{limit="http_memory"} 2`,
		`barnacle_limit_rejections_total{limit="upstream_connections"} 1`,
		`barnacle_login_failures_total{transport="http"} 1`,
		`barnacle_login_failures_total{transport="websocket"} 2`,
		`barnacle_websocket_login_rejections_total{reason="unauthenticated"} 1`,
		`barnacle_websocket_login_rejections_total{reason="malformed"} 1`,
		`barnacle_sql_stream_interruptions_total{kind="result_limit"} 1`,
		`barnacle_sql_stream_interruptions_total{kind="timeout"} 1`,
		`barnacle_sql_stream_interruptions_total{kind="query_or_transport"} 1`,
		`barnacle_sql_stream_interruptions_total{kind="canceled"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// What it tests: counters called on a nil *Metrics, as in a handler built
// without metrics. Expected: no panic. Callers rely on this so they need no nil
// checks.
func TestNilMetricsIsSafe(t *testing.T) {
	var m *Metrics
	m.RejectHTTPMemory()
	m.RejectUpstream()
	m.UpstreamFailure()
	m.LoginFailure("http")
	m.RejectLogin("unauthenticated")
	m.InterruptHTTPStream("timeout")
}
