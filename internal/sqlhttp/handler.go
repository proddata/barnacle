// Package sqlhttp translates Neon's SQL-over-HTTP shape into PostgreSQL queries.
// PostgreSQL authenticates the supplied credentials; the gateway limits and
// routes requests before opening a database connection.
package sqlhttp

import "github.com/proddata/barnacle/internal/gateway"

// Handler serves the SQL-over-HTTP entrance.
type Handler struct{ *gateway.Config }

func New(config *gateway.Config) Handler { return Handler{Config: config} }
