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
func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = 10 << 20
	}
	if r.ContentLength > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

type parsedRequest struct {
	queries   []query
	batch     bool
	options   executionOptions
	streaming bool
}

// decodeRequest runs after the upload size check and PostgreSQL authentication.
// Its token guard bounds allocations before JSON expands parameter structures.
func decodeRequest(r *http.Request, body []byte) (parsedRequest, error) {
	var parsed parsedRequest
	if err := limitJSONComplexity(body); err != nil {
		return parsed, err
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
		parsed.queries = []query{{Query: req.Query, Params: req.Params, ArrayMode: req.ArrayMode}}
	}
	if len(parsed.queries) == 0 || len(parsed.queries) > 100 {
		return parsed, errors.New("expected 1 to 100 queries")
	}
	var err error
	if parsed.options, err = optionsForRequest(r, req, parsed.batch); err != nil {
		return parsed, err
	}
	if parsed.streaming, err = responseMode(r, parsed.batch); err != nil {
		return parsed, err
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
				return errRequestComplexity
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
			return errRequestComplexity
		}
	}
	return nil
}
