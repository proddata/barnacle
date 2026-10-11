package sqlhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/proddata/barnacle/internal/gateway"
)

func TestJSONResourceLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limited    bool
	}{
		{"depth boundary", strings.Repeat("[", maxJSONDepth) + "0" + strings.Repeat("]", maxJSONDepth), false},
		{"depth exceeded", strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1), true},
		{"token boundary", "[" + strings.Repeat("0,", maxJSONTokens-2) + "0]", false},
		{"tokens exceeded", "[" + strings.Repeat("0,", maxJSONTokens-1) + "0]", true},
		{"ignored fields", `{"query":"select 1","ignored":[` + strings.Repeat(`{"x":0},`, 6000) + `{}]}`, true},
		{"duplicate fields", `{"query":"select 1",` + strings.Repeat(`"ignored":0,`, 9000) + `"ignored":0}`, true},
		{"string punctuation", `{"query":"` + strings.Repeat(`[]{}\"\\`, 20000) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.body)) {
				t.Fatal("invalid test JSON")
			}
			err := limitJSONComplexity([]byte(tc.body))
			if errors.Is(err, errRequestComplexity) != tc.limited {
				t.Fatalf("limit error = %v", err)
			}
		})
	}
	req := httptest.NewRequest("POST", "/sql", nil)
	for _, count := range []int{maxQueryParameters, maxQueryParameters + 1} {
		body := []byte(`{"query":"select 1","params":[` + strings.Repeat("0,", count-1) + `0]}`)
		_, err := decodeRequest(req, body)
		if errors.Is(err, errRequestComplexity) != (count > maxQueryParameters) {
			t.Fatalf("%d parameters: %v", count, err)
		}
	}
	for _, body := range []string{`{"query":"select 1"} {}`, `{"query":"select 1","params":[01]}`, `{"query":"select 1","params":["unterminated]}`, `null`, `[]`} {
		if _, err := decodeRequest(req, []byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestParametersPreserveConversion(t *testing.T) {
	body := []byte(`{"query":"select 1","params":[null,true,false,123456789012345678901234567890,1.234567890123456789e-100,"escaped\n\"\\",{"z":9007199254740993,"a":[1,null]},[["a,b","a\"b"],[null,"a\\b"]],[]]}`)
	parsed, err := decodeRequest(httptest.NewRequest("POST", "/sql", nil), body)
	if err != nil {
		t.Fatal(err)
	}
	var old struct{ Params []any }
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&old); err != nil {
		t.Fatal(err)
	}
	for i, value := range parsed.queries[0].Params {
		got, err := pgParameter(value)
		if err != nil {
			t.Fatal(err)
		}
		want, err := pgParameter(old.Params[i])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("parameter %d = %#v, want %#v", i, got, want)
		}
	}
}

func TestCredentialSizeLimits(t *testing.T) {
	handler := New(&gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable"})
	for _, field := range []string{"user", "password", "database", "bearer", "url", "basic password", "basic user"} {
		for _, excess := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", field, excess), func(t *testing.T) {
				req := httptest.NewRequest("POST", "/sql", nil)
				user, password, database := "app", "secret", "appdb"
				switch field {
				case "user":
					user = strings.Repeat("u", maxUsernameBytes+excess)
				case "password":
					password = strings.Repeat("@", maxPasswordBytes+excess)
				case "database":
					database = strings.Repeat("d", maxDatabaseBytes+excess)
				case "bearer":
					req.Header.Set("Authorization", "Bearer "+strings.Repeat("t", maxBearerTokenBytes+excess))
					password = ""
				case "basic password":
					password = ""
					req.SetBasicAuth("app", strings.Repeat("p", maxPasswordBytes+excess))
				case "basic user":
					password = ""
					req.SetBasicAuth(strings.Repeat("u", maxUsernameBytes+excess), "secret")
				}
				u := &url.URL{Scheme: "postgres", Host: "remote.example", Path: "/" + database, User: url.UserPassword(user, password)}
				if strings.HasPrefix(field, "basic") {
					u.User = nil
				}
				raw := u.String()
				if field == "url" {
					raw += "?application_name="
					raw += strings.Repeat("a", maxConnectionStringBytes+excess-len(raw))
				}
				req.Header.Set("Neon-Connection-String", raw)
				_, err := handler.connectionConfig(req)
				if excess == 0 && err != nil || excess == 1 && !errors.Is(err, errCredentialSize) {
					t.Fatalf("credential limit = %v", err)
				}
			})
		}
	}
	req := httptest.NewRequest("POST", "/sql", nil)
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	req.Header.Add("Authorization", "Basic YTpi")
	req.Header.Add("Authorization", "Bearer other")
	if _, err := handler.connectionConfig(req); err == nil {
		t.Fatal("accepted duplicate authorization")
	}
	req.Body = unreadableBody{}
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("s", maxBearerTokenBytes+1))
	response := httptest.NewRecorder()
	handler.HTTPSlots = make(chan struct{}, 1)
	handler.Serve(response, req)
	if response.Code != 431 || strings.Contains(response.Body.String(), "ssss") {
		t.Fatalf("credential response = %d %s", response.Code, response.Body.String())
	}
}

func TestBodyReadLimits(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, size := range []int{32, 33} {
			req := httptest.NewRequest("POST", "/sql", strings.NewReader(strings.Repeat("x", size)))
			if chunked {
				req.ContentLength = -1
			}
			body, err := readRequestBody(httptest.NewRecorder(), req, 32)
			if size == 32 && (err != nil || len(body) != 32) || size == 33 && err == nil {
				t.Fatalf("chunked=%t size=%d: %d bytes, %v", chunked, size, len(body), err)
			}
		}
	}
}

// Successful uploads authenticate before any JSON decoding; no query should be
// sent when the body is malformed or exceeds a complexity limit.
func TestAuthenticationBeforeRequestDecode(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		for _, body := range []string{`{"query":`, `{"query":"select 1","ignored":[` + strings.Repeat(`{"x":0},`, 6000) + `{}]}`} {
			t.Run(fmt.Sprintf("authenticated=%t/bytes=%d", authenticated, len(body)), func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan error, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					backend := pgproto3.NewBackend(conn, conn)
					if _, err := backend.ReceiveStartupMessage(); err != nil {
						done <- err
						return
					}
					backend.Send(&pgproto3.AuthenticationCleartextPassword{})
					if err := backend.Flush(); err != nil {
						done <- err
						return
					}
					if _, err := backend.Receive(); err != nil {
						done <- err
						return
					}
					if !authenticated {
						backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"})
						done <- backend.Flush()
						return
					}
					backend.Send(&pgproto3.AuthenticationOk{})
					backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 2}})
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
					if err := backend.Flush(); err != nil {
						done <- err
						return
					}
					msg, err := backend.Receive()
					if err == io.EOF {
						err = nil
					} else if _, ok := msg.(*pgproto3.Terminate); !ok && err == nil {
						err = fmt.Errorf("unexpected query message %T", msg)
					}
					done <- err
				}()
				handler := New(&gateway.Config{PGAddr: listener.Addr().String(), PGSSLMode: "disable", QueryTimeout: time.Second, HTTPSlots: make(chan struct{}, 1), UpstreamSlots: make(chan struct{}, 1)})
				req := httptest.NewRequest("POST", "/sql", strings.NewReader(body))
				req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
				response := httptest.NewRecorder()
				handler.Serve(response, req)
				if !authenticated {
					if !strings.Contains(response.Body.String(), `"code":"28P01"`) {
						t.Fatalf("decoded unauthenticated body: %d %s", response.Code, response.Body.String())
					}
				} else {
					want := 400
					if len(body) > 100 {
						want = 413
					}
					if response.Code != want {
						t.Fatalf("authenticated body: %d %s", response.Code, response.Body.String())
					}
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if len(handler.HTTPSlots) != 0 || len(handler.UpstreamSlots) != 0 {
					t.Fatal("admission slot leaked")
				}
			})
		}
	}
}

func TestUploadDoesNotAcquireUpstream(t *testing.T) {
	handler := New(&gateway.Config{PGAddr: "127.0.0.1:5432", PGSSLMode: "disable", QueryTimeout: time.Second, HTTPSlots: make(chan struct{}, 1), UpstreamSlots: make(chan struct{}, 1)})
	reader, writer := io.Pipe()
	defer reader.Close()
	req := httptest.NewRequest("POST", "/sql", reader)
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.Serve(response, req); close(done) }()
	if _, err := writer.Write([]byte(`{"query":`)); err != nil {
		t.Fatal(err)
	}
	if len(handler.HTTPSlots) != 1 || len(handler.UpstreamSlots) != 0 {
		t.Fatal("upload held an upstream connection")
	}
	writer.CloseWithError(errors.New("interrupted upload"))
	<-done
	if response.Code != 400 || len(handler.HTTPSlots) != 0 || len(handler.UpstreamSlots) != 0 {
		t.Fatalf("incomplete upload: %d", response.Code)
	}
}

func FuzzJSONComplexity(f *testing.F) {
	for _, seed := range []string{`{"query":"select 1","params":[null,123,true,{"x":[1,2]}]}`, `["\\\"{}[]",1e-20]`, strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33)} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 65536 || !json.Valid(body) {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		depth, tokens := 0, 0
		exceeded := false
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if delimiter, ok := token.(json.Delim); ok {
				if delimiter == ']' || delimiter == '}' {
					depth--
					continue
				}
				depth++
			}
			tokens++
			if depth > maxJSONDepth || tokens > maxJSONTokens {
				exceeded = true
			}
		}
		if got := errors.Is(limitJSONComplexity(body), errRequestComplexity); got != exceeded {
			t.Fatalf("limit=%t expected=%t depth/tokens=%d/%d for %q", got, exceeded, depth, tokens, body)
		}
	})
}

func TestAuthenticationDeadlineReleasesSlots(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(httpConnectTimeout + 3*time.Second))
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			done <- err
			return
		}
		// Deliberately stall authentication until the gateway closes the connection.
		_, err = io.Copy(io.Discard, conn)
		done <- err
	}()
	handler := New(&gateway.Config{PGAddr: listener.Addr().String(), PGSSLMode: "disable", QueryTimeout: time.Minute, HTTPSlots: make(chan struct{}, 1), UpstreamSlots: make(chan struct{}, 1)})
	req := httptest.NewRequest("POST", "/sql", strings.NewReader(`{"query":"select 1"}`))
	req.Header.Set("Neon-Connection-String", "postgres://app:secret@remote.example/appdb")
	response := httptest.NewRecorder()
	start := time.Now()
	handler.Serve(response, req)
	if elapsed := time.Since(start); elapsed < httpConnectTimeout-time.Second || elapsed > httpConnectTimeout+2*time.Second {
		t.Fatalf("authentication deadline took %s", elapsed)
	}
	if response.Code != 502 || len(handler.HTTPSlots) != 0 || len(handler.UpstreamSlots) != 0 {
		t.Fatalf("stalled authentication: %d, HTTP/upstream slots=%d/%d", response.Code, len(handler.HTTPSlots), len(handler.UpstreamSlots))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
