//go:build integration

// Package testutil hosts shared helpers for integration tests (Docker required).
package testutil

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// StartPostgres launches a disposable Postgres 16 with the four service schemas and returns its DSN.
func StartPostgres(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("acmecard"),
		tcpostgres.WithUsername("acme"),
		tcpostgres.WithPassword("acme"),
		tcpostgres.WithInitScripts(filepath.Join(RepoRoot(), "deploy", "postgres-init.sql")),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// RepoRoot returns the absolute repository root, independent of the test's package directory.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}
