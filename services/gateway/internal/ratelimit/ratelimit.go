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

// Allow records a hit for bucket at now if within budget. When blocked it returns retryAfter.
func (l *Limiter) Allow(ctx context.Context, bucket string, now time.Time) (ok bool, retryAfter time.Duration, err error) {
	now = now.UTC()
	cur := now.Truncate(l.window)
	prev := cur.Add(-l.window)
	weight := 1 - float64(now.Sub(cur))/float64(l.window)

	err = pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		// Ensure the row exists, then lock it: serializes concurrent hits on the same bucket.
		if _, err := tx.Exec(ctx, `INSERT INTO rate_limit_windows (bucket, window_start) VALUES ($1,$2) ON CONFLICT DO NOTHING`, bucket, cur); err != nil {
			return err
		}
		var curHits int
		if err := tx.QueryRow(ctx, `SELECT hits FROM rate_limit_windows WHERE bucket=$1 AND window_start=$2 FOR UPDATE`, bucket, cur).Scan(&curHits); err != nil {
			return err
		}
		var prevHits int
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT hits FROM rate_limit_windows WHERE bucket=$1 AND window_start=$2), 0)`, bucket, prev).Scan(&prevHits); err != nil {
			return err
		}
		if float64(prevHits)*weight+float64(curHits)+1 > float64(l.limit) {
			ok = false
			retryAfter = cur.Add(l.window).Sub(now)
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE rate_limit_windows SET hits = hits + 1 WHERE bucket=$1 AND window_start=$2`, bucket, cur); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return false, 0, fmt.Errorf("ratelimit: %w", err)
	}
	return ok, retryAfter, nil
}

// Sweep removes windows older than two windows.
func (l *Limiter) Sweep(ctx context.Context, now time.Time) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM rate_limit_windows WHERE window_start < $1`, now.UTC().Add(-2*l.window))
	return err
}
