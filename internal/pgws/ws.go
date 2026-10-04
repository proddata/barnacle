package pgws

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsWriter struct {
	conn         net.Conn
	mu           sync.Mutex
	writeTimeout time.Duration
	closing      atomic.Bool
	closeSent    bool
}

func (w *wsWriter) frame(op byte, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closeSent {
		if op == 8 {
			return nil
		}
		return net.ErrClosed
	}
	if w.closing.Load() && op != 8 {
		return net.ErrClosed
	}
	if timeout := w.shutdownWriteTimeout(); timeout > 0 {
		if err := w.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
	}
	head := []byte{0x80 | op, 0}
	switch {
	case len(data) < 126:
		head[1] = byte(len(data))
	case len(data) <= 65535:
		head[1] = 126
		head = binary.BigEndian.AppendUint16(head, uint16(len(data)))
	default:
		head[1] = 127
		head = binary.BigEndian.AppendUint64(head, uint64(len(data)))
	}
	if err := writeFull(w.conn, head); err != nil {
		return err
	}
	if err := writeFull(w.conn, data); err != nil {
		return err
	}
	if op == 8 {
		w.closeSent = true
	}
	return nil
}

func (c Handler) Serve(w http.ResponseWriter, r *http.Request) {
	if c.sessions.isDraining() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	if !c.OriginAllowed(r) {
		http.Error(w, "origin denied", http.StatusForbidden)
		return
	}
	if !c.AuthorizeWebSocket(w, r) {
		return
	}
	if r.Header.Get("Upgrade") != "websocket" || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") || r.Header.Get("Sec-Websocket-Version") != "13" {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Sec-Websocket-Key")
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 16 {
		http.Error(w, "invalid websocket key", http.StatusBadRequest)
		return
	}
	addresses := r.URL.Query()["address"]
	if len(c.PGAllowedAddrs) > 0 && len(addresses) > 1 {
		http.Error(w, "one PostgreSQL address expected", http.StatusBadRequest)
		return
	}
	requested := ""
	if len(addresses) == 1 {
		requested = addresses[0]
	}
	upstream, err := c.UpstreamAddr(requested)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !c.AcquireUpstream() {
		c.Metrics.RejectUpstream()
		http.Error(w, "postgres connection limit reached", http.StatusServiceUnavailable)
		return
	}
	defer c.ReleaseUpstream()
	backend, err := c.DialPostgres(upstream)
	if err != nil {
		c.Metrics.UpstreamFailure()
		http.Error(w, "postgres unavailable", http.StatusBadGateway)
		return
	}
	defer backend.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unavailable", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if c.WSWriteTimeout > 0 {
		if err := client.SetWriteDeadline(time.Now().Add(c.WSWriteTimeout)); err != nil {
			return
		}
	}
	accept := sha1.Sum([]byte(key + wsGUID))
	_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(accept[:]) + "\r\n\r\n")
	if err != nil {
		return
	}
	if err = rw.Flush(); err != nil {
		return
	}
	writer := &wsWriter{conn: client, writeTimeout: c.WSWriteTimeout}
	keyData := &backendKeyCapture{}
	session := &wsSession{client: client, backend: backend, writer: writer, cancel: func(ctx context.Context) { c.cancelForShutdown(ctx, upstream, keyData) }}
	if !c.sessions.add(session) {
		return
	}
	defer c.sessions.remove(session)
	if c.Metrics != nil {
		c.Metrics.AddWebSocket(1)
		defer c.Metrics.AddWebSocket(-1)
	}
	type relayEnd struct {
		cancel bool
	}
	done := make(chan relayEnd, 2)
	go func() {
		defer func() { done <- relayEnd{} }()
		buf := make([]byte, 32<<10)
		for {
			n, e := backend.Read(buf)
			if n > 0 {
				if c.WSIdleTimeout > 0 {
					_ = client.SetReadDeadline(time.Now().Add(c.WSIdleTimeout))
				}
				keyData.feed(buf[:n])
				if writer.frame(2, buf[:n]) != nil {
					// A slow or vanished client must not keep PostgreSQL busy.
					if !writer.closing.Load() {
						c.cancelDisconnectedSession(upstream, keyData)
					}
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	go func() {
		err := readWS(rw.Reader, &deadlineWriter{conn: backend, timeout: c.WSWriteTimeout}, writer, client, c.WSIdleTimeout)
		done <- relayEnd{cancel: err != nil}
	}()
	ended := <-done
	if ended.cancel && !writer.closing.Load() {
		c.cancelDisconnectedSession(upstream, keyData)
	}
	_ = client.Close()
	_ = backend.Close()
	<-done
}

// readWS streams masked client data into PostgreSQL without buffering whole messages.
func readWS(reader *bufio.Reader, backend io.Writer, writer *wsWriter, client net.Conn, idleTimeout time.Duration) error {
	var fragmented bool
	var buffer [32 << 10]byte
	for {
		if client != nil && idleTimeout > 0 {
			if err := client.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
				return err
			}
		}
		b1, err := reader.ReadByte()
		if err != nil {
			return err
		}
		b2, err := reader.ReadByte()
		if err != nil {
			return err
		}
		fin := b1&0x80 != 0
		op := b1 & 0x0f
		if b1&0x70 != 0 || b2&0x80 == 0 {
			return errors.New("invalid websocket frame")
		}
		n := uint64(b2 & 0x7f)
		if n == 126 {
			var v uint16
			if err = binary.Read(reader, binary.BigEndian, &v); err != nil {
				return err
			}
			n = uint64(v)
			if n < 126 {
				return errors.New("noncanonical frame length")
			}
		} else if n == 127 {
			if err = binary.Read(reader, binary.BigEndian, &n); err != nil {
				return err
			}
			if n <= 65535 || n>>63 != 0 {
				return errors.New("invalid frame length")
			}
		}
		if op >= 8 && (!fin || n > 125) {
			return errors.New("invalid control frame")
		}
		var mask [4]byte
		if _, err = io.ReadFull(reader, mask[:]); err != nil {
			return err
		}
		if op >= 8 {
			payload := make([]byte, int(n))
			if _, err = io.ReadFull(reader, payload); err != nil {
				return err
			}
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
			switch op {
			case 8:
				if len(payload) == 1 || len(payload) >= 2 && (!validCloseCode(binary.BigEndian.Uint16(payload[:2])) || !utf8.Valid(payload[2:])) {
					return errors.New("invalid close frame")
				}
				return writer.frame(8, payload)
			case 9:
				if err = writer.frame(10, payload); err != nil {
					return err
				}
			case 10:
			default:
				return errors.New("unknown control frame")
			}
			continue
		}
		switch op {
		case 2:
			if fragmented {
				return errors.New("unexpected binary frame")
			}
		case 0:
			if !fragmented {
				return errors.New("unexpected continuation")
			}
		default:
			return errors.New("expected binary frame")
		}
		offset := uint64(0)
		for n > 0 {
			take := int(min(n, uint64(len(buffer))))
			if _, err = io.ReadFull(reader, buffer[:take]); err != nil {
				return err
			}
			for i := 0; i < take; i++ {
				buffer[i] ^= mask[(offset+uint64(i))%4]
			}
			if err = writeFull(backend, buffer[:take]); err != nil {
				return err
			}
			offset += uint64(take)
			n -= uint64(take)
		}
		fragmented = !fin
	}
}

func validCloseCode(code uint16) bool {
	return code >= 1000 && code <= 4999 && code != 1004 && code != 1005 && code != 1006 && code != 1015
}

type deadlineWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	if w.timeout > 0 {
		if err := w.conn.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
			return 0, err
		}
	}
	return w.conn.Write(p)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
