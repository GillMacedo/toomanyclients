// Command repro opens one Postgres pool per simulated pod, fills every pool
// at once with pg_sleep queries, and prints how many connections Postgres
// accepted and the errors it returned. It shows what toomanyclients
// predicts: once the pools add up to more than max_connections minus the
// reserved slots, new connections fail with SQLSTATE 53300.
//
//	go run ./repro -dsn 'postgres://app@127.0.0.1:54329/postgres' -pods 4 -pool 5
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres connection string")
	pods := flag.Int("pods", 2, "pools to open, one per simulated pod")
	size := flag.Int("pool", 5, "SetMaxOpenConns or pool_max_conns of each pool; 0 means the library default")
	kind := flag.String("kind", "sql", "sql for database/sql, pgxpool for pgxpool")
	hold := flag.Duration("hold", 2*time.Second, "how long each query holds its connection")
	queries := flag.Int("queries", 0, "concurrent queries per pod; 0 means the pool size, or 50 with no limit")
	flag.Parse()

	perPod := *queries
	if perPod == 0 {
		perPod = *size
		if perPod == 0 {
			perPod = 50
		}
	}
	ctx := context.Background()
	var mu sync.Mutex
	errs := map[string]int{}
	ok := 0
	var wg sync.WaitGroup
	for p := 0; p < *pods; p++ {
		run, closeFn, err := openPool(ctx, *kind, *dsn, *size)
		if err != nil {
			log.Fatal(err)
		}
		defer closeFn()
		for q := 0; q < perPod; q++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := run(ctx, hold.Seconds())
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs[err.Error()]++
					return
				}
				ok++
			}()
		}
	}
	wg.Wait()
	fmt.Printf("%s, %d pods x pool %d, %d queries at once: %d succeeded\n", *kind, *pods, *size, *pods*perPod, ok)
	var msgs []string
	for m := range errs {
		msgs = append(msgs, m)
	}
	sort.Strings(msgs)
	for _, m := range msgs {
		fmt.Printf("%d failed: %s\n", errs[m], m)
	}
}

type runFunc func(ctx context.Context, secs float64) error

func openPool(ctx context.Context, kind, dsn string, size int) (runFunc, func(), error) {
	switch kind {
	case "sql":
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return nil, nil, err
		}
		db.SetMaxOpenConns(size)
		return func(ctx context.Context, secs float64) error {
			_, err := db.ExecContext(ctx, "select pg_sleep($1)", secs)
			return err
		}, func() { db.Close() }, nil
	case "pgxpool":
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			return nil, nil, err
		}
		if size > 0 {
			cfg.MaxConns = int32(size)
		}
		fmt.Printf("pgxpool MaxConns %d\n", cfg.MaxConns)
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return func(ctx context.Context, secs float64) error {
			_, err := pool.Exec(ctx, "select pg_sleep($1)", secs)
			return err
		}, pool.Close, nil
	}
	return nil, nil, fmt.Errorf("unknown -kind %q", kind)
}
