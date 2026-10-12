package pgws

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// maxLoginMessageLen only detects a desynchronized stream; messages other than
// the nine-byte AuthenticationOk-sized 'R' are forwarded without buffering.
const maxLoginMessageLen = 1 << 20

// Login errors are short; a longer one is forwarded but not inspected.
const maxLoginErrorBody = 4 << 10

var (
	errUnauthenticatedLogin = errors.New("PostgreSQL accepted the login without requesting a credential")
	errMalformedLogin       = errors.New("unexpected data from PostgreSQL before login completed")
)

// loginGuard watches the backend's messages until the first ReadyForQuery and
// refuses an AuthenticationOk that no authentication request preceded, which is
// what a trust rule produces. The WebSocket relay otherwise cannot tell, so any
// pg_hba.conf rule that trusts the gateway's source address would log every
// client in without a password.
//
// Any non-zero Authentication request (cleartext, MD5, SASL including SCRAM and
// OAUTHBEARER, GSS) counts as a challenge. The AuthenticationOk message is held
// back until it has been checked, so a rejected login never reaches the client.
// A desynchronized stream also fails closed, since otherwise a client could
// confuse the framing (for example with an SSLRequest) to skip the check.
//
// It also notes whether PostgreSQL refused the login with an authentication
// error (SQLSTATE class 28), so failed logins over WebSocket can be counted.
type loginGuard struct {
	done        bool
	allowTrust  bool // forward a login PostgreSQL accepts without a credential
	challenged  bool
	loginFailed bool
	errorBody   []byte
	header      [5]byte
	headerN     int
	code        [4]byte
	remaining   uint32
	bodyN       int
	hold        bool
}

// filter returns the bytes to forward to the client. After an error, the bytes
// returned are still safe to forward, but the session must then be closed.
func (g *loginGuard) filter(data []byte) ([]byte, error) {
	if g.done {
		return data, nil
	}
	var out []byte
	for len(data) > 0 {
		if g.headerN < len(g.header) {
			n := copy(g.header[g.headerN:], data)
			g.headerN += n
			data = data[n:]
			if g.headerN < len(g.header) {
				break
			}
			length := binary.BigEndian.Uint32(g.header[1:])
			if length < 4 || length > maxLoginMessageLen {
				return out, errMalformedLogin
			}
			g.remaining = length - 4
			g.bodyN = 0
			g.code = [4]byte{}
			g.errorBody = g.errorBody[:0]
			// An Authentication message of exactly eight bytes may be
			// AuthenticationOk, so keep it until its type is known.
			g.hold = g.header[0] == 'R' && length == 8
			if !g.hold {
				out = append(out, g.header[:]...)
			}
		}
		n := min(len(data), int(g.remaining))
		body := data[:n]
		if g.header[0] == 'R' && g.bodyN < len(g.code) {
			copy(g.code[g.bodyN:], body)
		}
		if g.header[0] == 'E' && len(g.errorBody)+n <= maxLoginErrorBody {
			g.errorBody = append(g.errorBody, body...)
		}
		if !g.hold {
			out = append(out, body...)
		}
		g.bodyN += n
		g.remaining -= uint32(n)
		data = data[n:]
		if g.remaining > 0 {
			break
		}
		g.headerN = 0
		if g.header[0] == 'E' {
			if isAuthenticationError(g.errorBody) {
				g.loginFailed = true
			}
			g.errorBody = g.errorBody[:0]
		}
		switch g.header[0] {
		case 'R':
			if g.bodyN < len(g.code) {
				return out, errMalformedLogin
			}
			if binary.BigEndian.Uint32(g.code[:]) == 0 {
				if !g.challenged && !g.allowTrust {
					return out, errUnauthenticatedLogin
				}
			} else {
				g.challenged = true
			}
			if g.hold {
				out = append(out, g.header[:]...)
				out = append(out, g.code[:]...)
			}
		case 'Z':
			g.done = true
			return append(out, data...), nil
		}
	}
	return out, nil
}

// isAuthenticationError reports whether an ErrorResponse body carries a
// SQLSTATE in class 28 (invalid authorization specification, for example 28P01
// for a wrong password and 28000 for a rejected role or token).
func isAuthenticationError(body []byte) bool {
	for len(body) > 1 {
		field, rest := body[0], body[1:]
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return false
		}
		if field == 'C' {
			return bytes.HasPrefix(rest[:end], []byte("28"))
		}
		body = rest[end+1:]
	}
	return false
}
