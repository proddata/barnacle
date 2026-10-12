package sqlhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxJSONDepth             = 32
	maxJSONTokens            = 16_384 // Includes object keys, values, and containers.
	maxQueryParameters       = 4_096
	maxConnectionStringBytes = 16 << 10
	maxBearerTokenBytes      = 16 << 10
	maxUsernameBytes         = 1 << 10
	maxPasswordBytes         = 4 << 10
	maxDatabaseBytes         = 1 << 10
	httpConnectTimeout       = 5 * time.Second
)

var (
	errRequestComplexity = errors.New("request exceeds JSON depth, element, or parameter limit")
	errCredentialSize    = errors.New("connection credentials exceed size limit")
)

// Read the complete upload before acquiring a PostgreSQL connection. Both known
// and chunked body lengths are bounded before JSON can allocate object trees.
// Each chunk is charged to the memory budget as it arrives, so a body that has
// been announced but not sent costs nothing.
func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64, lease *memoryLease) ([]byte, error) {
	if limit <= 0 {
		limit = 10 << 20
	}
	if r.ContentLength > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	reader := http.MaxBytesReader(w, r.Body, limit)
	body := make([]byte, 0, min(max(r.ContentLength, 0), 64<<10)+512)
	chunk := make([]byte, 32<<10)
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			if growErr := lease.grow(int64(n)); growErr != nil {
				return nil, growErr
			}
			body = append(body, chunk[:n]...)
		}
		if err == io.EOF {
			return body, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Decoding holds the raw body, the decoder's copy (grown by doubling) and the
// decoded values at once; measured live peaks are about four times the body on
// top of the body itself, which is charged while it is read. Afterwards the
// decoded values and pgx's send buffer, about the body's size each, stay until
// the request ends. Every decoded token also costs a map slot or boxed value;
// their count is bounded by maxJSONTokens.
const (
	decodeBodyFactor = 4
	decodeReleased   = 3 // of the decode charge, returned once decoding is done
	decodeTokenCost  = 128
)

func decodeMemoryCost(bodyLen int64, tokens int) int64 {
	// The eighth of the body is margin for peaks the measurement misses.
	return decodeBodyFactor*bodyLen + bodyLen/8 + int64(tokens)*decodeTokenCost
}

type parsedRequest struct {
	queries   []query
	batch     bool
	options   executionOptions
	streaming bool
	maxRows   int64 // Barnacle-Max-Rows default; a query's maxRows overrides it.
}

// decodeRequest runs after the upload size check and PostgreSQL authentication.
// Its token guard bounds allocations before JSON expands parameter structures.
//
// charge, when set, reserves the memory decoding needs once the cheap complexity
// scan has passed and before anything is allocated.
func decodeRequest(r *http.Request, body []byte, charge func(tokens int) error) (parsedRequest, error) {
	var parsed parsedRequest
	tokens, err := scanJSONComplexity(body)
	if err != nil {
		return parsed, err
	}
	if charge != nil {
		if err := charge(tokens); err != nil {
			return parsed, err
		}
	}
	var req queryRequest
	// Bound resources before expanding parameters; encoding/json owns syntax.
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		return parsed, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return parsed, errors.New("one JSON object required")
	}
	if req.Connection != nil {
		return parsed, errors.New("connection must be supplied in Neon-Connection-String header")
	}
	parsed.batch = req.Queries != nil
	if parsed.batch && req.Query != "" || !parsed.batch && req.Query == "" {
		return parsed, errors.New("provide query or queries")
	}
	parsed.queries = req.Queries
	if !parsed.batch {
		parsed.queries = []query{{Query: req.Query, Params: req.Params, ArrayMode: req.ArrayMode, MaxRows: req.MaxRows}}
	}
	if len(parsed.queries) == 0 || len(parsed.queries) > 100 {
		return parsed, errors.New("expected 1 to 100 queries")
	}
	for _, q := range parsed.queries {
		if q.MaxRows != nil && *q.MaxRows <= 0 {
			return parsed, errors.New("maxRows must be positive")
		}
	}
	if parsed.options, err = optionsForRequest(r, req, parsed.batch); err != nil {
		return parsed, err
	}
	if parsed.streaming, err = responseMode(r, parsed.batch); err != nil {
		return parsed, err
	}
	if parsed.maxRows, err = maxRowsHeader(r); err != nil {
		return parsed, err
	}
	if parsed.streaming {
		limited := parsed.maxRows > 0
		for _, q := range parsed.queries {
			limited = limited || q.MaxRows != nil
		}
		if limited {
			return parsed, errors.New("maxRows is not supported with Barnacle-Response-Mode: streaming")
		}
	}
	for _, q := range parsed.queries {
		if strings.TrimSpace(q.Query) == "" {
			return parsed, errors.New("empty query")
		}
		if len(q.Params) > maxQueryParameters {
			return parsed, errRequestComplexity
		}
	}
	return parsed, nil
}

// This allocation-free guard counts lexical tokens without interpreting JSON.
// It runs before decoding, so even ignored fields and duplicate fields cannot
// bypass the allocation limits. Invalid JSON is rejected by encoding/json.
func limitJSONComplexity(body []byte) error {
	_, err := scanJSONComplexity(body)
	return err
}

func scanJSONComplexity(body []byte) (int, error) {
	depth, tokens := 0, 0
	primitive := false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			tokens++
			primitive = false
			for i++; i < len(body); {
				n := bytes.IndexAny(body[i:], "\"\\")
				if n < 0 {
					// No further tokens; encoding/json will reject the missing quote.
					i = len(body)
					break
				}
				i += n
				if body[i] == '"' {
					break
				}
				i += 2 // Skip an escape and its following byte.
			}
		case '{', '[':
			depth++
			tokens++
			primitive = false
			if depth > maxJSONDepth {
				return tokens, errRequestComplexity
			}
		case '}', ']':
			depth--
			primitive = false
		case ',', ':', ' ', '\t', '\r', '\n':
			primitive = false
		default:
			if !primitive {
				tokens++
				primitive = true
			}
		}
		if tokens > maxJSONTokens {
			return tokens, errRequestComplexity
		}
	}
	return tokens, nil
}
