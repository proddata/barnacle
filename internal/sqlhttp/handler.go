package sqlhttp

import "github.com/proddata/barnacle/internal/gateway"

// Handler serves the SQL-over-HTTP entrance.
type Handler struct{ *gateway.Config }

func New(config *gateway.Config) Handler { return Handler{Config: config} }
