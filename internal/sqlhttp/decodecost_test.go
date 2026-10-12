package sqlhttp

import (
	"encoding/json"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

type decodeShape struct {
	name string
	body []byte
}

func decodeShapes() []decodeShape {
	marshal := func(value any) []byte {
		encoded, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		return encoded
	}
	manyStrings := make([]any, 3000)
	for i := range manyStrings {
		manyStrings[i] = strings.Repeat("v", 200)
	}
	manyNumbers := make([]any, 4000)
	for i := range manyNumbers {
		manyNumbers[i] = i * 1013
	}
	objects := make([]any, 1500)
	for i := range objects {
		objects[i] = map[string]any{"id": i, "name": "item", "tags": []any{"a", "b"}}
	}
	queries := make([]any, 100)
	for i := range queries {
		queries[i] = map[string]any{"query": "insert into t(a, b) values ($1, $2)", "params": []any{i, strings.Repeat("p", 20_000)}}
	}
	return []decodeShape{
		{"point lookup", marshal(map[string]any{"query": "select * from orders where id = $1", "params": []any{42}})},
		{"one 9.9 MB string", marshal(map[string]any{"query": "select length($1::text)", "params": []any{strings.Repeat("x", 9_900_000)}})},
		{"one 2 MB string", marshal(map[string]any{"query": "select length($1::text)", "params": []any{strings.Repeat("x", 2_000_000)}})},
		{"3000 strings of 200 B", marshal(map[string]any{"query": "insert into t select unnest($1::text[])", "params": []any{manyStrings}})},
		{"4000 numbers", marshal(map[string]any{"query": "select $1::int[]", "params": []any{manyNumbers}})},
		{"1500 small objects", marshal(map[string]any{"query": "select $1::jsonb", "params": []any{objects}})},
		{"100-query batch, 20 KB each", marshal(map[string]any{"queries": queries})},
	}
}

func heapNow() int64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// Decoding is the request-side memory spike. Measure the live heap it reaches
// and what stays reachable afterwards, and require the budget's charge to cover
// both. The body itself is charged separately while it is read (the 1 in the
// retained charge); pgx's send
// buffer, about one more body, is allocated after decoding.
func TestDecodeMemoryCostCoversDecoding(t *testing.T) {
	for _, shape := range decodeShapes() {
		t.Run(shape.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/sql", nil)
			tokens, err := scanJSONComplexity(shape.body)
			if err != nil {
				t.Fatal(err)
			}
			bodyLen := int64(len(shape.body))

			var decodeErr error
			peak := livePeak(func() { _, decodeErr = decodeRequest(req, shape.body, nil) })
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			before := heapNow()
			held, err := decodeRequest(req, shape.body, nil)
			if err != nil {
				t.Fatal(err)
			}
			retained := heapNow() - before
			runtime.KeepAlive(held)

			charge := decodeMemoryCost(bodyLen, tokens)
			retainedCharge := (1+decodeBodyFactor-decodeReleased)*bodyLen + int64(tokens)*decodeTokenCost
			retainedNeeded := retained + bodyLen // the decoded values plus pgx's send buffer
			t.Logf("body %d B, %d tokens: decode peak %.2fx, retained %.2fx (+1x send buffer); charged %.2fx then %.2fx",
				bodyLen, tokens, float64(peak)/float64(bodyLen), float64(retained)/float64(bodyLen),
				float64(charge)/float64(bodyLen), float64(retainedCharge)/float64(bodyLen))
			// Allocator and race-detector overhead on tiny requests is noise next to a
			// budget of tens of MiB, so allow a small absolute tolerance.
			const tolerance = 64 << 10
			if peak > charge+tolerance {
				t.Errorf("decode charge %d B undercounts the live heap decoding reaches (%d B)", charge, peak)
			}
			if retainedNeeded > retainedCharge+tolerance {
				t.Errorf("retained charge %d B undercounts the decoded values and send buffer (%d B)", retainedCharge, retainedNeeded)
			}
			if bodyLen > 1<<20 && charge > 6*bodyLen {
				t.Errorf("decode charge %d B is over 6x the %d B body", charge, bodyLen)
			}
		})
	}
}
