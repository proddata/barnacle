package pgws

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func pgMessage(kind byte, body ...byte) []byte {
	msg := []byte{kind, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(msg[1:], uint32(len(body)+4))
	return append(msg, body...)
}

func authRequest(code uint32, extra ...byte) []byte {
	return pgMessage('R', append(binary.BigEndian.AppendUint32(nil, code), extra...)...)
}

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

var (
	authOK        = authRequest(0)
	backendKey    = pgMessage('K', 0, 0, 0, 42, 1, 2, 3, 4)
	readyForQuery = pgMessage('Z', 'I')
	paramStatus   = pgMessage('S', []byte("server_version\x0017.0\x00")...)
	loginTail     = concat(paramStatus, backendKey, readyForQuery)

	cleartextLogin = concat(authRequest(3), authOK, loginTail)
	md5Login       = concat(authRequest(5, 1, 2, 3, 4), authOK, loginTail)
	scramLogin     = concat(
		authRequest(10, []byte("SCRAM-SHA-256\x00\x00")...),
		authRequest(11, []byte("r=nonce,s=salt,i=4096")...),
		authRequest(12, []byte("v=signature")...),
		authOK, loginTail)
	// PostgreSQL's OAuth validator may finish without a SASL final message.
	oauthLogin = concat(authRequest(10, []byte("OAUTHBEARER\x00\x00")...), authOK, loginTail)
	// A rejected token is reported through SASL continue, then an error.
	oauthRejected = concat(
		authRequest(10, []byte("OAUTHBEARER\x00\x00")...),
		authRequest(11, []byte(`{"status":"invalid_token"}`)...),
		pgMessage('E', []byte("SFATAL\x00C28000\x00Mbad token\x00\x00")...))
	trustLogin = concat(authOK, loginTail)
)

// feedInChunks runs the guard over wire, split every step bytes, and returns
// everything it chose to forward.
func feedInChunks(wire []byte, step int) ([]byte, error) {
	guard := &loginGuard{}
	var forwarded []byte
	for offset := 0; offset < len(wire); offset += step {
		out, err := guard.filter(wire[offset:min(offset+step, len(wire))])
		forwarded = append(forwarded, out...)
		if err != nil {
			return forwarded, err
		}
	}
	return forwarded, nil
}

func TestLoginGuardAllowsEveryAuthenticationMethod(t *testing.T) {
	for name, wire := range map[string][]byte{
		"cleartext": cleartextLogin, "md5": md5Login, "scram": scramLogin, "oauthbearer": oauthLogin,
		"rejected oauth token": oauthRejected,
	} {
		for _, step := range []int{1, 2, 3, 5, 7, 64, len(wire)} {
			forwarded, err := feedInChunks(wire, step)
			if err != nil {
				t.Errorf("%s step %d: %v", name, step, err)
			}
			if !bytes.Equal(forwarded, wire) {
				t.Errorf("%s step %d: forwarded %d of %d bytes, or altered them", name, step, len(forwarded), len(wire))
			}
		}
	}
}

func TestLoginGuardRejectsTrustWithoutForwardingAuthenticationOk(t *testing.T) {
	for _, step := range []int{1, 2, 4, 9, 64, len(trustLogin)} {
		forwarded, err := feedInChunks(trustLogin, step)
		if !errors.Is(err, errUnauthenticatedLogin) {
			t.Fatalf("step %d: error = %v", step, err)
		}
		if len(forwarded) != 0 {
			t.Fatalf("step %d: forwarded %x before rejecting", step, forwarded)
		}
	}
	// Messages ahead of the unauthenticated login still pass; the Ok does not.
	negotiate := pgMessage('v', 0, 0, 0, 0, 0, 0, 0, 0)
	forwarded, err := feedInChunks(concat(negotiate, trustLogin), 3)
	if !errors.Is(err, errUnauthenticatedLogin) || !bytes.Equal(forwarded, negotiate) {
		t.Fatalf("forwarded %x, error %v", forwarded, err)
	}
}

func TestLoginGuardFailsClosedOnDesynchronizedStream(t *testing.T) {
	// A client SSLRequest answered with a bare 'N' shifts message framing.
	for name, wire := range map[string][]byte{
		"ssl refusal byte": concat([]byte{'N'}, trustLogin),
		"short length":     {'R', 0, 0, 0, 3},
		"huge length":      {'E', 0xff, 0xff, 0xff, 0xff},
		"empty R":          concat(pgMessage('R'), authOK),
	} {
		if _, err := feedInChunks(wire, 2); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoginGuardPassesEverythingAfterReadyForQuery(t *testing.T) {
	guard := &loginGuard{}
	login := concat(authRequest(5, 1, 2, 3, 4), authOK, backendKey, readyForQuery)
	rows := pgMessage('D', bytes.Repeat([]byte{'x'}, 1000)...)
	// Data in the same read as ReadyForQuery, and non-protocol bytes later.
	out, err := guard.filter(concat(login, rows))
	if err != nil || !bytes.Equal(out, concat(login, rows)) {
		t.Fatalf("login chunk: %v", err)
	}
	later := []byte("not a PostgreSQL message at all")
	out, err = guard.filter(later)
	if err != nil || !bytes.Equal(out, later) {
		t.Fatalf("after login: %q, %v", out, err)
	}
	// An unauthenticated AuthenticationOk after login is ordinary data to the guard.
	out, err = guard.filter(authOK)
	if err != nil || !bytes.Equal(out, authOK) {
		t.Fatalf("post-login R: %v", err)
	}
}

func TestLoginGuardDisabledForwardsTrustLogin(t *testing.T) {
	guard := &loginGuard{done: true}
	out, err := guard.filter(trustLogin)
	if err != nil || !bytes.Equal(out, trustLogin) {
		t.Fatalf("disabled guard: %v", err)
	}
}

func FuzzLoginGuardChunking(f *testing.F) {
	for _, wire := range [][]byte{cleartextLogin, md5Login, scramLogin, oauthLogin, oauthRejected, trustLogin} {
		f.Add(wire, uint8(1))
		f.Add(wire, uint8(13))
	}
	f.Add([]byte{'N', 'R', 0, 0, 0}, uint8(1))
	f.Fuzz(func(t *testing.T, wire []byte, chunkSize uint8) {
		if len(wire) > 64<<10 {
			t.Skip()
		}
		wholeOut, wholeErr := feedInChunks(wire, max(len(wire), 1))
		chunkedOut, chunkedErr := feedInChunks(wire, int(chunkSize%32)+1)
		if (wholeErr == nil) != (chunkedErr == nil) || !bytes.Equal(wholeOut, chunkedOut) {
			t.Fatalf("chunking changed the result: whole (%d bytes, %v) vs chunked (%d bytes, %v)",
				len(wholeOut), wholeErr, len(chunkedOut), chunkedErr)
		}
		// The guard only ever withholds the tail of the stream, never rewrites it.
		if !bytes.HasPrefix(wire, wholeOut) || !bytes.HasPrefix(wire, chunkedOut) {
			t.Fatal("forwarded bytes are not a prefix of the backend stream")
		}
	})
}
