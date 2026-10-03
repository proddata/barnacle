package pgws

import (
	"context"

	"github.com/proddata/hermit/internal/gateway"
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
