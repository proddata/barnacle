package pgws

import (
	"encoding/binary"
	"errors"
)

// maxLoginMessageLen only detects a desynchronized stream; messages other than
// the nine-byte AuthenticationOk-sized 'R' are forwarded without buffering.
const maxLoginMessageLen = 1 << 20

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
type loginGuard struct {
	done       bool
	challenged bool
	header     [5]byte
	headerN    int
	code       [4]byte
	remaining  uint32
	bodyN      int
	hold       bool
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
		switch g.header[0] {
		case 'R':
			if g.bodyN < len(g.code) {
				return out, errMalformedLogin
			}
			if binary.BigEndian.Uint32(g.code[:]) == 0 {
				if !g.challenged {
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
