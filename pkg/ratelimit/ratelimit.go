// Package ratelimit implements a Postgres-backed sliding-window-counter limiter.
//
// estimate = hits(current window) + hits(previous window) × (1 − elapsed/window).
// The increment is conditional and atomic, so concurrent requests never exceed the limit.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is the DDL each service that uses a Limiter embeds in its own migrations.
const Migration = `
CREATE TABLE rate_limit_windows (
    bucket       text        NOT NULL,
    window_start timestamptz NOT NULL,
    hits         integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start)
);`

// Limiter allows `limit` hits per `window` per bucket.
type Limiter struct {
	pool   *pgxpool.Pool
	limit  int
	window time.Duration
}

// New builds a Limiter.
func New(pool *pgxpool.Pool, limit int, window time.Duration) *Limiter {
	return &Limiter{pool: pool, limit: limit, window: window}
}

// Allow records a hit for bucket at now if within budget, in its own transaction.
// When blocked it returns retryAfter.
func (l *Limiter) Allow(ctx context.Context, bucket string, now time.Time) (ok bool, retryAfter time.Duration, err error) {
	err = pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		ok, retryAfter, err = l.AllowTx(ctx, tx, bucket, now)
		return err
	})
	return ok, retryAfter, err
}

// AllowTx is Allow inside the caller's transaction (the hit commits or rolls back with it).
// Use it when the caller already holds a pooled connection, to avoid needing a second one.
func (l *Limiter) AllowTx(ctx context.Context, tx pgx.Tx, bucket string, now time.Time) (ok bool, retryAfter time.Duration, err error) {
	now = now.UTC()
	cur := now.Truncate(l.window)
	prev := cur.Add(-l.window)
	weight := 1 - float64(now.Sub(cur))/float64(l.window)

	// Ensure the row exists, then lock it: serializes concurrent hits on the same bucket.
	if _, err := tx.Exec(ctx, `INSERT INTO rate_limit_windows (bucket, window_start) VALUES ($1,$2) ON CONFLICT DO NOTHING`, bucket, cur); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	var curHits, prevHits int
	if err := tx.QueryRow(ctx, `SELECT hits FROM rate_limit_windows WHERE bucket=$1 AND window_start=$2 FOR UPDATE`, bucket, cur).Scan(&curHits); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT hits FROM rate_limit_windows WHERE bucket=$1 AND window_start=$2), 0)`, bucket, prev).Scan(&prevHits); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	if float64(prevHits)*weight+float64(curHits)+1 > float64(l.limit) {
		return false, cur.Add(l.window).Sub(now), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE rate_limit_windows SET hits = hits + 1 WHERE bucket=$1 AND window_start=$2`, bucket, cur); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	return true, 0, nil
}

// Sweep removes windows older than two windows.
func (l *Limiter) Sweep(ctx context.Context, now time.Time) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM rate_limit_windows WHERE window_start < $1`, now.UTC().Add(-2*l.window))
	return err
}
