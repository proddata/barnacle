package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestJSONParametersMatchPostgresTextInput(t *testing.T) {
	tests := []struct {
		input any
		want  any
	}{
		{nil, nil},
		{true, "true"},
		{json.Number("42.125"), "42.125"},
		{"unquoted text", "unquoted text"},
		{map[string]any{"answer": json.Number("42")}, `{"answer":42}`},
		{[]any{true, nil, "NULL", "a,b", `q"z`, `a\b`}, `{true,NULL,"NULL","a,b","q\"z","a\\b"}`},
		{[]any{[]any{json.Number("1"), json.Number("2")}, []any{json.Number("3"), json.Number("4")}}, `{{1,2},{3,4}}`},
	}
	for _, test := range tests {
		got, err := pgParameter(test.input)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("pgParameter(%#v) = %#v, want %#v", test.input, got, test.want)
		}
	}
}

func TestPostgresTextArraysAndScalarTypes(t *testing.T) {
	types := pgtype.NewMap()
	tests := []struct {
		text string
		oid  uint32
		want any
	}{
		{"t", pgtype.BoolOID, true},
		{"32767", pgtype.Int2OID, int64(32767)},
		{"9223372036854775807", pgtype.Int8OID, "9223372036854775807"},
		{"Infinity", pgtype.Float8OID, "Infinity"},
		{`{"a":1}`, pgtype.JSONBOID, map[string]any{"a": json.Number("1")}},
		{`{1,NULL,3}`, pgtype.Int4ArrayOID, []any{int64(1), nil, int64(3)}},
		{`{{1,2},{3,4}}`, pgtype.Int4ArrayOID, []any{[]any{int64(1), int64(2)}, []any{int64(3), int64(4)}}},
		{`{"a,b","NULL",NULL,"q\"z"}`, pgtype.TextArrayOID, []any{"a,b", "NULL", nil, `q"z`}},
	}
	for _, test := range tests {
		got, err := pgTextValue(test.text, test.oid, types)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("pgTextValue(%q, %d) = %#v, want %#v", test.text, test.oid, got, test.want)
		}
	}
}
