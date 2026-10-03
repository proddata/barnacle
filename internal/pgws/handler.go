package pgws

import "github.com/proddata/hermit/internal/gateway"

// Handler serves the PostgreSQL wire-over-WebSocket entrance.
type Handler struct{ *gateway.Config }

func New(config *gateway.Config) Handler { return Handler{Config: config} }
