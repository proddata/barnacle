package sqlhttp

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proddata/barnacle/internal/gateway"
)

func TestMaxRows(t *testing.T) {
	const total = 50
	type response struct {
		Rows      []any    `json:"rows"`
		Command   string   `json:"command"`
		RowCount  *float64 `json:"rowCount"`
		Truncated bool     `json:"truncated"`
	}
	run := func(body string, headers ...string) (*httptest.ResponseRecorder, response) {
		cfg := memoryTestConfig(t, total, gateway.NewMemoryBudget(64<<20))
		cfg.PGAddr = resultBackendTag(t, total, "SELECT 50")
		req := httptest.NewRequest("POST", "/sql", strings.NewReader(body))
		req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		New(cfg).Serve(rec, req)
		var out response
		if rec.Code == 200 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return rec, out
	}

	t.Run("more rows than the limit keeps the true total", func(t *testing.T) {
		rec, out := run(`{"query":"select n from t","maxRows":10}`)
		if rec.Code != 200 || len(out.Rows) != 10 || !out.Truncated || out.RowCount == nil || *out.RowCount != total || out.Command != "SELECT" {
			t.Fatalf("%d rows=%d truncated=%v rowCount=%v command=%q", rec.Code, len(out.Rows), out.Truncated, out.RowCount, out.Command)
		}
		if rec.Header().Get("Barnacle-Truncated") != "true" {
			t.Fatal("missing Barnacle-Truncated header")
		}
	})
	t.Run("exactly the limit is not truncated", func(t *testing.T) {
		for _, limit := range []string{"50", "51"} {
			rec, out := run(`{"query":"select n from t","maxRows":` + limit + `}`)
			if len(out.Rows) != total || out.Truncated || out.RowCount == nil || *out.RowCount != total || rec.Header().Get("Barnacle-Truncated") != "" {
				t.Fatalf("maxRows %s: rows=%d truncated=%v rowCount=%v", limit, len(out.Rows), out.Truncated, out.RowCount)
			}
		}
		if _, out := run(`{"query":"select n from t"}`); len(out.Rows) != total || out.Truncated {
			t.Fatal("no limit must return everything")
		}
	})
	t.Run("one row past the limit is truncated", func(t *testing.T) {
		if _, out := run(`{"query":"select n from t","maxRows":49}`); len(out.Rows) != 49 || !out.Truncated {
			t.Fatalf("rows=%d truncated=%v", len(out.Rows), out.Truncated)
		}
	})
	t.Run("a write with returning is read to the end", func(t *testing.T) {
		_, out := run(`{"query":"insert into t values (1) returning n","maxRows":10}`)
		if len(out.Rows) != 10 || !out.Truncated || out.RowCount == nil || *out.RowCount != total {
			t.Fatalf("rows=%d truncated=%v rowCount=%v", len(out.Rows), out.Truncated, out.RowCount)
		}
	})
	t.Run("header is the default and the body overrides it", func(t *testing.T) {
		if _, out := run(`{"query":"select n from t"}`, "Barnacle-Max-Rows", "7"); len(out.Rows) != 7 || !out.Truncated {
			t.Fatalf("header: rows=%d truncated=%v", len(out.Rows), out.Truncated)
		}
		if _, out := run(`{"query":"select n from t","maxRows":3}`, "Barnacle-Max-Rows", "7"); len(out.Rows) != 3 {
			t.Fatalf("override: rows=%d", len(out.Rows))
		}
	})
	t.Run("invalid values", func(t *testing.T) {
		for _, body := range []string{`{"query":"select 1","maxRows":0}`, `{"query":"select 1","maxRows":-1}`} {
			if rec, _ := run(body); rec.Code != 400 {
				t.Fatalf("%s: %d", body, rec.Code)
			}
		}
		for _, value := range []string{"0", "-3", "abc", ""} {
			if rec, _ := run(`{"query":"select 1"}`, "Barnacle-Max-Rows", value); rec.Code != 400 && value != "" {
				t.Fatalf("header %q: %d", value, rec.Code)
			}
		}
		if rec, _ := run(`{"query":"select 1","maxRows":5}`, "Barnacle-Response-Mode", "streaming"); rec.Code != 400 {
			t.Fatalf("streaming with maxRows: %d", rec.Code)
		}
	})
}
