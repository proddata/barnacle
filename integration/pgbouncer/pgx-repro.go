// Run with: go run ./integration/pgbouncer/pgx-repro.go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func main() {
	for mode, port := range map[string]int{"session": 16431, "transaction": 16432, "statement": 16433} {
		url := fmt.Sprintf("postgres://barnacle:barnacle_dev_password@127.0.0.1:%d/barnacle?sslmode=disable", port)
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			panic(err)
		}
		cfg.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
		ctx := context.Background()
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			panic(err)
		}
		var first int
		err = conn.QueryRow(ctx, "select $1::int", 1).Scan(&first)
		if err != nil {
			panic(err)
		}
		time.Sleep(3 * time.Second) // PgBouncer closes idle server connections after one second.
		var second int
		err = conn.QueryRow(ctx, "select $1::int", 2).Scan(&second)
		fmt.Printf("%s: first=%d second=%d error=%v\n", mode, first, second, err)
		_ = conn.Close(ctx)
		var pgErr *pgconn.PgError
		if mode == "session" && err != nil || mode != "session" && (!errors.As(err, &pgErr) || pgErr.Code != "26000") {
			fmt.Fprintln(os.Stderr, "unexpected prepared statement result")
			os.Exit(1)
		}
	}
}
