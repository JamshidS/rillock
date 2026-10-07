package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jamshids/rillock/internal/testdb"
)

// migratedConn returns a connection to a fresh, fully migrated database.
func migratedConn(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx := t.Context()
	url := testdb.New(t)
	if _, err := Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(t.Context()) })
	return conn
}

// insertRunWithEvent creates the minimum rows needed for one run with one event.
func insertRunWithEvent(t *testing.T, conn *pgx.Conn) (runID string) {
	t.Helper()
	ctx := t.Context()
	mustExec(t, conn, `INSERT INTO agents (name) VALUES ('refund-agent')`)

	var versionID string
	err := conn.QueryRow(ctx, `
		INSERT INTO agent_versions (agent_name, version, spec, spec_hash)
		VALUES ('refund-agent', 1, '{}', 'hash-1')
		RETURNING id`).Scan(&versionID)
	if err != nil {
		t.Fatal(err)
	}

	err = conn.QueryRow(ctx, `
		INSERT INTO runs (agent_version_id, input, state)
		VALUES ($1, '{"text": "refund order 812"}', 'queued')
		RETURNING id`, versionID).Scan(&runID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, `INSERT INTO events (run_id, seq, type) VALUES ($1, 1, 'RunCreated')`, runID)
	return runID
}

func TestEventsAreAppendOnly(t *testing.T) {
	conn := migratedConn(t)
	runID := insertRunWithEvent(t, conn)

	statements := map[string]string{
		"update":   `UPDATE events SET type = 'Tampered' WHERE run_id = $1`,
		"delete":   `DELETE FROM events WHERE run_id = $1`,
		"truncate": `TRUNCATE events`,
	}
	for name, sql := range statements {
		t.Run(name, func(t *testing.T) {
			var args []any
			if strings.Contains(sql, "$1") {
				args = append(args, runID)
			}
			_, err := conn.Exec(t.Context(), sql, args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "append-only") {
				t.Fatalf("err = %v, want the append-only trigger to reject it", err)
			}
		})
	}

	// Appending still works.
	mustExec(t, conn, `INSERT INTO events (run_id, seq, type) VALUES ($1, 2, 'RunClaimed')`, runID)
}

func TestSchemaRejectsInvalidRows(t *testing.T) {
	conn := migratedConn(t)
	runID := insertRunWithEvent(t, conn)
	mustExec(t, conn, `INSERT INTO tools (name) VALUES ('payments.refund')`)

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{"unknown run state", `UPDATE runs SET state = 'paused' WHERE id = $1`, []any{runID}},
		{"duplicate event sequence", `INSERT INTO events (run_id, seq, type) VALUES ($1, 1, 'Again')`, []any{runID}},
		{"event for missing run", `INSERT INTO events (run_id, seq, type) VALUES (uuidv7(), 1, 'Orphan')`, nil},
		{"unknown effect class", `INSERT INTO tool_versions (tool_name, version, effect_class, spec, spec_hash)
			VALUES ('payments.refund', 1, 'maybe_safe', '{}', 'h')`, nil},
		{"duplicate agent version", `INSERT INTO agent_versions (agent_name, version, spec, spec_hash)
			VALUES ('refund-agent', 1, '{}', 'hash-2')`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := conn.Exec(t.Context(), tt.sql, tt.args...); err == nil {
				t.Fatal("expected the database to reject this")
			}
		})
	}
}

func TestIDsAreTimeOrderedUUIDv7(t *testing.T) {
	conn := migratedConn(t)
	runID := insertRunWithEvent(t, conn)
	// The version digit of a UUID is the first character of the third group.
	if parts := strings.Split(runID, "-"); len(parts) != 5 || parts[2][0] != '7' {
		t.Fatalf("run id %s is not a UUIDv7", runID)
	}
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
