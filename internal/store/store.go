// Package store is Rillock's only access to PostgreSQL. All SQL lives here.
package store

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds a pool of database connections. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url and checks that it answers.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, describeConnectError(err)
	}
	// pgxpool.New does not connect yet. Ping does, so a wrong address or
	// password is reported here instead of on the first query.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, describeConnectError(err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all connections.
func (s *Store) Close() {
	s.pool.Close()
}

// describeConnectError turns low-level connection errors into a message that
// says what to do. The original error stays wrapped for errors.Is/As.
func describeConnectError(err error) error {
	var netErr *net.OpError
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &netErr):
		return fmt.Errorf("cannot reach the database (is it running? try `make db-up`): %w", err)
	case errors.As(err, &pgErr) && pgErr.Code == "28P01":
		return fmt.Errorf("database rejected the password: %w", err)
	default:
		return fmt.Errorf("connect to database: %w", err)
	}
}
