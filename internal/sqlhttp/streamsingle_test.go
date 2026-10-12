package sqlhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

// fakeRows is an in-memory pgx.Rows. It serves the given rows, then reports
// err (if set) in place of a clean end, as pgx does when the connection fails
// mid-result.
type fakeRows struct {
	descriptions []pgconn.FieldDescription
	rows         [][][]byte
	tag          string
	err          error
	next         int
	closed       bool
}

func (r *fakeRows) Close()                                       { r.closed = true }
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag(r.tag) }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return r.descriptions }
func (r *fakeRows) Scan(...any) error                            { return errors.New("not used") }
func (r *fakeRows) Values() ([]any, error)                       { return nil, errors.New("not used") }
func (r *fakeRows) TypeMap() *pgtype.Map                         { return pgtype.NewMap() }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) RawValues() [][]byte                          { return r.rows[r.next-1] }
func (r *fakeRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func description(name string, oid uint32) pgconn.FieldDescription {
	return pgconn.FieldDescription{Name: name, DataTypeOID: oid}
}

// stream runs streamSingle against a recorder and returns the recorder and the
// error it returned. Memory refusals are answered the way the handler does.
func stream(rows pgx.Rows, arrayMode, rawText bool, maxRow, maxResponse int64, lease *memoryLease) (*httptest.ResponseRecorder, error) {
	return streamWithMetrics(&gateway.Metrics{}, rows, arrayMode, rawText, maxRow, maxResponse, lease)
}

func streamWithMetrics(m *gateway.Metrics, rows pgx.Rows, arrayMode, rawText bool, maxRow, maxResponse int64, lease *memoryLease) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	h := New(&gateway.Config{Metrics: m})
	err := streamSingle(w, rows, pgtype.NewMap(), arrayMode, rawText, maxRow, maxResponse, lease, h.memoryRefusal)
	return w, err
}

func twoColumns() []pgconn.FieldDescription {
	return []pgconn.FieldDescription{description("id", pgtype.Int4OID), description("name", pgtype.TextOID)}
}

// What it tests: a normal streamed result in the default object mode, with a
// NULL and a value that needs JSON escaping. Expected: HTTP 200 and valid JSON
// with the field list, one object per row, the command name, and the row count
// taken from the command tag. Integers are numbers, not strings.
func TestStreamSingleWritesRowsAsObjects(t *testing.T) {
	rows := &fakeRows{
		descriptions: twoColumns(),
		rows:         [][][]byte{{[]byte("1"), []byte(`quote " and \ slash`)}, {[]byte("2"), nil}},
		tag:          "SELECT 2",
	}
	w, err := stream(rows, false, false, 0, 0, nil)
	if err != nil || w.Code != http.StatusOK {
		t.Fatalf("status %d, error %v", w.Code, err)
	}
	var got struct {
		Fields     []map[string]any `json:"fields"`
		Rows       []map[string]any `json:"rows"`
		Command    string           `json:"command"`
		RowCount   int              `json:"rowCount"`
		RowAsArray bool             `json:"rowAsArray"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", w.Body.String(), err)
	}
	if len(got.Fields) != 2 || got.Command != "SELECT" || got.RowCount != 2 || got.RowAsArray || len(got.Rows) != 2 {
		t.Fatalf("unexpected result: %s", w.Body.String())
	}
	if got.Rows[0]["id"] != float64(1) || got.Rows[0]["name"] != `quote " and \ slash` || got.Rows[1]["name"] != nil {
		t.Fatalf("unexpected rows: %v", got.Rows)
	}
	if !rows.closed {
		t.Fatal("rows were not closed")
	}
}

// What it tests: array mode (a client option). Expected: each row is a JSON
// array instead of an object, and rowAsArray is true so the driver knows.
func TestStreamSingleArrayMode(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), rows: [][][]byte{{[]byte("7"), []byte("x")}}, tag: "SELECT 1"}
	w, err := stream(rows, true, false, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"rows":[[7,"x"]]`) || !strings.Contains(w.Body.String(), `"rowAsArray":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// What it tests: raw-text mode, where values are sent exactly as PostgreSQL
// formatted them. Expected: even an integer stays a quoted string.
func TestStreamSingleRawTextKeepsValuesAsText(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), rows: [][][]byte{{[]byte("7"), []byte("x")}}, tag: "SELECT 1"}
	w, err := stream(rows, true, true, 0, 0, nil)
	if err != nil || !strings.Contains(w.Body.String(), `"rows":[["7","x"]]`) {
		t.Fatalf("error %v, body: %s", err, w.Body.String())
	}
}

// What it tests: a statement with no result rows (here an INSERT). Expected:
// 200 with an empty row list and the row count from the tag, not null.
func TestStreamSingleWithoutRows(t *testing.T) {
	rows := &fakeRows{descriptions: nil, tag: "INSERT 0 3"}
	w, err := stream(rows, false, false, 0, 0, nil)
	if err != nil || w.Code != http.StatusOK {
		t.Fatalf("status %d, error %v", w.Code, err)
	}
	if !strings.Contains(w.Body.String(), `"rows":[]`) || !strings.Contains(w.Body.String(), `"command":"INSERT"`) || !strings.Contains(w.Body.String(), `"rowCount":3`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// What it tests: a database error before any row (a failing query). Expected:
// a proper error status, not 200. This is why streamSingle looks at the first
// row before it sends headers.
func TestStreamSingleErrorBeforeFirstRowIsAnHTTPError(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), err: &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}}
	w, err := stream(rows, false, false, 0, 0, nil)
	if err != nil {
		t.Fatalf("error was returned instead of answered: %v", err)
	}
	if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "42P01") {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
}

// What it tests: a first row bigger than the per-row limit. Expected: 413 and
// nothing streamed, because the limit is checked before headers are sent.
func TestStreamSingleOversizeFirstRowIsRefusedCleanly(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), rows: [][][]byte{{[]byte("1"), []byte(strings.Repeat("x", 100))}}, tag: "SELECT 1"}
	w, err := stream(rows, false, false, 50, 0, nil)
	if err != nil || w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, error %v", w.Code, err)
	}
}

// What it tests: a later row bigger than the row limit, after the 200 and part
// of the body are already sent. Expected: the stream stops and the error is
// returned so the caller can count the interruption. The status can no longer
// change, which is the known limit of streaming.
func TestStreamSingleOversizeLaterRowInterruptsStream(t *testing.T) {
	rows := &fakeRows{
		descriptions: twoColumns(),
		rows:         [][][]byte{{[]byte("1"), []byte("ok")}, {[]byte("2"), []byte(strings.Repeat("x", 100))}},
		tag:          "SELECT 2",
	}
	w, err := stream(rows, false, false, 50, 0, nil)
	if !errors.Is(err, errHTTPResultTooLarge) || w.Code != http.StatusOK {
		t.Fatalf("error %v, status %d", err, w.Code)
	}
	if strings.Contains(w.Body.String(), `"command"`) {
		t.Fatal("an interrupted stream was finished as if complete")
	}
}

// What it tests: the whole-response size limit. Expected: once the next write
// would pass the limit the stream fails with the size error and the output is
// cut at a write boundary, never past the limit.
func TestStreamSingleStopsAtResponseLimit(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), rows: [][][]byte{{[]byte("1"), []byte("abc")}, {[]byte("2"), []byte("def")}}, tag: "SELECT 2"}
	w, err := stream(rows, false, false, 0, 90, nil)
	if !errors.Is(err, errHTTPResultTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if w.Body.Len() > 90 {
		t.Fatalf("wrote %d bytes past the 90 byte limit", w.Body.Len())
	}
}

// What it tests: an error from the database after rows were already sent.
// Expected: the error is returned, the body is unfinished (no closing
// command field), so a client sees invalid JSON instead of a truncated result
// that looks complete.
func TestStreamSingleErrorAfterRowsInterruptsStream(t *testing.T) {
	rows := &fakeRows{descriptions: twoColumns(), rows: [][][]byte{{[]byte("1"), []byte("a")}}, err: context.DeadlineExceeded}
	w, err := stream(rows, false, false, 0, 0, nil)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(w.Body.String(), `"command"`) {
		t.Fatalf("error %v, body %s", err, w.Body.String())
	}
}

func arrayRows(elements int) *fakeRows {
	value := "{" + strings.TrimSuffix(strings.Repeat("1,", elements), ",") + "}"
	return &fakeRows{
		descriptions: []pgconn.FieldDescription{description("a", pgtype.Int4ArrayOID)},
		rows:         [][][]byte{{[]byte(value)}},
		tag:          "SELECT 1",
	}
}

// What it tests: the memory budget on the streaming path. Decoding an array
// needs far more heap than its text, so the first row reserves an estimate.
// Cases: (1) the row can never fit the whole budget; (2) it would fit but other
// requests hold the memory right now; (3) it fits. Expected: (1) 413, (2) 503
// and the memory-rejection metric counted, (3) a normal 200. Refusals are
// answered before any header is sent.
func TestStreamSingleMemoryBudget(t *testing.T) {
	cost := streamRowCost(arrayRows(1000).rows[0], []cellKind{kindArray})
	if cost == 0 {
		t.Skip("array cost estimate is zero")
	}

	t.Run("never fits", func(t *testing.T) {
		lease := &memoryLease{budget: gateway.NewMemoryBudget(cost - 1)}
		w, err := stream(arrayRows(1000), false, false, 0, 0, lease)
		if err != nil || w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, error %v", w.Code, err)
		}
	})

	t.Run("busy", func(t *testing.T) {
		budget := gateway.NewMemoryBudget(cost + 10)
		other := &memoryLease{budget: budget}
		if err := other.grow(cost); err != nil {
			t.Fatal(err)
		}
		metrics := &gateway.Metrics{}
		w, err := streamWithMetrics(metrics, arrayRows(1000), false, false, 0, 0, &memoryLease{budget: budget})
		if err != nil || w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, error %v", w.Code, err)
		}
		scrape := httptest.NewRecorder()
		metrics.Serve(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !strings.Contains(scrape.Body.String(), `limit="http_memory"} 1`) {
			t.Fatal("memory rejection was not counted")
		}
		other.release()
	})

	t.Run("fits and is released", func(t *testing.T) {
		budget := gateway.NewMemoryBudget(cost * 2)
		lease := &memoryLease{budget: budget}
		w, err := stream(arrayRows(1000), false, false, 0, 0, lease)
		if err != nil || w.Code != http.StatusOK {
			t.Fatalf("status %d, error %v", w.Code, err)
		}
		lease.release()
		if !budget.TryReserve(cost * 2) {
			t.Fatal("memory was not given back")
		}
	})
}

// What it tests: the rule for choosing the streaming path. Expected: raw-text
// results always stream; results whose column types are all known to the
// driver stream; a column with an unknown type (such as an array of a custom
// type) does not, because only the buffered path can look up its element type.
func TestCanStream(t *testing.T) {
	types := pgtype.NewMap()
	known := &fakeRows{descriptions: twoColumns()}
	unknown := &fakeRows{descriptions: []pgconn.FieldDescription{description("x", 4000000)}}
	if !canStream(known, types, false) {
		t.Error("known types should stream")
	}
	if canStream(unknown, types, false) {
		t.Error("unknown type should use the buffered path")
	}
	if !canStream(unknown, types, true) {
		t.Error("raw text should always stream")
	}
}

// What it tests: the label under which an interrupted stream is counted.
// Expected: size errors, cancellations and timeouts have their own labels and
// everything else falls into the catch-all, matching the metrics series.
func TestInterruptedResultKind(t *testing.T) {
	for err, want := range map[error]string{
		errHTTPResultTooLarge:          "result_limit",
		context.Canceled:               "canceled",
		context.DeadlineExceeded:       "timeout",
		errors.New("connection reset"): "query_or_transport",
	} {
		if got := interruptedResultKind(err); got != want {
			t.Errorf("%v: got %q, want %q", err, got, want)
		}
	}
}
