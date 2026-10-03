package sqlhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// pgParameter converts the JSON parameter shape to PostgreSQL text input.
func pgParameter(value any) (any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	case map[string]any:
		encoded, err := json.Marshal(v)
		return string(encoded), err
	case []any:
		return pgArrayParameter(v)
	default:
		return nil, fmt.Errorf("unsupported JSON parameter type %T", value)
	}
}

func pgArrayParameter(values []any) (string, error) {
	parts := make([]string, len(values))
	for i, value := range values {
		switch v := value.(type) {
		case nil:
			parts[i] = "NULL"
		case []any:
			text, err := pgArrayParameter(v)
			if err != nil {
				return "", err
			}
			parts[i] = text
		case string:
			parts[i] = quotePGArray(v)
		case map[string]any:
			encoded, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			parts[i] = quotePGArray(string(encoded))
		case json.Number:
			parts[i] = v.String()
		case bool:
			parts[i] = strconv.FormatBool(v)
		default:
			return "", fmt.Errorf("unsupported array parameter type %T", value)
		}
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}

func quotePGArray(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func pgTextValue(text string, oid uint32, types *pgtype.Map) (any, error) {
	if typ, ok := types.TypeForOID(oid); ok {
		if array, ok := typ.Codec.(*pgtype.ArrayCodec); ok {
			delimiter := byte(',')
			if array.ElementType.OID == pgtype.BoxOID {
				delimiter = ';'
			}
			return parsePGArray(text, array.ElementType.OID, delimiter, types)
		}
	}
	switch oid {
	case pgtype.BoolOID:
		return text == "t", nil
	case pgtype.Int2OID, pgtype.Int4OID:
		value, err := strconv.ParseInt(text, 10, 32)
		return value, err
	case pgtype.Float4OID, pgtype.Float8OID:
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return text, nil
		}
		return value, nil
	case pgtype.JSONOID, pgtype.JSONBOID:
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return text, nil
	}
}

type pgArrayParser struct {
	text      string
	position  int
	element   uint32
	delimiter byte
	types     *pgtype.Map
}

func parsePGArray(text string, element uint32, delimiter byte, types *pgtype.Map) ([]any, error) {
	if strings.HasPrefix(text, "[") {
		index := strings.IndexByte(text, '=')
		if index < 0 {
			return nil, errors.New("invalid PostgreSQL array bounds")
		}
		text = text[index+1:]
	}
	parser := pgArrayParser{text: strings.TrimSpace(text), element: element, delimiter: delimiter, types: types}
	values, err := parser.array()
	if err != nil || parser.position != len(parser.text) {
		return nil, errors.New("invalid PostgreSQL array")
	}
	return values, nil
}

func (p *pgArrayParser) array() ([]any, error) {
	if p.position >= len(p.text) || p.text[p.position] != '{' {
		return nil, errors.New("missing PostgreSQL array opener")
	}
	p.position++
	values := make([]any, 0)
	for {
		if p.position >= len(p.text) {
			return nil, errors.New("unterminated PostgreSQL array")
		}
		if p.text[p.position] == '}' {
			p.position++
			return values, nil
		}
		var value any
		var err error
		if p.text[p.position] == '{' {
			value, err = p.array()
		} else {
			value, err = p.item()
		}
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		if p.position < len(p.text) && p.text[p.position] == p.delimiter {
			p.position++
			continue
		}
		if p.position >= len(p.text) || p.text[p.position] != '}' {
			return nil, errors.New("invalid PostgreSQL array separator")
		}
	}
}

func (p *pgArrayParser) item() (any, error) {
	if p.text[p.position] == '"' {
		p.position++
		var value strings.Builder
		for p.position < len(p.text) {
			character := p.text[p.position]
			p.position++
			switch character {
			case '"':
				return pgTextValue(value.String(), p.element, p.types)
			case '\\':
				if p.position >= len(p.text) {
					return nil, errors.New("invalid PostgreSQL array escape")
				}
				character = p.text[p.position]
				p.position++
			}
			value.WriteByte(character)
		}
		return nil, errors.New("unterminated PostgreSQL array string")
	}
	start := p.position
	for p.position < len(p.text) && p.text[p.position] != p.delimiter && p.text[p.position] != '}' {
		p.position++
	}
	value := strings.TrimSpace(p.text[start:p.position])
	if value == "NULL" {
		return nil, nil
	}
	return pgTextValue(value, p.element, p.types)
}
