package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RejectionRecorder records rejected webhook attempts.
type RejectionRecorder interface {
	Record(ctx context.Context, source, reason string, at time.Time) error
}

// RejectionLog collapses rejections to one row per (source, window).
type RejectionLog struct {
	pool   *pgxpool.Pool
	window time.Duration
}

// NewRejectionLog builds a RejectionLog.
func NewRejectionLog(pool *pgxpool.Pool, window time.Duration) *RejectionLog {
	return &RejectionLog{pool: pool, window: window}
}

// Record upserts the (source, window) row.
func (r *RejectionLog) Record(ctx context.Context, source, reason string, at time.Time) error {
	at = at.UTC()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO webhook_rejections (source, window_start, last_reason, first_seen, last_seen)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (source, window_start) DO UPDATE
		SET attempts = webhook_rejections.attempts + 1, last_reason = EXCLUDED.last_reason, last_seen = EXCLUDED.last_seen`,
		source, at.Truncate(r.window), reason, at)
	return err
}
