package sqlhttp

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proddata/barnacle/internal/gateway"
)

const pgRowMessageOverhead = 16 << 10 // PostgreSQL allows at most 1,600 columns.

type query struct {
	Query     string `json:"query"`
	Params    []any  `json:"params"`
	ArrayMode *bool  `json:"arrayMode"`
}
type queryOptions struct {
	ArrayMode      *bool   `json:"arrayMode"`
	RawText        *bool   `json:"rawText"`
	ReadOnly       *bool   `json:"readOnly"`
	IsolationLevel *string `json:"isolationLevel"`
	Deferrable     *bool   `json:"deferrable"`
}
type queryRequest struct {
	Query      string          `json:"query"`
	Params     []any           `json:"params"`
	ArrayMode  *bool           `json:"arrayMode"`
	Queries    []query         `json:"queries"`
	Options    *queryOptions   `json:"options"`
	Connection json.RawMessage `json:"connection"`
}
type executionOptions struct {
	arrayMode bool
	rawText   bool
	tx        pgx.TxOptions
}

func optionsForRequest(r *http.Request, req queryRequest, batch bool) (executionOptions, error) {
	var body queryOptions
	if req.Options != nil {
		body = *req.Options
	}
	var options executionOptions
	var err error
	if options.arrayMode, err = optionBool(r, "Array-Mode", body.ArrayMode); err != nil {
		return options, err
	}
	if options.rawText, err = optionBool(r, "Raw-Text-Output", body.RawText); err != nil {
		return options, err
	}
	if batch {
		readOnly, err := optionBool(r, "Batch-Read-Only", body.ReadOnly)
		if err != nil {
			return options, err
		}
		isolationLevel, err := optionString(r, "Batch-Isolation-Level", body.IsolationLevel)
		if err != nil {
			return options, err
		}
		deferrable, err := optionBool(r, "Batch-Deferrable", body.Deferrable)
		if err != nil {
			return options, err
		}
		if readOnly {
			options.tx.AccessMode = pgx.ReadOnly
		}
		options.tx.IsoLevel, err = batchIsolation(isolationLevel)
		if err != nil {
			return options, err
		}
		if deferrable {
			options.tx.DeferrableMode = pgx.Deferrable
		}
	}
	return options, nil
}

func optionBool(r *http.Request, header string, body *bool) (bool, error) {
	value, err := optionString(r, header, nil)
	if err != nil {
		return false, err
	}
	if body != nil {
		if len(r.Header.Values(header))+len(r.Header.Values("Neon-"+header)) > 0 {
			return false, fmt.Errorf("use either options or %s", header)
		}
		return *body, nil
	}
	if len(r.Header.Values(header))+len(r.Header.Values("Neon-"+header)) == 0 {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", header)
	}
}

func optionString(r *http.Request, header string, body *string) (string, error) {
	values := append(r.Header.Values(header), r.Header.Values("Neon-"+header)...)
	if len(values) > 1 || body != nil && len(values) > 0 {
		return "", fmt.Errorf("use one value for %s or Neon-%s", header, header)
	}
	if body != nil {
		return *body, nil
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return "", nil
}

type field struct {
	Name             string `json:"name"`
	TableID          uint32 `json:"tableID"`
	ColumnID         uint16 `json:"columnID"`
	DataTypeID       uint32 `json:"dataTypeID"`
	DataTypeSize     int16  `json:"dataTypeSize"`
	DataTypeModifier int32  `json:"dataTypeModifier"`
	Format           string `json:"format"`
}
type result struct {
	Rows       any     `json:"rows"`
	Fields     []field `json:"fields"`
	Command    string  `json:"command"`
	RowCount   *int64  `json:"rowCount"`
	RowAsArray bool    `json:"rowAsArray"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeLimitedJSON(w http.ResponseWriter, status int, value any, maxBytes int64) {
	encoded, err := encodeLimitedJSON(value, maxBytes)
	if err != nil {
		if errors.Is(err, errHTTPResultTooLarge) {
			apiError(w, http.StatusRequestEntityTooLarge, err)
		} else {
			apiError(w, http.StatusInternalServerError, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
func encodeLimitedJSON(value any, maxBytes int64) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(encoded)) > maxBytes {
		return nil, errHTTPResultTooLarge
	}
	return encoded, nil
}
func apiError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"message": err.Error(), "code": "BARNACLE_ERROR"})
}

func (c Handler) connectionConfig(r *http.Request) (*pgx.ConnConfig, error) {
	connectionHeaders := r.Header.Values("Connection-String")
	neonHeaders := r.Header.Values("Neon-Connection-String")
	if len(connectionHeaders)+len(neonHeaders) > 1 {
		return nil, errors.New("use one connection header: Connection-String or Neon-Connection-String")
	}
	raw := ""
	if len(connectionHeaders) == 1 {
		raw = connectionHeaders[0]
	} else if len(neonHeaders) == 1 {
		raw = neonHeaders[0]
	}
	authorization := r.Header.Get("Authorization")
	if raw == "" {
		return nil, errors.New("Connection-String or Neon-Connection-String required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Opaque != "" || u.Fragment != "" {
		return nil, errors.New("invalid PostgreSQL connection string")
	}
	authScheme, _, hasAuthScheme := strings.Cut(authorization, " ")
	if authorization != "" && !hasAuthScheme {
		return nil, errors.New("expected Authorization: Basic <credentials> or Bearer <token>")
	}
	if hasAuthScheme && strings.EqualFold(authScheme, "Basic") {
		user, password, ok := r.BasicAuth()
		if !ok || user == "" || password == "" {
			return nil, errors.New("expected Authorization: Basic <credentials>")
		}
		if u.User != nil {
			return nil, errors.New("provide PostgreSQL credentials in either Authorization or connection")
		}
		u.User = url.UserPassword(user, password)
		raw = u.String()
	}
	var suppliedUser, suppliedPassword string
	if u.User != nil {
		suppliedUser = u.User.Username()
		suppliedPassword, _ = u.User.Password()
	}
	if suppliedUser == "" || u.Path == "" || u.Path == "/" {
		return nil, errors.New("database and user required")
	}
	for key, values := range u.Query() {
		switch key {
		case "sslmode", "channel_binding", "application_name":
		case "require_auth":
			if len(values) != 1 || values[0] != "scram-sha-256" && values[0] != "md5" {
				return nil, errors.New("unsupported PostgreSQL authentication method")
			}
		default:
			return nil, errors.New("unsupported PostgreSQL connection option")
		}
	}
	if authorization == "" && suppliedPassword == "" {
		return nil, errors.New("PostgreSQL password or Authorization required")
	}
	upstream, err := c.HTTPUpstreamAddr(raw)
	if err != nil {
		return nil, err
	}
	pgcfg, err := pgx.ParseConfigWithOptions(raw, pgx.ParseConfigOptions{ParseConfigOptions: pgconn.ParseConfigOptions{
		ConnStringAllowedKeys: []string{"host", "port", "database", "user", "password", "sslmode", "channel_binding", "application_name", "require_auth"},
	}})
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection string")
	}
	if suppliedUser != "" && pgcfg.User != suppliedUser || suppliedPassword != "" && pgcfg.Password != suppliedPassword {
		return nil, errors.New("invalid PostgreSQL connection string")
	}
	host, portString, err := net.SplitHostPort(upstream)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portString, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("invalid postgres port")
	}
	pgcfg.Host = host
	pgcfg.Port = uint16(port)
	pgcfg.Fallbacks = nil
	switch c.PGQueryExecMode {
	case "", "exec":
		pgcfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	case "cache_describe":
		pgcfg.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	case "cache_statement":
		pgcfg.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	default:
		return nil, errors.New("invalid PostgreSQL query execution mode")
	}
	if c.MaxHTTPRowBytes > 0 {
		pgcfg.MaxProtocolMessageBodyLen = int(c.MaxHTTPRowBytes) + pgRowMessageOverhead
	}
	if c.PGSSLMode == "disable" {
		pgcfg.TLSConfig = nil
	} else if c.PGSSLMode == "require" {
		pgcfg.TLSConfig = c.PGTLSConfig(host)
	} else {
		return nil, errors.New("BARNACLE_PG_SSLMODE must be disable or require")
	}
	if authorization != "" && !strings.EqualFold(authScheme, "Basic") {
		scheme, token, ok := strings.Cut(authorization, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return nil, errors.New("expected Authorization: Bearer <token>")
		}
		bearer := strings.TrimSpace(token)
		pgcfg.OAuthTokenProvider = func(context.Context) (string, error) {
			return bearer, nil
		}
		if c.OIDC != nil {
			// A verified access token must only be sent with PostgreSQL OAuth.
			// Reject SCRAM/password/unauthenticated server configurations.
			pgcfg.Password = ""
			pgcfg.RequireAuth = "oauth"
		} else {
			// Preserve password-shaped bearer tokens for legacy setups.
			pgcfg.Password = bearer
		}
	}
	if pgcfg.User == "" || pgcfg.Database == "" {
		return nil, errors.New("database and user required")
	}
	return pgcfg, nil
}

func (c Handler) Serve(w http.ResponseWriter, r *http.Request) {
	if !c.AllowOrigin(w, r) {
		return
	}
	if !c.AuthorizeHTTP(w, r) {
		return
	}
	var req queryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		requestDecodeError(w, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			requestDecodeError(w, err)
		} else {
			apiError(w, 400, errors.New("one JSON object required"))
		}
		return
	}
	if req.Connection != nil {
		apiError(w, 400, errors.New("connection must be supplied in Connection-String or Neon-Connection-String header"))
		return
	}
	batch := req.Queries != nil
	if batch && req.Query != "" || !batch && req.Query == "" {
		apiError(w, 400, errors.New("provide query or queries"))
		return
	}
	queries := req.Queries
	if !batch {
		queries = []query{{Query: req.Query, Params: req.Params, ArrayMode: req.ArrayMode}}
	}
	if len(queries) == 0 || len(queries) > 100 {
		apiError(w, 400, errors.New("expected 1 to 100 queries"))
		return
	}
	requestOptions, err := optionsForRequest(r, req, batch)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	if !c.AcquireHTTP() {
		c.Metrics.RejectHTTPQuery()
		apiError(w, http.StatusServiceUnavailable, errors.New("HTTP query limit reached"))
		return
	}
	defer c.ReleaseHTTP()
	for _, q := range queries {
		if strings.TrimSpace(q.Query) == "" {
			apiError(w, 400, errors.New("empty query"))
			return
		}
	}
	pgcfg, err := c.connectionConfig(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	if !c.AcquireUpstream() {
		c.Metrics.RejectUpstream()
		apiError(w, http.StatusServiceUnavailable, errors.New("postgres connection limit reached"))
		return
	}
	defer c.ReleaseUpstream()
	ctx, cancel := context.WithTimeout(r.Context(), c.QueryTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, pgcfg)
	if err != nil {
		c.Metrics.UpstreamFailure()
		dbConnectError(w, err, pgcfg.RequireAuth == "oauth" && pgcfg.OAuthTokenProvider != nil)
		return
	}
	defer conn.Close(context.Background())
	arrayMode := requestOptions.arrayMode
	rawText := requestOptions.rawText
	rowLimit := c.MaxHTTPRowBytes
	if batch {
		rowLimit = min(rowLimit, c.MaxHTTPBufferedBytes)
	}
	conn.PgConn().Frontend().SetMaxBodyLen(int(rowLimit) + pgRowMessageOverhead)
	results := make([]result, 0, len(queries))
	bufferedRemaining := c.MaxHTTPBufferedBytes
	var runner interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
	} = conn
	var tx pgx.Tx
	if batch {
		tx, err = conn.BeginTx(ctx, requestOptions.tx)
		if err != nil {
			dbError(w, err)
			return
		}
		defer tx.Rollback(context.Background())
		runner = tx
	}
	for _, q := range queries {
		queryArrayMode := arrayMode
		if q.ArrayMode != nil {
			queryArrayMode = *q.ArrayMode
		}
		args := make([]any, len(q.Params))
		for i, v := range q.Params {
			args[i], err = pgParameter(v)
			if err != nil {
				apiError(w, 400, err)
				return
			}
		}
		args = append([]any{pgx.QueryResultFormats{pgx.TextFormatCode}}, args...)
		rows, err := runner.Query(ctx, q.Query, args...)
		if err != nil {
			dbError(w, err)
			return
		}
		if !batch && canStream(rows, conn.TypeMap(), rawText) {
			if err := streamSingle(w, rows, conn.TypeMap(), queryArrayMode, rawText, c.MaxHTTPRowBytes, c.MaxHTTPResponseBytes); err != nil {
				kind := interruptedResultKind(err)
				c.Metrics.InterruptHTTPStream(kind)
				slog.Warn("HTTP result stream interrupted", "kind", kind)
			}
			return
		}
		conn.PgConn().Frontend().SetMaxBodyLen(int(min(c.MaxHTTPBufferedBytes, bufferedRemaining)) + pgRowMessageOverhead)
		item, err := collect(ctx, rows, conn.TypeMap(), queryArrayMode, rawText, &bufferedRemaining)
		if err != nil {
			if errors.Is(err, errHTTPResultTooLarge) {
				apiError(w, http.StatusRequestEntityTooLarge, err)
			} else {
				dbError(w, err)
			}
			return
		}
		results = append(results, item)
	}
	var encodedBatch []byte
	if batch {
		encodedBatch, err = encodeLimitedJSON(map[string]any{"results": results}, c.MaxHTTPResponseBytes)
		if err != nil {
			if errors.Is(err, errHTTPResultTooLarge) {
				apiError(w, http.StatusRequestEntityTooLarge, err)
			} else {
				apiError(w, http.StatusInternalServerError, err)
			}
			return
		}
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			dbError(w, err)
			return
		}
	}
	if batch {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodedBatch)
	} else {
		writeLimitedJSON(w, 200, results[0], c.MaxHTTPResponseBytes)
	}
}

func requestDecodeError(w http.ResponseWriter, err error) {
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		apiError(w, http.StatusRequestEntityTooLarge, errors.New("request is too large"))
		return
	}
	apiError(w, http.StatusBadRequest, err)
}

func collect(ctx context.Context, rows pgx.Rows, types *pgtype.Map, arrayMode, rawText bool, remaining *int64) (result, error) {
	defer rows.Close()
	descriptions := rows.FieldDescriptions()
	fields := fieldsFromDescriptions(descriptions)
	data := make([]any, 0)
	unknownArrays := make(map[uint32]map[int]struct{})
	for {
		if remaining != nil {
			rows.Conn().PgConn().Frontend().SetMaxBodyLen(int(*remaining) + pgRowMessageOverhead)
		}
		if !rows.Next() {
			break
		}
		values := rows.RawValues()
		// Rows and column containers consume memory even when every field is empty.
		cost := int64(64 + 64*len(values))
		for _, value := range values {
			cost += int64(len(value))
		}
		if remaining != nil && cost > *remaining {
			return result{}, errHTTPResultTooLarge
		}
		if remaining != nil {
			*remaining -= cost
		}
		out := make([]any, len(values))
		for i, v := range values {
			if v == nil {
				out[i] = nil
				continue
			}
			if rawText {
				out[i] = string(v)
			} else {
				converted, err := pgTextValue(string(v), descriptions[i].DataTypeOID, types)
				if err != nil {
					return result{}, err
				}
				out[i] = converted
				if _, known := types.TypeForOID(descriptions[i].DataTypeOID); !known && len(v) > 0 && (v[0] == '{' || v[0] == '[') {
					if unknownArrays[descriptions[i].DataTypeOID] == nil {
						unknownArrays[descriptions[i].DataTypeOID] = make(map[int]struct{})
					}
					unknownArrays[descriptions[i].DataTypeOID][i] = struct{}{}
				}
			}
		}
		if arrayMode {
			data = append(data, out)
		} else {
			object := make(map[string]any, len(out))
			for i, v := range out {
				object[fields[i].Name] = v
			}
			data = append(data, object)
		}
	}
	if err := rows.Err(); err != nil {
		return result{}, err
	}
	tag := rows.CommandTag()
	rows.Close()
	if len(unknownArrays) > 0 {
		for oid, indexes := range unknownArrays {
			var element uint32
			var delimiter string
			if err := rows.Conn().QueryRow(ctx, "select typelem, typdelim::text from pg_type where oid=$1", oid).Scan(&element, &delimiter); err != nil {
				return result{}, err
			}
			if element == 0 || delimiter == "" {
				continue
			}
			for _, row := range data {
				for index := range indexes {
					var value any
					if arrayMode {
						value = row.([]any)[index]
					} else {
						value = row.(map[string]any)[fields[index].Name]
					}
					text, ok := value.(string)
					if !ok || text == "" {
						continue
					}
					converted, err := parsePGArray(text, element, delimiter[0], types)
					if err != nil {
						return result{}, err
					}
					if arrayMode {
						row.([]any)[index] = converted
					} else {
						row.(map[string]any)[fields[index].Name] = converted
					}
				}
			}
		}
	}
	command, _, _ := strings.Cut(tag.String(), " ")
	return result{Rows: data, Fields: fields, Command: command, RowCount: commandRowCount(tag.String()), RowAsArray: arrayMode}, nil
}

func batchIsolation(value string) (pgx.TxIsoLevel, error) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), " ", "")) {
	case "", "readcommitted":
		return pgx.ReadCommitted, nil
	case "readuncommitted":
		return pgx.ReadUncommitted, nil
	case "repeatableread":
		return pgx.RepeatableRead, nil
	case "serializable":
		return pgx.Serializable, nil
	default:
		return "", errors.New("invalid isolation level")
	}
}

func commandRowCount(tag string) *int64 {
	parts := strings.Fields(tag)
	index := 1
	if len(parts) > 0 && parts[0] == "INSERT" {
		index = 2
	}
	if len(parts) <= index {
		return nil
	}
	count, err := strconv.ParseInt(parts[index], 10, 64)
	if err != nil {
		return nil
	}
	return &count
}

func fieldsFromDescriptions(descriptions []pgconn.FieldDescription) []field {
	fields := make([]field, len(descriptions))
	for i, d := range descriptions {
		fields[i] = field{Name: d.Name, TableID: d.TableOID, ColumnID: d.TableAttributeNumber, DataTypeID: d.DataTypeOID, DataTypeSize: d.DataTypeSize, DataTypeModifier: d.TypeModifier, Format: "text"}
	}
	return fields
}

func dbConnectError(w http.ResponseWriter, err error, requireOAuth bool) {
	// pgx rejects a password, SCRAM, or trust-only server before sending any
	// credentials when require_auth=oauth. Distinguish that upstream capability
	// mismatch from an invalid bearer token or a transient connection failure.
	var connectErr *pgconn.ConnectError
	var pgerr *pgconn.PgError
	if requireOAuth && errors.As(err, &connectErr) && !errors.As(err, &pgerr) &&
		strings.Contains(connectErr.Error(), `authentication method requirement "oauth" failed:`) {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"message": "upstream PostgreSQL does not offer OAuth authentication for this connection",
			"code":    "BARNACLE_UPSTREAM_OAUTH_UNAVAILABLE",
		})
		return
	}
	dbError(w, err)
}

func dbError(w http.ResponseWriter, err error) {
	var oversized *pgproto3.ExceededMaxBodyLenErr
	if errors.As(err, &oversized) {
		apiError(w, http.StatusRequestEntityTooLarge, errHTTPResultTooLarge)
		return
	}
	var unknownCA x509.UnknownAuthorityError
	var wrongHost x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &unknownCA) || errors.As(err, &wrongHost) || errors.As(err, &invalidCert) {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"message": "PostgreSQL TLS certificate verification failed; check the CA certificate and host name",
			"code":    "BARNACLE_UPSTREAM_TLS_VERIFICATION_FAILED",
		})
		return
	}
	// pgx 5.11 reports a rejected OAUTHBEARER token as a wrapped SASL error,
	// not a PgError. Keep the validator's details out of the HTTP response.
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) && strings.Contains(connectErr.Error(), "OAuth authentication failed:") {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		apiError(w, http.StatusUnauthorized, gateway.ErrInvalidToken)
		return
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		var position, internalPosition, line any
		if pgerr.Position > 0 {
			position = strconv.FormatInt(int64(pgerr.Position), 10)
		}
		if pgerr.InternalPosition > 0 {
			internalPosition = strconv.FormatInt(int64(pgerr.InternalPosition), 10)
		}
		if pgerr.Line > 0 {
			line = strconv.FormatInt(int64(pgerr.Line), 10)
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"message": pgerr.Message, "code": pgerr.Code, "severity": pgerr.Severity,
			"detail": nullableString(pgerr.Detail), "hint": nullableString(pgerr.Hint),
			"position": position, "internalPosition": internalPosition,
			"internalQuery": nullableString(pgerr.InternalQuery), "where": nullableString(pgerr.Where),
			"schema": nullableString(pgerr.SchemaName), "table": nullableString(pgerr.TableName),
			"column": nullableString(pgerr.ColumnName), "dataType": nullableString(pgerr.DataTypeName),
			"constraint": nullableString(pgerr.ConstraintName), "file": nullableString(pgerr.File),
			"line": line, "routine": nullableString(pgerr.Routine),
		})
		return
	}
	apiError(w, http.StatusBadGateway, errors.New("postgres connection or query failed"))
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func interruptedResultKind(err error) string {
	switch {
	case errors.Is(err, errHTTPResultTooLarge):
		return "result_limit"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "query_or_transport"
	}
}
