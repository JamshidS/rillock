package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql
	"github.com/pressly/goose/v3"
)

// The SQL files are compiled into the binary, so `rillock migrate` works
// without the source tree.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies every migration that has not been applied yet and returns
// the versions it applied. Running it again applies nothing.
func Migrate(ctx context.Context, url string) ([]int64, error) {
	var applied []int64
	err := withMigrations(ctx, url, func(p *goose.Provider) error {
		results, err := p.Up(ctx)
		if err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		for _, r := range results {
			applied = append(applied, r.Source.Version)
		}
		return nil
	})
	return applied, err
}

// ErrSchemaOutdated means migrations are pending.
var ErrSchemaOutdated = errors.New("the database schema is out of date: run `rillock migrate`")

// CheckMigrated returns ErrSchemaOutdated if migrations are pending, so the
// server refuses to start on an old schema instead of failing on the first query.
func CheckMigrated(ctx context.Context, url string) error {
	return withMigrations(ctx, url, func(p *goose.Provider) error {
		pending, err := p.HasPending(ctx)
		if err != nil {
			return fmt.Errorf("check migrations: %w", err)
		}
		if pending {
			return ErrSchemaOutdated
		}
		return nil
	})
}

// withMigrations opens a short-lived connection and gives fn a goose provider
// loaded with the embedded migrations. goose uses database/sql rather than
// pgx's own interface, so it cannot share the Store's pool.
func withMigrations(ctx context.Context, url string, fn func(*goose.Provider) error) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return describeConnectError(err)
	}
	defer closeQuietly(db)
	// sql.Open does not connect; Ping does, so a stopped database is reported
	// clearly here.
	if err := db.PingContext(ctx); err != nil {
		return describeConnectError(err)
	}

	dir, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, dir)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	return fn(provider)
}

// closeQuietly closes a short-lived handle whose Close error cannot be acted
// on: everything we needed from it has already succeeded or failed.
func closeQuietly(c io.Closer) {
	_ = c.Close()
}
