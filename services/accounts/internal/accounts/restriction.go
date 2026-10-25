package accounts

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/outbox"
)

// RestrictionPaymentReturned is the system restriction set by a returned payment (accepted risk,
// ADR-018). The customer cannot lift it.
const RestrictionPaymentReturned = "payment_returned"

// restrictInTx is fail-safe (review I1): every payment return restricts the card inside its own
// finalize transaction, without reading the ledger, and bumps exposure_epoch. A lift afterwards
// clears it when exposure is within the limit; if that lift never happens, the card stays safely
// restricted until the sweep lifts it.
func (s *Service) restrictInTx(ctx context.Context, tx pgx.Tx, accountID string, meta *commonv1.RequestMeta) error {
	before, err := s.load(ctx, tx, accountID, true)
	if err != nil {
		return err
	}
	after, err := scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET
			restricted_reason = $2,
			restricted_at = CASE WHEN restricted_reason IS NULL THEN $3 ELSE restricted_at END,
			exposure_epoch = exposure_epoch + 1, version = version + 1, updated_at = now()
		WHERE id = $1 RETURNING `+accountCols, accountID, RestrictionPaymentReturned, s.clock.Now()))
	if err != nil || before.Restricted != nil {
		return err
	}
	ev := event("card.restricted", "account", accountID, meta)
	ev.Before, ev.After = snapshot(before), snapshot(after)
	return outbox.Enqueue(ctx, tx, ev)
}

// liftRestriction reads the ledger exposure outside any lock and lifts the restriction if
// Σ holds + receivable ≤ credit limit, unless a return happened since the read (epoch changed).
func (s *Service) liftRestriction(ctx context.Context, accountID string, meta *commonv1.RequestMeta) error {
	acc, err := s.load(ctx, s.pool, accountID, false)
	if err != nil || acc.Restricted == nil {
		return err
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return err
	}
	if v.Holds+v.Receivable > acc.CreditLimit {
		return nil
	}
	return s.clearRestriction(ctx, acc.ID, acc.Epoch, meta)
}

// clearRestriction lifts the restriction only if exposure_epoch still equals seenEpoch, i.e. no
// payment return committed after the exposure reading the caller based its decision on (review M1).
func (s *Service) clearRestriction(ctx context.Context, accountID string, seenEpoch int64, meta *commonv1.RequestMeta) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, accountID, true)
		if err != nil || before.Restricted == nil || before.Epoch != seenEpoch {
			return err
		}
		after, err := scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET restricted_reason=NULL, restricted_at=NULL, version=version+1, updated_at=now()
			WHERE id=$1 RETURNING `+accountCols, accountID))
		if err != nil {
			return err
		}
		ev := event("card.unrestricted", "account", accountID, meta)
		ev.Before, ev.After = snapshot(before), snapshot(after)
		return outbox.Enqueue(ctx, tx, ev)
	})
}

// liftQuietly is liftRestriction for request paths: a failed lift leaves the card restricted,
// which is the safe state, and the per-minute sweep retries it.
func (s *Service) liftQuietly(ctx context.Context, accountID string, meta *commonv1.RequestMeta) {
	if err := s.liftRestriction(ctx, accountID, meta); err != nil {
		slog.WarnContext(ctx, "restriction lift deferred to the sweep", "account_id", accountID, "err", err)
	}
}

// ReevaluateRestrictions lifts restrictions whose exposure dropped back within the limit through
// any path (releases, expiries, reversals, refunds, deposits). Returns how many were checked.
func (s *Service) ReevaluateRestrictions(ctx context.Context, batch int) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE restricted_reason IS NOT NULL ORDER BY restricted_at LIMIT $1`, batch)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	meta := &commonv1.RequestMeta{Actor: "accounts.restriction_sweep"}
	for _, id := range ids {
		if err := s.liftRestriction(ctx, id, meta); err != nil {
			slog.WarnContext(ctx, "restriction re-evaluation failed", "account_id", id, "err", err)
		}
	}
	return len(ids), nil
}
