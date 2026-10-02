package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errHTTPResultTooLarge = errors.New("HTTP result exceeds configured size limit")

type responseBudget struct {
	writer    io.Writer
	max, used int64
}

func (b *responseBudget) Write(p []byte) (int, error) {
	if b.max > 0 && int64(len(p)) > b.max-b.used {
		return 0, errHTTPResultTooLarge
	}
	n, err := b.writer.Write(p)
	b.used += int64(n)
	return n, err
}

func rowWithinLimit(values [][]byte, max int64) bool {
	if max <= 0 {
		return true
	}
	var size int64
	for _, value := range values {
		size += int64(len(value))
		if size > max {
			return false
		}
	}
	return true
}

func canStream(rows pgx.Rows, types *pgtype.Map, rawText bool) bool {
	if rawText {
		return true
	}
	for _, description := range rows.FieldDescriptions() {
		if _, known := types.TypeForOID(description.DataTypeOID); !known {
			return false // The buffered path resolves custom array element types.
		}
	}
	return true
}

func streamSingle(w http.ResponseWriter, rows pgx.Rows, types *pgtype.Map, arrayMode, rawText bool, maxRowBytes, maxResponseBytes int64) error {
	defer rows.Close()
	fields := fieldsFromDescriptions(rows.FieldDescriptions())
	firstRow := rows.Next()
	if !firstRow && rows.Err() != nil {
		dbError(w, rows.Err())
		return nil
	}
	if firstRow && !rowWithinLimit(rows.RawValues(), maxRowBytes) {
		apiError(w, http.StatusRequestEntityTooLarge, errHTTPResultTooLarge)
		return nil
	}
	out := &responseBudget{writer: w, max: maxResponseBytes}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encodedFields, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err = out.Write([]byte(`{"fields":`)); err != nil {
		return err
	}
	if _, err = out.Write(encodedFields); err != nil {
		return err
	}
	if _, err = out.Write([]byte(`,"rows":[`)); err != nil {
		return err
	}
	for rowIndex := 0; firstRow; rowIndex++ {
		if !rowWithinLimit(rows.RawValues(), maxRowBytes) {
			return errHTTPResultTooLarge
		}
		if rowIndex > 0 {
			if _, err = out.Write([]byte{','}); err != nil {
				return err
			}
		}
		if err = streamRow(out, rows.RawValues(), fields, types, arrayMode, rawText); err != nil {
			return err
		}
		if rowIndex%64 == 0 {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		firstRow = rows.Next()
	}
	if err = rows.Err(); err != nil {
		return err
	}
	tag := rows.CommandTag()
	command, _, _ := strings.Cut(tag.String(), " ")
	encodedCommand, err := json.Marshal(command)
	if err != nil {
		return err
	}
	if _, err = out.Write([]byte(`],"command":`)); err != nil {
		return err
	}
	if _, err = out.Write(encodedCommand); err != nil {
		return err
	}
	rowCount := "null"
	if count := commandRowCount(tag.String()); count != nil {
		rowCount = strconv.FormatInt(*count, 10)
	}
	if _, err = out.Write([]byte(`,"rowCount":` + rowCount + `,"rowAsArray":` + strconv.FormatBool(arrayMode) + `}`)); err != nil {
		return err
	}
	return nil
}

func streamRow(w io.Writer, values [][]byte, fields []field, types *pgtype.Map, arrayMode, rawText bool) error {
	if arrayMode {
		if _, err := w.Write([]byte{'['}); err != nil {
			return err
		}
	} else if _, err := w.Write([]byte{'{'}); err != nil {
		return err
	}
	for i, value := range values {
		if i > 0 {
			if _, err := w.Write([]byte{','}); err != nil {
				return err
			}
		}
		if !arrayMode {
			name, err := json.Marshal(fields[i].Name)
			if err != nil {
				return err
			}
			if _, err = w.Write(name); err != nil {
				return err
			}
			if _, err = w.Write([]byte{':'}); err != nil {
				return err
			}
		}
		if err := streamValue(w, value, fields[i].DataTypeID, types, rawText); err != nil {
			return err
		}
	}
	closing := byte('}')
	if arrayMode {
		closing = ']'
	}
	_, err := w.Write([]byte{closing})
	return err
}

func streamValue(w io.Writer, value []byte, oid uint32, types *pgtype.Map, rawText bool) error {
	if value == nil {
		_, err := w.Write([]byte("null"))
		return err
	}
	if rawText {
		return writeJSONString(w, value)
	}
	if oid == pgtype.JSONOID || oid == pgtype.JSONBOID {
		_, err := w.Write(value) // PostgreSQL validated the JSON value.
		return err
	}
	if !isConvertedScalar(oid) && !isArrayType(oid, types) {
		return writeJSONString(w, value)
	}
	converted, err := pgTextValue(string(value), oid, types)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		return err
	}
	_, err = w.Write(encoded)
	return err
}

func isConvertedScalar(oid uint32) bool {
	switch oid {
	case pgtype.BoolOID, pgtype.Int2OID, pgtype.Int4OID, pgtype.Float4OID, pgtype.Float8OID:
		return true
	default:
		return false
	}
}

func isArrayType(oid uint32, types *pgtype.Map) bool {
	typ, ok := types.TypeForOID(oid)
	if !ok {
		return false
	}
	_, ok = typ.Codec.(*pgtype.ArrayCodec)
	return ok
}

// writeJSONString escapes one PostgreSQL text value in fixed-size chunks.
func writeJSONString(w io.Writer, value []byte) error {
	var buffer [32 << 10]byte
	used := 0
	flush := func() error {
		if used == 0 {
			return nil
		}
		_, err := w.Write(buffer[:used])
		used = 0
		return err
	}
	appendBytes := func(part []byte) error {
		if used+len(part) > len(buffer) {
			if err := flush(); err != nil {
				return err
			}
		}
		copy(buffer[used:], part)
		used += len(part)
		return nil
	}
	if err := appendBytes([]byte{'"'}); err != nil {
		return err
	}
	for len(value) > 0 {
		character := value[0]
		var escaped []byte
		var size int
		switch character {
		case '"':
			escaped, size = []byte(`\"`), 1
		case '\\':
			escaped, size = []byte(`\\`), 1
		case '\n':
			escaped, size = []byte(`\n`), 1
		case '\r':
			escaped, size = []byte(`\r`), 1
		case '\t':
			escaped, size = []byte(`\t`), 1
		default:
			if character < 0x20 {
				const hex = "0123456789abcdef"
				escaped, size = []byte{'\\', 'u', '0', '0', hex[character>>4], hex[character&0xf]}, 1
			} else if character < utf8.RuneSelf {
				escaped, size = value[:1], 1
			} else {
				_, size = utf8.DecodeRune(value)
				if size == 1 {
					escaped = []byte(`\ufffd`)
				} else {
					escaped = value[:size]
				}
			}
		}
		if err := appendBytes(escaped); err != nil {
			return err
		}
		value = value[size:]
	}
	if err := appendBytes([]byte{'"'}); err != nil {
		return err
	}
	return flush()
}
