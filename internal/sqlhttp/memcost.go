package sqlhttp

import (
	"bytes"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

var errHTTPMemoryBusy = errors.New("buffered results in progress are using the gateway's memory budget; retry or use streaming")

// memoryLease is one request's share of the gateway-wide memory budget. It is
// released once the response has been written.
type memoryLease struct {
	budget *gateway.MemoryBudget
	held   int64
}

// grow reserves n more bytes. A request that could never fit, even alone, is
// too large (413); one that only misses because of other requests is busy (503).
func (l *memoryLease) grow(n int64) error {
	if l == nil || l.budget == nil {
		return nil
	}
	if l.held+n > l.budget.Max() {
		return errHTTPResultTooLarge
	}
	if !l.budget.TryReserve(n) {
		return errHTTPMemoryBusy
	}
	l.held += n
	return nil
}

// shrink returns part of what the lease holds, for example once a request body
// has been decoded and its raw bytes are gone.
func (l *memoryLease) shrink(n int64) {
	if l == nil || l.budget == nil || n <= 0 {
		return
	}
	n = min(n, l.held)
	l.budget.Release(n)
	l.held -= n
}

func (l *memoryLease) release() {
	if l == nil || l.budget == nil {
		return
	}
	l.budget.Release(l.held)
	l.held = 0
}

type cellKind uint8

const (
	kindText    cellKind = iota // kept as a Go string
	kindJSON                    // decoded into a tree of Go values
	kindArray                   // parsed into a slice of Go values
	kindUnknown                 // type unknown to pgx; may turn out to be an array
)

func cellKinds(descriptions []pgconn.FieldDescription, types *pgtype.Map, rawText bool) []cellKind {
	kinds := make([]cellKind, len(descriptions))
	if rawText {
		return kinds
	}
	for i, description := range descriptions {
		oid := description.DataTypeOID
		if oid == pgtype.JSONOID || oid == pgtype.JSONBOID {
			kinds[i] = kindJSON
			continue
		}
		typ, known := types.TypeForOID(oid)
		switch {
		case !known:
			kinds[i] = kindUnknown
		default:
			if _, isArray := typ.Codec.(*pgtype.ArrayCodec); isArray {
				kinds[i] = kindArray
			}
		}
	}
	return kinds
}

// nestedKind resolves kindUnknown by the value, since only arrays look like {...}.
func nestedKind(kind cellKind, v []byte) cellKind {
	if kind == kindUnknown {
		if len(v) > 0 && (v[0] == '{' || v[0] == '[') {
			return kindArray
		}
		return kindText
	}
	return kind
}

// streamRowCost estimates the transient heap one streamed row needs. Text is
// escaped in fixed-size chunks and JSON passes through, but an array column is
// parsed into Go values and re-encoded as a whole, which is far larger than its
// text. Only the current row is held, so the charge is returned after each row.
func streamRowCost(values [][]byte, kinds []cellKind) int64 {
	var cost int64
	for i, v := range values {
		if v == nil || nestedKind(kinds[i], v) != kindArray {
			continue
		}
		cost += 5*int64(len(v)) + 130*(int64(bytes.Count(v, []byte{','}))+1)
	}
	return cost
}

// bufferedRowCost estimates the heap one buffered row holds until its response
// is written: the converted Go values plus the transient copies json.Marshal
// makes while encoding. Wire size alone is a poor guide, because arrays and
// JSON expand into many small allocations; delimiter counts approximate those.
// The per-item figures were measured on the current Go and pgx, with margin;
// TestBufferedRowCost fails if a change makes rows heavier than this.
func bufferedRowCost(values [][]byte, kinds []cellKind, arrayMode bool) int64 {
	n := int64(len(values))
	cost := 360 + 56*n // a map row; Go allocates a full group even for one column
	if arrayMode {
		cost = 24 + 16*n
	}
	for i, v := range values {
		if v == nil {
			continue
		}
		cost += 4*int64(len(v)) + 48
		switch nestedKind(kinds[i], v) {
		case kindJSON:
			cost += 340*int64(bytes.Count(v, []byte{'{'})) + 120*int64(bytes.Count(v, []byte{','})) + 48*int64(bytes.Count(v, []byte{'['}))
		case kindArray:
			cost += 64 * (int64(bytes.Count(v, []byte{','})) + 1)
		}
	}
	return cost
}
