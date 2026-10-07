// Package testdb gives each test its own empty PostgreSQL database.
//
// Tests that need a database call New. If RILLOCK_TEST_DATABASE_URL is not
// set, the test is skipped, so `go test ./...` works without Docker. Run
// `make db-up` and `make test-db` to include database tests.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvVar names the variable that points tests at a PostgreSQL server.
const EnvVar = "RILLOCK_TEST_DATABASE_URL"

// New creates a fresh database, returns its URL, and drops it when the test
// ends. A separate database per test means tests never see each other's rows
// and can run in parallel.
func New(t *testing.T) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s not set; run `make db-up` and `make test-db` to include database tests", EnvVar)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() // cleanup only; nothing to do if it fails

	name := "rillock_test_" + randomSuffix(t)
	// Identifiers cannot be passed as $1 parameters, so the name is sanitized
	// and quoted instead. It only contains letters, digits, and underscores.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("drop test database: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		// FORCE disconnects any connection the test forgot to close.
		if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	u.Path = "/" + name
	return u.String()
}

func randomSuffix(t *testing.T) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
