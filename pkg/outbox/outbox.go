// Package outbox implements the transactional outbox: events are inserted in the same
// transaction as the state change, then a relay publishes them (at-least-once).
//
// Every service schema that uses it must contain the table from Migration.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/events"
)

// Migration is the DDL each service embeds in its own migrations.
const Migration = `
CREATE TABLE outbox (
    seq          bigserial   PRIMARY KEY,
    event_id     uuid        NOT NULL UNIQUE,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;`

// Execer is satisfied by pgx.Tx, *pgx.Conn and *pgxpool.Pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue writes evs through tx (normally the state-change transaction).
func Enqueue(ctx context.Context, tx Execer, evs ...events.Event) error {
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("outbox: marshal: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (event_id, payload) VALUES ($1, $2)`, ev.ID, b); err != nil {
			return fmt.Errorf("outbox: insert: %w", err)
		}
	}
	return nil
}

// Relay moves unpublished rows to a Publisher.
type Relay struct {
	pool  *pgxpool.Pool
	pub   events.Publisher
	batch int
}

// NewRelay builds a relay publishing up to batch events per round.
func NewRelay(pool *pgxpool.Pool, pub events.Publisher, batch int) *Relay {
	return &Relay{pool: pool, pub: pub, batch: batch}
}

// RelayOnce publishes one batch. Rows are locked with SKIP LOCKED so several relay workers
// (one per replica) never publish the same row concurrently.
func (r *Relay) RelayOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT seq, payload FROM outbox WHERE published_at IS NULL
		ORDER BY seq LIMIT $1 FOR UPDATE SKIP LOCKED`, r.batch)
	if err != nil {
		return 0, fmt.Errorf("outbox: select: %w", err)
	}
	var (
		seqs []int64
		evs  []events.Event
	)
	for rows.Next() {
		var (
			seq int64
			raw []byte
			ev  events.Event
		)
		if err := rows.Scan(&seq, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox: decode seq %d: %w", seq, err)
		}
		seqs, evs = append(seqs, seq), append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	if err := r.pub.Publish(ctx, evs); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE seq = ANY($1)`, seqs); err != nil {
		return 0, err
	}
	return len(evs), tx.Commit(ctx)
}

// Run relays until ctx is cancelled, sleeping interval when idle.
func (r *Relay) Run(ctx context.Context, interval time.Duration, log *slog.Logger) {
	for {
		n, err := r.RelayOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("outbox relay failed", "err", err)
		}
		if n == r.batch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
