// Package pgws tunnels PostgreSQL's wire protocol over WebSocket. The client
// and PostgreSQL perform authentication inside the tunnel; this package checks
// the route, limits resources, and handles disconnects.
package pgws

import (
	"context"

	"github.com/proddata/barnacle/internal/gateway"
)

// Handler serves the PostgreSQL wire-over-WebSocket entrance.
type Handler struct {
	*gateway.Config
	sessions *sessionRegistry
}

func New(config *gateway.Config) Handler {
	return Handler{Config: config, sessions: newSessionRegistry()}
}

func (h Handler) Shutdown(ctx context.Context) error {
	return h.sessions.shutdown(ctx)
}
