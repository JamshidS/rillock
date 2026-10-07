package store

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jamshids/rillock/internal/testdb"
)

func TestMigrateAppliesOnce(t *testing.T) {
	ctx := t.Context()
	url := testdb.New(t)

	first, err := Migrate(ctx, url)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if !slices.Contains(first, 1) {
		t.Fatalf("first Migrate applied %v, want it to include version 1", first)
	}

	second, err := Migrate(ctx, url)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second Migrate applied %v, want nothing", second)
	}
}

func TestCheckMigrated(t *testing.T) {
	ctx := t.Context()
	url := testdb.New(t)

	if err := CheckMigrated(ctx, url); !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("before migrating: err = %v, want ErrSchemaOutdated", err)
	}
	if _, err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	if err := CheckMigrated(ctx, url); err != nil {
		t.Fatalf("after migrating: err = %v, want nil", err)
	}
}

func TestOpenReportsUnreachableDatabase(t *testing.T) {
	// Port 1 is reserved and nothing listens there.
	_, err := Open(t.Context(), "postgres://rillock:rillock@127.0.0.1:1/rillock?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "is it running") {
		t.Fatalf("error should tell the user the database may be down, got: %v", err)
	}

	t.Log(err) // visible with -v: shows the message a user would see
}
