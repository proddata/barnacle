package sqlhttp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriteJSONStringEscapesAcrossChunks(t *testing.T) {
	input := strings.Repeat("abc", 20_000) + "\"\\\n\x01☃" + strings.Repeat("xyz", 20_000)
	var encoded bytes.Buffer
	if err := writeJSONString(&encoded, []byte(input)); err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != input {
		t.Fatal("streamed JSON text changed the value")
	}
}

func TestResponseBudgetStopsBeforeOverLimitWrite(t *testing.T) {
	var output bytes.Buffer
	budget := &responseBudget{writer: &output, max: 5}
	if _, err := budget.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.Write([]byte("def")); err != errHTTPResultTooLarge {
		t.Fatalf("write error = %v, want size limit", err)
	}
	if output.String() != "abc" {
		t.Fatalf("output = %q, want abc", output.String())
	}
}

func TestRowWithinLimitCountsAllFields(t *testing.T) {
	values := [][]byte{[]byte("abc"), []byte("def")}
	if rowWithinLimit(values, 5) || !rowWithinLimit(values, 6) {
		t.Fatal("row byte limit was not enforced")
	}
}
