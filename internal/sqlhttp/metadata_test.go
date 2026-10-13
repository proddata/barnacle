package sqlhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

// A batch retains column metadata even when every query returns zero rows.
// The second query must share the first query's reservation, rather than each
// empty result appearing to cost nothing.
func TestEmptyResultsShareMetadataBudget(t *testing.T) {
	descriptions := twoColumns()
	cost := resultMetadataCost(descriptions)
	budget := gateway.NewMemoryBudget(2*cost - 1)
	lease := &memoryLease{budget: budget}
	defer lease.release()
	rows := &fakeRows{descriptions: descriptions, tag: "SELECT 0"}
	item, err := collect(context.Background(), rows, pgtype.NewMap(), false, false, 0, 0, nil, lease)
	if err != nil || len(item.Fields) != 2 {
		t.Fatalf("first empty result: fields=%d error=%v", len(item.Fields), err)
	}
	if lease.held == 0 {
		t.Fatal("empty result did not reserve metadata memory")
	}
	rows = &fakeRows{descriptions: descriptions, tag: "SELECT 0"}
	_, err = collect(context.Background(), rows, pgtype.NewMap(), false, false, 0, 0, nil, lease)
	if !errors.Is(err, errHTTPResultTooLarge) || !rows.closed {
		t.Fatalf("second empty result: error=%v closed=%v", err, rows.closed)
	}
	if !budgetIsEmpty(budget) {
		t.Fatal("failed batch did not return its reservations")
	}
}

func TestEmptyStreamReservesMetadataBeforeHeaders(t *testing.T) {
	descriptions := twoColumns()
	cost := resultMetadataCost(descriptions)
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%v", busy), func(t *testing.T) {
			max := cost - 1
			if busy {
				max = 2 * cost
			}
			budget := gateway.NewMemoryBudget(max)
			other := &memoryLease{budget: budget}
			defer other.release()
			if busy {
				if err := other.grow(cost + 1); err != nil {
					t.Fatal(err)
				}
			}
			lease := &memoryLease{budget: budget}
			defer lease.release()
			rows := &fakeRows{descriptions: descriptions, tag: "SELECT 0"}
			w, err := stream(rows, false, false, 0, 0, lease)
			wantStatus := 413
			if busy {
				wantStatus = 503
			}
			if err != nil || w.Code != wantStatus || !rows.closed {
				t.Fatalf("status=%d error=%v closed=%v", w.Code, err, rows.closed)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Fatal("metadata refusal produced a partial stream")
			}
			lease.release()
			other.release()
			if !budgetIsEmpty(budget) {
				t.Fatal("metadata refusal leaked memory")
			}
		})
	}
}

// Compare the estimate to real field containers and JSON output for ordinary
// empty-result batches. This exercises the missing allocation independently of
// row costs, without needing large requests or a running database.
func TestMetadataCostCoversHeapAndEncoding(t *testing.T) {
	for _, name := range []string{"column_name", strings.Repeat("x", 63), strings.Repeat("\x01", 63)} {
		t.Run(fmt.Sprintf("name_bytes=%d_escaped=%v", len(name), name[0] == '\x01'), func(t *testing.T) {
			const columns, queries = 128, 100
			descriptions := make([]pgconn.FieldDescription, columns)
			for i := range descriptions {
				descriptions[i] = pgconn.FieldDescription{
					Name: name, TableOID: ^uint32(0), TableAttributeNumber: ^uint16(0),
					DataTypeOID: ^uint32(0), DataTypeSize: -32768, TypeModifier: -2147483648,
				}
			}
			estimate := queries * resultMetadataCost(descriptions)
			before := heapInUse()
			results := make([]result, queries)
			for i := range results {
				results[i] = result{Fields: fieldsFromDescriptions(descriptions), Rows: []any{}, Command: "SELECT"}
			}
			after := heapInUse()
			encoded, err := json.Marshal(map[string]any{"results": results})
			if err != nil {
				t.Fatal(err)
			}
			runtime.KeepAlive(results)
			retained := max(int64(after)-int64(before), 0)
			needed := retained + 3*int64(len(encoded))
			t.Logf("estimate=%d retained=%d encoded=%d needed=%d bytes", estimate, retained, len(encoded), needed)
			if estimate < needed {
				t.Fatalf("metadata undercounted: estimate=%d needed=%d", estimate, needed)
			}
			if estimate > 4*needed {
				t.Fatalf("metadata estimate too pessimistic: estimate=%d needed=%d", estimate, needed)
			}
		})
	}
}
