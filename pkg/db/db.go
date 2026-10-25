// Package db wires pgx pools and runs embedded migrations, one Postgres schema per service.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a 20-connection pgx pool whose search_path is pinned to the service schema.
func Connect(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	return ConnectN(ctx, dsn, schema, 20)
}

// ConnectN is Connect with an explicit pool size (e.g. a tiny dedicated pool for readiness probes).
func ConnectN(ctx context.Context, dsn, schema string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all up migrations from dir inside files into schema. Safe to call repeatedly.
// The migrations table lives inside the schema so several services can share one database.
func Migrate(dsn, schema string, files fs.FS, dir string) error {
	src, err := iofs.New(files, dir)
	if err != nil {
		return fmt.Errorf("db: migration source: %w", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("db: parse dsn: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	u.Scheme = "pgx5"

	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return fmt.Errorf("db: migrate init: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}
