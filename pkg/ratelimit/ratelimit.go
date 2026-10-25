// Package ratelimit implements a Postgres-backed sliding-window-counter limiter.
//
// estimate = hits(current window) + hits(previous window) × (1 − elapsed/window).
// The increment is conditional and atomic (a single statement), so concurrent requests never
// exceed the limit and a check costs one round trip.
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
//
// One statement: the previous window is read, then the current window row is inserted or
// incremented only while the weighted estimate stays within the limit. ON CONFLICT DO UPDATE
// locks the current row and re-evaluates the condition against its latest value, so concurrent
// hits are exact.
func (l *Limiter) AllowTx(ctx context.Context, tx pgx.Tx, bucket string, now time.Time) (ok bool, retryAfter time.Duration, err error) {
	now = now.UTC()
	cur := now.Truncate(l.window)
	prev := cur.Add(-l.window)
	weight := 1 - float64(now.Sub(cur))/float64(l.window)

	if err := tx.QueryRow(ctx, `
		WITH prev AS (
		    SELECT coalesce((SELECT hits FROM rate_limit_windows WHERE bucket = $1 AND window_start = $3), 0)::float8 AS hits
		), hit AS (
		    INSERT INTO rate_limit_windows (bucket, window_start, hits)
		    SELECT $1, $2, 1 FROM prev WHERE prev.hits * $4 + 1 <= $5
		    ON CONFLICT (bucket, window_start) DO UPDATE SET hits = rate_limit_windows.hits + 1
		    WHERE (SELECT hits FROM prev) * $4 + rate_limit_windows.hits + 1 <= $5
		    RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM hit)`, bucket, cur, prev, weight, float64(l.limit)).Scan(&ok); err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	if !ok {
		return false, cur.Add(l.window).Sub(now), nil
	}
	return true, 0, nil
}

// Sweep removes windows older than two windows.
func (l *Limiter) Sweep(ctx context.Context, now time.Time) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM rate_limit_windows WHERE window_start < $1`, now.UTC().Add(-2*l.window))
	return err
}
