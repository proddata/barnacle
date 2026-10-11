package pgws

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// Serve resolves and checks the PostgreSQL destination before upgrading. After
// the 101 response, ordinary HTTP error responses are no longer possible.
func (c Handler) Serve(w http.ResponseWriter, r *http.Request) {
	if c.sessions.isDraining() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	if !c.OriginAllowed(r) {
		http.Error(w, "origin denied", http.StatusForbidden)
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
	// Origin was checked above against the gateway's own policy, which the
	// library's host-only comparison cannot express.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(-1) // Frames are streamed to PostgreSQL, never buffered whole.
	client := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	// BackendKeyData arrives during PostgreSQL startup. Keep only its small
	// cancellation key while forwarding all wire bytes without interpreting SQL.
	keyData := &backendKeyCapture{}
	session := &wsSession{ws: ws, backend: backend, cancel: func(ctx context.Context) { c.cancelForShutdown(ctx, upstream, keyData) }}
	if !c.sessions.add(session) {
		return
	}
	defer c.sessions.remove(session)
	if c.Metrics != nil {
		c.Metrics.OpenWebSocket()
		defer c.Metrics.CloseWebSocket()
	}
	type relayEnd struct {
		cancel bool
	}
	// Either relay can finish first. Close both sockets to wake the other relay,
	// then wait for it before releasing the connection slot.
	done := make(chan relayEnd, 2)
	go func() {
		cancel := false
		defer func() { done <- relayEnd{cancel: cancel} }()
		buf := make([]byte, 32<<10)
		for {
			n, e := backend.Read(buf)
			if n > 0 {
				if c.WSIdleTimeout > 0 {
					_ = client.SetReadDeadline(time.Now().Add(c.WSIdleTimeout))
				}
				keyData.feed(buf[:n])
				if _, err := (&deadlineWriter{conn: client, timeout: c.WSWriteTimeout}).Write(buf[:n]); err != nil {
					// A slow or vanished client must not keep PostgreSQL busy.
					cancel = !session.closing.Load()
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	go func() {
		_, _ = io.Copy(&deadlineWriter{conn: backend, timeout: c.WSWriteTimeout}, &idleReader{conn: client, timeout: c.WSIdleTimeout})
		// A close frame also abandons any query still running upstream.
		done <- relayEnd{cancel: true}
	}()
	ended := <-done
	if ended.cancel && !session.closing.Load() {
		c.cancelDisconnectedSession(upstream, keyData)
	}
	_ = ws.CloseNow()
	_ = backend.Close()
	<-done
}

type idleReader struct {
	conn    net.Conn
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	if r.timeout > 0 {
		if err := r.conn.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
			return 0, err
		}
	}
	return r.conn.Read(p)
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
