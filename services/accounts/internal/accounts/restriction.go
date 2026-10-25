package accounts

import (
	"context"

	"github.com/jackc/pgx/v5"

	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/outbox"
)

// RestrictionPaymentReturned is the system restriction set by a returned payment that leaves the
// account's exposure above its limit (accepted risk, ADR-018). The customer cannot lift it.
const RestrictionPaymentReturned = "payment_returned"

// applyRestriction reads the ledger exposure (outside any lock), then sets or lifts the restriction:
// it lifts whenever Σ holds + receivable ≤ credit limit; it sets only when allowSet (return path).
func (s *Service) applyRestriction(ctx context.Context, accountID string, allowSet bool, meta *commonv1.RequestMeta) error {
	acc, err := s.load(ctx, s.pool, accountID, false)
	if err != nil {
		return err
	}
	if acc.Restricted == nil && !allowSet {
		return nil // nothing to lift and not allowed to set: skip the ledger round trip
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return err
	}
	over := v.Holds+v.Receivable > acc.CreditLimit
	if over == (acc.Restricted != nil) || (over && !allowSet) {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, accountID, true)
		if err != nil {
			return err
		}
		var after accountRow
		typ := "card.unrestricted"
		if over {
			if before.Restricted != nil {
				return nil
			}
			typ = "card.restricted"
			after, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET restricted_reason=$2, restricted_at=$3, version=version+1, updated_at=now()
				WHERE id=$1 RETURNING `+accountCols, accountID, RestrictionPaymentReturned, s.clock.Now()))
		} else {
			if before.Restricted == nil {
				return nil
			}
			after, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET restricted_reason=NULL, restricted_at=NULL, version=version+1, updated_at=now()
				WHERE id=$1 RETURNING `+accountCols, accountID))
		}
		if err != nil {
			return err
		}
		ev := event(typ, "account", accountID, meta)
		ev.Before, ev.After = snapshot(before), snapshot(after)
		return outbox.Enqueue(ctx, tx, ev)
	})
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
	for _, id := range ids {
		if err := s.applyRestriction(ctx, id, false, &commonv1.RequestMeta{Actor: "accounts.restriction_sweep"}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
