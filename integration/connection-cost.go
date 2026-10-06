// Run with: TEST_DATABASE_URL=... go run integration/connection-cost.go -mode=fresh -queries=500
// This isolates PostgreSQL connection setup cost; it does not implement Barnacle's HTTP path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	mode := flag.String("mode", "fresh", "fresh or reuse")
	workers := flag.Int("workers", 1, "concurrent PostgreSQL connections")
	queries := flag.Int("queries", 500, "total select 1 queries")
	flag.Parse()
	if (*mode != "fresh" && *mode != "reuse") || *workers < 1 || *queries < *workers {
		panic("use -mode=fresh|reuse -workers=N -queries=N (queries >= workers)")
	}
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		panic("set TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(url)
	check(err)
	ctx := context.Background()
	observer, err := pgx.ConnectConfig(ctx, config)
	check(err)
	defer observer.Close(ctx)
	sessions := func() int64 {
		_, err := observer.Exec(ctx, "select pg_stat_clear_snapshot()")
		check(err)
		var count int64
		check(observer.QueryRow(ctx, "select sessions from pg_stat_database where datname=current_database()").Scan(&count))
		return count
	}
	before := sessions()
	latencies := make([]float64, *queries)
	var group sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	start := time.Now()
	for worker := 0; worker < *workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			var conn *pgx.Conn
			var connErr error
			if *mode == "reuse" {
				conn, connErr = pgx.ConnectConfig(ctx, config)
				if connErr != nil {
					errOnce.Do(func() { firstErr = connErr })
					return
				}
				defer conn.Close(ctx)
			}
			for i := worker; i < *queries; i += *workers {
				queryStart := time.Now()
				if *mode == "fresh" {
					conn, connErr = pgx.ConnectConfig(ctx, config)
					if connErr != nil {
						errOnce.Do(func() { firstErr = connErr })
						return
					}
				}
				var answer int
				queryErr := conn.QueryRow(ctx, "select 1").Scan(&answer)
				if *mode == "fresh" {
					closeErr := conn.Close(ctx)
					if queryErr == nil {
						queryErr = closeErr
					}
				}
				if queryErr != nil || answer != 1 {
					errOnce.Do(func() { firstErr = fmt.Errorf("query failed: %v (answer %d)", queryErr, answer) })
					return
				}
				latencies[i] = float64(time.Since(queryStart).Microseconds()) / 1000
			}
		}(worker)
	}
	group.Wait()
	duration := time.Since(start)
	check(firstErr)
	time.Sleep(time.Second) // PostgreSQL statistics updates may lag the completed sessions.
	created := sessions() - before
	sort.Float64s(latencies)
	p := func(fraction float64) float64 { return latencies[int(float64(len(latencies)-1)*fraction)] }
	result := map[string]any{
		"mode": *mode, "workers": *workers, "queries": *queries,
		"seconds": duration.Seconds(), "qps": float64(*queries) / duration.Seconds(),
		"p50Ms": p(0.50), "p95Ms": p(0.95), "p99Ms": p(0.99),
		"postgresSessionCounterDelta": created,
	}
	check(json.NewEncoder(os.Stdout).Encode(result))
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
