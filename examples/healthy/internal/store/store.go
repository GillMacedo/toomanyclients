// Package store opens the shop's Postgres connections.
package store

import (
	"database/sql"
	"os"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// envInt reads a whole number from the environment, or returns def.
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Open returns a database/sql pool sized by DB_MAX_OPEN_CONNS.
func Open() (*sql.DB, error) {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(envInt("DB_MAX_OPEN_CONNS", 10))
	db.SetMaxIdleConns(5)
	return db, nil
}
