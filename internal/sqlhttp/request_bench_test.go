package sqlhttp

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestBenchmarkBodies() map[string][]byte {
	return map[string][]byte{
		"simple":      []byte(`{"query":"select 1","params":[]}`),
		"scalars":     []byte(`{"query":"select $1, $2, $3, $4, $5","params":[42,"hello",true,null,1234567890123456789]}`),
		"structured":  []byte(`{"query":"select $1, $2","params":[{"x":1,"y":[1,2,3],"z":"hello"},[[1,2],[3,4]]]}`),
		"batch100":    []byte(`{"queries":[` + strings.Repeat(`{"query":"select $1","params":[42]},`, 99) + `{"query":"select $1","params":[42]}]}`),
		"text64KiB":   []byte(`{"query":"select $1","params":["` + strings.Repeat("x", 64<<10) + `"]}`),
		"objects2000": []byte(`{"query":"select $1","params":[[` + strings.Repeat(`{"x":0},`, 1999) + `{"x":0}]]}`),
	}
}

// Includes upload buffering, decoding and PostgreSQL parameter conversion.
func BenchmarkRequestDecode(b *testing.B) {
	for name, body := range requestBenchmarkBodies() {
		b.Run(name, func(b *testing.B) {
			req := httptest.NewRequest("POST", "/sql", nil)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				req.Body = ioNopCloserReader(body)
				buffered, err := readRequestBody(nil, req, 10<<20, nil)
				if err != nil {
					b.Fatal(err)
				}
				parsed, err := decodeRequest(req, buffered, nil)
				if err != nil {
					b.Fatal(err)
				}
				for _, q := range parsed.queries {
					for _, v := range q.Params {
						if _, err := pgParameter(v); err != nil {
							b.Fatal(err)
						}
					}
				}
			}
		})
	}
}

type benchmarkReader struct{ *bytes.Reader }

func (benchmarkReader) Close() error                { return nil }
func ioNopCloserReader(body []byte) benchmarkReader { return benchmarkReader{bytes.NewReader(body)} }

// Reproduces the small-object expansion that previously allocated over 100 MiB
// from a 2.3 MiB body. This includes the bounded raw upload, not authentication.
func BenchmarkRejectComplexRequest(b *testing.B) {
	body := []byte(`{"query":"select 1","params":[` + strings.Repeat(`{"x":0},`, 299999) + `{"x":0}]}`)
	req := httptest.NewRequest("POST", "/sql", nil)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		req.Body = ioNopCloserReader(body)
		buffered, err := readRequestBody(nil, req, 10<<20, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := decodeRequest(req, buffered, nil); err != errRequestComplexity {
			b.Fatalf("expected complexity rejection: %v", err)
		}
	}
}
