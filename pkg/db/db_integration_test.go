//go:build integration

package db

import (
	"context"
	"embed"
	"testing"

	"github.com/acme/acme-card/pkg/testutil"
)

//go:embed testdata/migrations/*.sql
var testMigrations embed.FS

func TestMigrateIntoSchemaAndConnect(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	ctx := context.Background()

	if err := Migrate(dsn, "accounts", testMigrations, "testdata/migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Idempotent: running again is a no-op.
	if err := Migrate(dsn, "accounts", testMigrations, "testdata/migrations"); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	pool, err := Connect(ctx, dsn, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `INSERT INTO widgets(name) VALUES ('a')`); err != nil {
		t.Fatalf("table should resolve via search_path: %v", err)
	}
	var schema string
	if err := pool.QueryRow(ctx, `SELECT table_schema FROM information_schema.tables WHERE table_name='widgets'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != "accounts" {
		t.Fatalf("widgets created in schema %q, want accounts", schema)
	}
	var mig string
	if err := pool.QueryRow(ctx, `SELECT table_schema FROM information_schema.tables WHERE table_name='schema_migrations'`).Scan(&mig); err != nil {
		t.Fatal(err)
	}
	if mig != "accounts" {
		t.Fatalf("schema_migrations in %q, want accounts", mig)
	}
}
