// Package store persists audit events append-only and consumes them from SQS.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/events"
)

// Store writes audit_events.
type Store struct{ pool *pgxpool.Pool }

// New builds a Store.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Record inserts ev. Redelivered events (same id) are ignored; inserted reports whether a row was added.
func (s *Store) Record(ctx context.Context, ev events.Event) (inserted bool, err error) {
	var status *int
	if ev.HTTPStatus != 0 {
		status = &ev.HTTPStatus
	}
	var keyHash *string
	if ev.IdempotencyKeyHash != "" {
		keyHash = &ev.IdempotencyKeyHash
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO audit_events (id, event_type, aggregate_type, aggregate_id, actor, idempotency_key_hash, http_status, before, after, occurred_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO NOTHING`,
		ev.ID, ev.Type, ev.AggregateType, ev.AggregateID, ev.Actor, keyHash, status, nullJSON(ev.Before), nullJSON(ev.After), ev.OccurredAt)
	if err != nil {
		return false, fmt.Errorf("audit: insert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
