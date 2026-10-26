// Package store persists audit events append-only, in a tamper-evident hash chain, and consumes
// them from SQS.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/events"
)

// Store writes audit_events.
type Store struct{ pool *pgxpool.Pool }

// New builds a Store.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Record redacts ev, then appends it to the chain under the chain-head lock (contiguous
// chain_seq). Redelivered events (same id) are ignored without advancing the chain; inserted
// reports whether a row was added.
func (s *Store) Record(ctx context.Context, ev events.Event) (inserted bool, err error) {
	var redB, redA bool
	ev.Before, redB = Redact(ev.Before)
	ev.After, redA = Redact(ev.After)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var seq int64
		var prev string
		if err := tx.QueryRow(ctx, `SELECT chain_seq, hash FROM chain_head WHERE id=1 FOR UPDATE`).Scan(&seq, &prev); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE id=$1)`, ev.ID).Scan(&exists); err != nil || exists {
			return err
		}
		if err := insertChained(ctx, tx, ev, seq+1, prev, redB || redA); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("audit: insert: %w", err)
	}
	return inserted, nil
}

func insertChained(ctx context.Context, tx pgx.Tx, ev events.Event, seq int64, prev string, redacted bool) error {
	canon, err := Canonical(ev, seq)
	if err != nil {
		return err
	}
	hash := Hash(prev, canon)
	var status *int
	if ev.HTTPStatus != 0 {
		status = &ev.HTTPStatus
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_events (id, event_type, aggregate_type, aggregate_id, actor, idempotency_key_hash, http_status, before, after, occurred_at,
			source, source_seq, chain_seq, prev_hash, hash, redacted)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, $9, $10, NULLIF($11, ''), NULLIF($12::bigint, 0), $13, $14, $15, $16)`,
		ev.ID, ev.Type, ev.AggregateType, ev.AggregateID, ev.Actor, ev.IdempotencyKeyHash, status, nullJSON(ev.Before), nullJSON(ev.After),
		ev.OccurredAt.UTC().Truncate(time.Microsecond), ev.Source, ev.SourceSeq, seq, prev, hash, redacted); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE chain_head SET chain_seq=$1, hash=$2 WHERE id=1`, seq, hash)
	return err
}

// rowEvent rebuilds the hashed event from a stored row.
const rowCols = `id::text, event_type, aggregate_type, aggregate_id, coalesce(actor,''), coalesce(idempotency_key_hash,''), coalesce(http_status,0),
	before::text, after::text, occurred_at, coalesce(source,''), coalesce(source_seq,0)`

func scanRowEvent(row pgx.Row, extra ...any) (events.Event, error) {
	var ev events.Event
	var before, after *string
	dest := append([]any{&ev.ID, &ev.Type, &ev.AggregateType, &ev.AggregateID, &ev.Actor, &ev.IdempotencyKeyHash, &ev.HTTPStatus,
		&before, &after, &ev.OccurredAt, &ev.Source, &ev.SourceSeq}, extra...)
	if err := row.Scan(dest...); err != nil {
		return ev, err
	}
	if before != nil {
		ev.Before = json.RawMessage(*before)
	}
	if after != nil {
		ev.After = json.RawMessage(*after)
	}
	return ev, nil
}

// BackfillChain chains rows written before Sprint 6 (chain_seq NULL), in recorded order.
func (s *Store) BackfillChain(ctx context.Context) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var seq int64
		var prev string
		if err := tx.QueryRow(ctx, `SELECT chain_seq, hash FROM chain_head WHERE id=1 FOR UPDATE`).Scan(&seq, &prev); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('audit.backfill', 'on', true)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+rowCols+` FROM audit_events WHERE chain_seq IS NULL ORDER BY recorded_at, id`)
		if err != nil {
			return err
		}
		var pending []events.Event
		for rows.Next() {
			ev, err := scanRowEvent(rows)
			if err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, ev)
		}
		rows.Close()
		for _, ev := range pending {
			seq++
			canon, err := Canonical(ev, seq)
			if err != nil {
				return err
			}
			hash := Hash(prev, canon)
			if _, err := tx.Exec(ctx, `UPDATE audit_events SET chain_seq=$2, prev_hash=$3, hash=$4 WHERE id=$1`, ev.ID, seq, prev, hash); err != nil {
				return err
			}
			prev = hash
			n++
		}
		_, err = tx.Exec(ctx, `UPDATE chain_head SET chain_seq=$1, hash=$2 WHERE id=1`, seq, prev)
		return err
	})
	return n, err
}

// ErrChainMissing is returned when the start of a verified range has no predecessor on record.
var ErrChainMissing = errors.New("audit: chain predecessor missing")

// VerifyRange recomputes the chain from chain_seq `from` (to = 0 means the head) and returns the
// first chain_seq that is missing, out of order or whose hash does not match (0 = intact), and
// how many rows were checked. A start > 1 links to the stored hash of from-1.
func (s *Store) VerifyRange(ctx context.Context, from, to int64) (firstBad, checked int64, err error) {
	return s.VerifyFrom(ctx, from, to, "")
}

// VerifyFrom is VerifyRange with an explicit prev hash for `from` (e.g. from an archive manifest);
// an empty prev uses genesis for 1 or the stored hash of from-1.
func (s *Store) VerifyFrom(ctx context.Context, from, to int64, prev string) (firstBad, checked int64, err error) {
	if prev == "" {
		if from <= 1 {
			prev, from = GenesisHash, 1
		} else if err := s.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=$1`, from-1).Scan(&prev); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return from, 0, ErrChainMissing
			}
			return 0, 0, err
		}
	}
	if to <= 0 {
		if err := s.pool.QueryRow(ctx, `SELECT chain_seq FROM chain_head WHERE id=1`).Scan(&to); err != nil {
			return 0, 0, err
		}
	}
	expect := from
	const page = 5000
	for expect <= to {
		rows, err := s.pool.Query(ctx, `SELECT `+rowCols+`, chain_seq, prev_hash, hash FROM audit_events
			WHERE chain_seq >= $1 AND chain_seq <= $2 ORDER BY chain_seq LIMIT $3`, expect, to, page)
		if err != nil {
			return 0, checked, err
		}
		got := 0
		for rows.Next() {
			var seq int64
			var storedPrev, storedHash string
			ev, err := scanRowEvent(rows, &seq, &storedPrev, &storedHash)
			if err != nil {
				rows.Close()
				return 0, checked, err
			}
			got++
			if seq != expect || storedPrev != prev {
				rows.Close()
				return expect, checked, nil
			}
			canon, err := Canonical(ev, seq)
			if err != nil {
				rows.Close()
				return seq, checked, nil
			}
			if h := Hash(prev, canon); h != storedHash {
				rows.Close()
				return seq, checked, nil
			}
			prev = storedHash
			expect++
			checked++
		}
		rows.Close()
		if got == 0 {
			return expect, checked, nil // rows missing up to the head
		}
	}
	return 0, checked, nil
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// EventAt returns the stored event at chain_seq (as hashed: redacted, normalized by jsonb).
func (s *Store) EventAt(ctx context.Context, chainSeq int64) (events.Event, error) {
	return scanRowEvent(s.pool.QueryRow(ctx, `SELECT `+rowCols+` FROM audit_events WHERE chain_seq=$1`, chainSeq))
}

// Head returns the chain head.
func (s *Store) Head(ctx context.Context) (int64, string, error) {
	var seq int64
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT chain_seq, hash FROM chain_head WHERE id=1`).Scan(&seq, &hash)
	return seq, hash, err
}

// Each calls fn for every stored row with from <= chain_seq <= to, in chain order.
func (s *Store) Each(ctx context.Context, from, to int64, fn func(ev events.Event, chainSeq int64, prevHash, hash string) error) error {
	const page = 5000
	for next := from; next <= to; {
		rows, err := s.pool.Query(ctx, `SELECT `+rowCols+`, chain_seq, prev_hash, hash FROM audit_events
			WHERE chain_seq >= $1 AND chain_seq <= $2 ORDER BY chain_seq LIMIT $3`, next, to, page)
		if err != nil {
			return err
		}
		got := 0
		for rows.Next() {
			var seq int64
			var prev, hash string
			ev, err := scanRowEvent(rows, &seq, &prev, &hash)
			if err == nil {
				err = fn(ev, seq, prev, hash)
			}
			if err != nil {
				rows.Close()
				return err
			}
			got++
			next = seq + 1
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if got < page {
			return nil
		}
	}
	return nil
}
