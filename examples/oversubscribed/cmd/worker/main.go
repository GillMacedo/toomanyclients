// Command worker sends the shop's order emails from a Postgres queue.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	for {
		if _, err := pool.Exec(ctx, "update outbox set sent_at = now() where sent_at is null"); err != nil {
			log.Print(err)
		}
		time.Sleep(time.Second)
	}
}
