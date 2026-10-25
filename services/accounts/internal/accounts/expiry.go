package accounts

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

// ExpireDue releases holds of approved authorizations past expires_at. Rows being handled by a
// concurrent capture/reversal are skipped (SKIP LOCKED) and picked up on the next run, so several
// replicas can run the worker safely. Returns how many authorizations expired.
func (s *Service) ExpireDue(ctx context.Context, batch int) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+authCols+` FROM authorizations
			WHERE status='approved' AND expires_at < $1 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, s.clock.Now(), batch)
		if err != nil {
			return err
		}
		var due []authRow
		for rows.Next() {
			a, err := scanAuth(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range due {
			if _, err := s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{
				HoldReference: "auth:" + a.NetworkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED}); err != nil {
				return err
			}
			after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='expired' WHERE id=$1 RETURNING `+authCols, a.ID))
			if err != nil {
				return err
			}
			if err := s.emitAuth(ctx, tx, "authorization.expired", a, after, nil); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RunExpiry runs ExpireDue every interval until ctx is cancelled.
func (s *Service) RunExpiry(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for {
			n, err := s.ExpireDue(ctx, 100)
			if err != nil && ctx.Err() == nil {
				log.Error("hold expiry failed", "err", err)
			}
			if n > 0 {
				log.Info("authorizations expired", "count", n)
			}
			if n < 100 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
