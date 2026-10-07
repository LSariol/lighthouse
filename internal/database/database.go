// Package database is Lighthouse's Postgres store, with its built-in migrations.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database is a connection pool, logged in as lighthouse_app.
type Database struct {
	pool *pgxpool.Pool
}

// Connect opens a pool for connString (LIGHTHOUSE_DATABASE_URL) and checks
// that the database answers.
func Connect(ctx context.Context, connString string) (*Database, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("LIGHTHOUSE_DATABASE_URL isn't a valid connection string: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("can't reach the database at LIGHTHOUSE_DATABASE_URL: %w", err)
	}
	return &Database{pool: pool}, nil
}

// Ping checks that the database answers, giving up after 2 seconds.
func (d *Database) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return d.pool.Ping(ctx)
}

func (d *Database) Close() {
	d.pool.Close()
}

// IsUnreachable reports whether err means the database couldn't be reached at
// all (down, restarting, wrong host), as opposed to an error from it.
func IsUnreachable(err error) bool {
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		return false
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr)
}
