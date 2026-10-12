package sqlhttp

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

// resultBackend logs every client in and answers each query with `rows` rows of
// one int4 column.
func resultBackend(t *testing.T, rows int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.AuthenticationCleartextPassword{})
				if backend.Flush() != nil {
					return
				}
				if _, err := backend.Receive(); err != nil {
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 2}})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if backend.Flush() != nil {
					return
				}
				for {
					msg, err := backend.Receive()
					if err != nil {
						return
					}
					if _, ok := msg.(*pgproto3.Sync); !ok {
						continue
					}
					backend.Send(&pgproto3.ParseComplete{})
					backend.Send(&pgproto3.BindComplete{})
					backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
						{Name: []byte("n"), DataTypeOID: pgtype.Int4OID, DataTypeSize: 4, TypeModifier: -1},
					}})
					for i := 0; i < rows; i++ {
						backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1234567")}})
					}
					backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT")})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					if backend.Flush() != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func serveSelect(t *testing.T, cfg *gateway.Config) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/sql", strings.NewReader(`{"query":"select n from t"}`))
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	response := httptest.NewRecorder()
	New(cfg).Serve(response, req)
	return response
}

func memoryTestConfig(t *testing.T, rows int, budget *gateway.MemoryBudget) *gateway.Config {
	return &gateway.Config{
		PGAddr: resultBackend(t, rows), PGSSLMode: "disable", QueryTimeout: 5 * time.Second,
		HTTPSlots: make(chan struct{}, 8), UpstreamSlots: make(chan struct{}, 8),
		MaxHTTPRequestBytes: 1 << 20, MaxHTTPRowBytes: 1 << 20,
		MaxHTTPBufferedBytes: 10 << 20, MaxHTTPResponseBytes: 64 << 20,
		HTTPMemory: budget, Metrics: &gateway.Metrics{},
	}
}

func budgetIsEmpty(budget *gateway.MemoryBudget) bool {
	ok := budget.TryReserve(budget.Max())
	if ok {
		budget.Release(budget.Max())
	}
	return ok
}

func TestBufferedResultsUseTheMemoryBudget(t *testing.T) {
	const rows = 2000 // About 2000 * 400 bytes of decoded rows.
	t.Run("fits", func(t *testing.T) {
		budget := gateway.NewMemoryBudget(64 << 20)
		response := serveSelect(t, memoryTestConfig(t, rows, budget))
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"rowCount":`) {
			t.Fatalf("%d %.200s", response.Code, response.Body.String())
		}
		if !budgetIsEmpty(budget) {
			t.Fatal("memory was not returned after a successful response")
		}
	})
	t.Run("alone larger than the whole budget", func(t *testing.T) {
		budget := gateway.NewMemoryBudget(100 << 10)
		response := serveSelect(t, memoryTestConfig(t, rows, budget))
		if response.Code != 413 {
			t.Fatalf("%d %.200s", response.Code, response.Body.String())
		}
		if !budgetIsEmpty(budget) {
			t.Fatal("memory was not returned after a rejected response")
		}
	})
	t.Run("budget busy with another request", func(t *testing.T) {
		budget := gateway.NewMemoryBudget(1 << 20)
		older := &memoryLease{budget: budget}
		if err := older.grow(1<<20 - 1000); err != nil {
			t.Fatal(err)
		}
		cfg := memoryTestConfig(t, rows, budget)
		response := serveSelect(t, cfg)
		if response.Code != 503 || !strings.Contains(response.Body.String(), "memory budget") {
			t.Fatalf("%d %.200s", response.Code, response.Body.String())
		}
		var scrape strings.Builder
		metrics := httptest.NewRecorder()
		cfg.Metrics.Serve(metrics, nil)
		scrape.WriteString(metrics.Body.String())
		if !strings.Contains(scrape.String(), `barnacle_limit_rejections_total{limit="http_memory"} 1`) {
			t.Fatalf("rejection not counted:\n%s", scrape.String())
		}
		older.release()
		if !budgetIsEmpty(budget) {
			t.Fatal("memory leaked after a busy rejection")
		}
	})
	t.Run("without a budget", func(t *testing.T) {
		response := serveSelect(t, memoryTestConfig(t, rows, nil))
		if response.Code != 200 {
			t.Fatalf("%d %.200s", response.Code, response.Body.String())
		}
	})
}
