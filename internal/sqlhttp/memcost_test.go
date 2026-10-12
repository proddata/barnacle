package sqlhttp

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

type costShape struct {
	name      string
	oids      []uint32
	values    [][]byte
	arrayMode bool
	rawText   bool
}

func repeatedList(element string, count int) string {
	return "{" + strings.TrimSuffix(strings.Repeat(element+",", count), ",") + "}"
}

func costShapes() []costShape {
	manyInts := make([]uint32, 12)
	manyValues := make([][]byte, 12)
	for i := range manyInts {
		manyInts[i], manyValues[i] = pgtype.Int4OID, []byte("123456")
	}
	jsonObject := []byte(`{"id":12345,"name":"widget","tags":["a","b","c"],"nested":{"x":1.5,"y":null}}`)
	jsonNumbers := []byte("[" + strings.TrimSuffix(strings.Repeat("1.5,", 2000), ",") + "]")
	return []costShape{
		{name: "one int", oids: []uint32{pgtype.Int4OID}, values: [][]byte{[]byte("7")}},
		{name: "one bool", oids: []uint32{pgtype.BoolOID}, values: [][]byte{[]byte("t")}},
		{name: "one short text", oids: []uint32{pgtype.TextOID}, values: [][]byte{[]byte("hello")}},
		{name: "four 100-byte texts", oids: []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID},
			values: [][]byte{[]byte(strings.Repeat("x", 100)), []byte(strings.Repeat("y", 100)), []byte(strings.Repeat("z", 100)), []byte(strings.Repeat("w", 100))}},
		{name: "twelve ints", oids: manyInts, values: manyValues},
		{name: "twelve ints, array mode", oids: manyInts, values: manyValues, arrayMode: true},
		{name: "empty row", oids: nil, values: nil},
		{name: "null text", oids: []uint32{pgtype.TextOID}, values: [][]byte{nil}},
		{name: "text array of three", oids: []uint32{pgtype.TextArrayOID}, values: [][]byte{[]byte("{1,a,b}")}},
		{name: "int array of 1000", oids: []uint32{pgtype.Int4ArrayOID}, values: [][]byte{[]byte(repeatedList("12345", 1000))}},
		{name: "int array of 1000, array mode", oids: []uint32{pgtype.Int4ArrayOID}, values: [][]byte{[]byte(repeatedList("12345", 1000))}, arrayMode: true},
		{name: "text array of 200 short strings", oids: []uint32{pgtype.TextArrayOID}, values: [][]byte{[]byte(repeatedList("ab", 200))}},
		{name: "jsonb object", oids: []uint32{pgtype.JSONBOID}, values: [][]byte{jsonObject}},
		{name: "jsonb array of 2000 numbers", oids: []uint32{pgtype.JSONBOID}, values: [][]byte{jsonNumbers}},
		{name: "raw text mode", oids: []uint32{pgtype.Int4OID, pgtype.TextArrayOID}, values: [][]byte{[]byte("7"), []byte("{1,a,b}")}, rawText: true},
	}
}

func heapInUse() uint64 {
	var stats runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// The memory budget is only as good as its estimate. Build real converted rows
// through the production code and compare the estimate with the retained heap
// plus the JSON encoding, so a change to row conversion that allocates more
// fails here.
func TestBufferedRowCostCoversRetainedHeapAndEncoding(t *testing.T) {
	types := pgtype.NewMap()
	for _, shape := range costShapes() {
		t.Run(shape.name, func(t *testing.T) {
			descriptions := make([]pgconn.FieldDescription, len(shape.oids))
			fields := make([]field, len(shape.oids))
			for i, oid := range shape.oids {
				name := fmt.Sprintf("column_%d", i)
				descriptions[i] = pgconn.FieldDescription{Name: name, DataTypeOID: oid}
				fields[i] = field{Name: name, DataTypeID: oid}
			}
			kinds := cellKinds(descriptions, types, shape.rawText)
			estimate := bufferedRowCost(shape.values, kinds, shape.arrayMode)

			// Enough rows for stable per-row averages, without minutes of work on huge rows.
			rows := int(min(5000, max(200, 4_000_000/max(estimate, 1))))
			before := heapInUse()
			data := make([]any, 0, rows)
			for i := 0; i < rows; i++ {
				row, err := convertRow(shape.values, descriptions, fields, types, shape.arrayMode, shape.rawText, map[uint32]map[int]struct{}{})
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, row)
			}
			after := heapInUse()
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			runtime.KeepAlive(data)

			retained := (int64(after) - int64(before)) / int64(rows)
			// Marshaling holds the values plus a few copies of the encoding.
			actual := retained + 3*int64(len(encoded))/int64(rows)
			t.Logf("estimate %d B/row, retained %d, encoded %d, needed %d", estimate, retained, int64(len(encoded))/int64(rows), actual)
			// Never undercount: the budget exists to stay below the memory limit.
			if estimate < actual {
				t.Errorf("row undercounted: estimate %d < actual %d bytes/row", estimate, actual)
			}
			// Nor be so pessimistic that ordinary results are refused.
			if actual > 100 && estimate > 4*actual {
				t.Errorf("row overcounted: estimate %d > 4x actual %d bytes/row", estimate, actual)
			}
		})
	}
}

func TestMemoryLeaseDistinguishesBusyFromTooLarge(t *testing.T) {
	budget := gateway.NewMemoryBudget(100)
	a, b := &memoryLease{budget: budget}, &memoryLease{budget: budget}
	if err := a.grow(60); err != nil {
		t.Fatal(err)
	}
	if err := b.grow(60); err != errHTTPMemoryBusy {
		t.Fatalf("request beside another that holds memory: %v, want busy", err)
	}
	if err := b.grow(101); err != errHTTPResultTooLarge {
		t.Fatalf("request larger than the whole budget: %v, want too large", err)
	}
	a.release()
	if err := b.grow(60); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err := b.grow(41); err != errHTTPResultTooLarge {
		t.Fatalf("one request outgrowing the budget alone: %v, want too large", err)
	}
	b.shrink(20)
	if err := a.grow(61); err != errHTTPMemoryBusy {
		t.Fatalf("shrunk lease should leave 60 of 100 free, not 61: %v", err)
	}
	b.release()
	b.release() // Releasing twice must not return memory twice.
	if err := a.grow(100); err != nil {
		t.Fatalf("budget leaked or double-released: %v", err)
	}
	a.release()
}

func TestMemoryLeaseNilBudgetIsUnlimited(t *testing.T) {
	for _, lease := range []*memoryLease{nil, {}} {
		if err := lease.grow(1 << 40); err != nil {
			t.Fatal(err)
		}
		lease.release()
	}
}

// A streamed row holding an array is parsed whole and encoded again, so its
// live heap is many times its text. The streaming path charges that while the
// row is converted; check the charge against the heap actually reached.
func TestStreamRowCostCoversArrayConversion(t *testing.T) {
	types := pgtype.NewMap()
	list := func(element string, n int) []byte { return []byte(repeatedList(element, n)) }
	for _, c := range []struct {
		name  string
		oid   uint32
		value []byte
	}{
		{"int4[] six-digit ints", pgtype.Int4ArrayOID, list("123456", 300_000)},
		{"int4[] one-digit ints", pgtype.Int4ArrayOID, list("7", 1_000_000)},
		{"text[] two-char strings", pgtype.TextArrayOID, list("ab", 700_000)},
		{"text[] 30-char strings", pgtype.TextArrayOID, list(strings.Repeat("s", 30), 80_000)},
		{"float8[] floats", pgtype.Float8ArrayOID, list("1234.5678", 250_000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var err error
			live := livePeak(func() { err = streamValue(io.Discard, c.value, c.oid, types, false) })
			if err != nil {
				t.Fatal(err)
			}
			charge := streamRowCost([][]byte{c.value}, []cellKind{kindArray})
			t.Logf("wire %d B: live peak %.1fx, charged %.1fx", len(c.value), float64(live)/float64(len(c.value)), float64(charge)/float64(len(c.value)))
			if raceEnabled {
				live = live * 4 / 5 // the race detector inflates live heap; the plain build is the reference
			}
			if charge < live {
				t.Errorf("charge %d B undercounts the live heap %d B", charge, live)
			}
			if charge > 4*live {
				t.Errorf("charge %d B is over 4x the live heap %d B", charge, live)
			}
		})
	}
}
