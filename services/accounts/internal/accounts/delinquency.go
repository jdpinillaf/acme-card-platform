package accounts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// Delinquency stages, decided by billing (ADR-019). charged_off is permanent and reached only
// through ChargeOff.
const (
	DelinquencyNone       = "none"
	DelinquencyPastDue    = "past_due"
	DelinquencyChargedOff = "charged_off"
)

// SetDelinquency records the stage billing derived (none | past_due). It never leaves charged_off.
func (s *Service) SetDelinquency(ctx context.Context, req *accountsv1.SetDelinquencyRequest) (*accountsv1.GetAccountResponse, error) {
	if req.Stage != DelinquencyNone && req.Stage != DelinquencyPastDue {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_stage", "stage must be none or past_due")
	}
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		acc = before
		if before.Delinquency == DelinquencyChargedOff {
			return grpcx.Error(codes.FailedPrecondition, "charged_off", "account is charged off")
		}
		if before.Delinquency == req.Stage {
			return nil
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET delinquency=$2, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols,
			before.ID, req.Stage)); err != nil {
			return err
		}
		ev := event("account.delinquency_changed", "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(before), snapshot(acc)
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.accountResponse(ctx, acc)
}

// CheckExposure restricts the card (over_limit) when Σ holds + receivable exceeds the limit, e.g.
// after billing posted interest or a fee. An existing restriction is kept as is.
func (s *Service) CheckExposure(ctx context.Context, req *accountsv1.CheckExposureRequest) (*accountsv1.GetAccountResponse, error) {
	acc, err := s.load(ctx, s.pool, req.AccountId, false)
	if err != nil {
		return nil, err
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	if v.Holds+v.Receivable > acc.CreditLimit {
		if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			return s.restrictInTx(ctx, tx, acc.ID, RestrictionOverLimit, req.Meta)
		}); err != nil {
			return nil, internal(err)
		}
	}
	return s.accountResponse(ctx, acc)
}

func (s *Service) accountResponse(ctx context.Context, acc accountRow) (*accountsv1.GetAccountResponse, error) {
	acc, err := s.load(ctx, s.pool, acc.ID, false)
	if err != nil {
		return nil, err
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	return &accountsv1.GetAccountResponse{Account: s.accountProto(acc, v)}, nil
}

// ChargeOff applies the collateral to the debt at 60 days past due and blocks the card for good
// (ADR-019). Saga: reserve (stage charged_off, business date, pending marker) → expire every
// approved authorization → fix the amount min(max(receivable,0), collateral) → ledger
// PostCollateralApplication("chargeoff:<id>") → finalize (collateral and limit drop). While a
// card operation is in flight it answers Unavailable and is retried; ResolvePending resumes it.
func (s *Service) ChargeOff(ctx context.Context, req *accountsv1.ChargeOffRequest) (*accountsv1.GetAccountResponse, error) {
	var acc accountRow
	var pending bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		acc = before
		if err := tx.QueryRow(ctx, `SELECT charge_off_pending_at IS NOT NULL FROM accounts WHERE id=$1`, before.ID).Scan(&pending); err != nil {
			return err
		}
		if before.Delinquency == DelinquencyChargedOff {
			return nil // done, or resume a pending one
		}
		if before.State != StateActive {
			return grpcx.Error(codes.FailedPrecondition, "not_active", "only active accounts can be charged off")
		}
		pending = true
		effective := s.accountNow(before)
		if at := req.GetMeta().GetEffectiveAt(); at != nil { // billing passes the DPD-60 business date
			effective = at.AsTime()
		}
		acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET delinquency='charged_off', charge_off_pending_at=$2,
			charge_off_effective_at=$3, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols, before.ID, s.clock.Now(), effective))
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	if !pending {
		return s.accountResponse(ctx, acc)
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := s.expireForChargeOff(ctx, acc.ID); err != nil {
		return nil, err
	}
	amount, effectiveAt, err := s.chargeOffAmount(ctx, acc)
	if err != nil {
		return nil, err
	}
	if amount > 0 {
		if _, err := s.ledger.PostCollateralApplication(ctx, &ledgerv1.PostCollateralApplicationRequest{AccountId: acc.ID, Reference: "chargeoff:" + acc.ID,
			AmountMinor: amount, Meta: withEffective(req.Meta, effectiveAt)}); err != nil {
			return nil, err // stays pending: a retry or ResolvePending resumes it
		}
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, acc.ID, true)
		if err != nil {
			return err
		}
		var still bool
		if err := tx.QueryRow(ctx, `SELECT charge_off_pending_at IS NOT NULL FROM accounts WHERE id=$1`, acc.ID).Scan(&still); err != nil || !still {
			acc = before
			return err // finished concurrently
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET collateral_minor = collateral_minor - $2, credit_limit_minor = credit_limit_minor - $2,
			charge_off_pending_at=NULL, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols, acc.ID, amount)); err != nil {
			return err
		}
		ev := event("account.charged_off", "account", acc.ID, req.Meta)
		ev.Before = snapshot(before)
		ev.After = snapshot(map[string]any{"account": acc, "collateral_applied_minor": amount})
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.accountResponse(ctx, acc)
}

// errOpInFlight makes the charge-off wait for card operations already under way, so a capture can
// never land after the collateral was applied.
var errOpInFlight = grpcx.Error(codes.Unavailable, "operation_in_flight", "a card operation is in flight; retry the charge-off")

// expireForChargeOff expires every approved authorization of the account (hold → expired).
func (s *Service) expireForChargeOff(ctx context.Context, accountID string) error {
	inFlight := func() error {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM authorizations WHERE account_id=$1 AND (status='pending' OR pending_op IS NOT NULL)`, accountID).Scan(&n); err != nil {
			return internal(err)
		}
		if n > 0 {
			return errOpInFlight
		}
		return nil
	}
	if err := inFlight(); err != nil {
		return err
	}
	for {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			a, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authCols+` FROM authorizations WHERE account_id=$1 AND status='approved' AND pending_op IS NULL
				LIMIT 1 FOR UPDATE`, accountID))
			if err != nil {
				return err
			}
			if _, err := s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{HoldReference: "auth:" + a.NetworkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED}); err != nil {
				return err
			}
			after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='expired' WHERE id=$1 RETURNING `+authCols, a.ID))
			if err != nil {
				return err
			}
			return s.emitAuth(ctx, tx, "authorization.expired", a, after, &commonv1.RequestMeta{Actor: "accounts.charge_off"})
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return inFlight() // a capture may have reserved between our scans
		}
		if err != nil {
			return internal(err)
		}
	}
}

// chargeOffAmount fixes the amount once, so a retry posts exactly the same journal. A payment
// settling in between can leave a small credit balance (accepted, ADR-019).
func (s *Service) chargeOffAmount(ctx context.Context, acc accountRow) (int64, *time.Time, error) {
	v, err := s.available(ctx, acc)
	if err != nil {
		return 0, nil, err
	}
	var amount int64
	var at *time.Time
	err = s.pool.QueryRow(ctx, `UPDATE accounts SET charge_off_amount = coalesce(charge_off_amount, LEAST(GREATEST($2::bigint, 0), collateral_minor))
		WHERE id=$1 RETURNING charge_off_amount, charge_off_effective_at`, acc.ID, v.Receivable).Scan(&amount, &at)
	if err != nil {
		return 0, nil, internal(err)
	}
	return amount, at, nil
}

// resolveChargeOffs resumes charge-offs left pending by a lost response or an operation in flight.
func (s *Service) resolveChargeOffs(ctx context.Context, cutoff time.Time) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE charge_off_pending_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		slog.ErrorContext(ctx, "charge-off reconciliation query failed", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.ChargeOff(ctx, &accountsv1.ChargeOffRequest{AccountId: id, Meta: &commonv1.RequestMeta{Actor: "accounts.reconciler"}}); err != nil {
			slog.WarnContext(ctx, "resuming charge-off failed; retried next pass", "account_id", id, "err", err)
		}
	}
}

// ListAccountIDs pages through active accounts (charged-off accounts stay active) for billing.
func (s *Service) ListAccountIDs(ctx context.Context, req *accountsv1.ListAccountIDsRequest) (*accountsv1.ListAccountIDsResponse, error) {
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE state='active' AND ($1 = '' OR id > $1::uuid) ORDER BY id LIMIT $2`, req.AfterId, limit)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	resp := &accountsv1.ListAccountIDsResponse{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, internal(err)
		}
		resp.Ids = append(resp.Ids, id)
	}
	return resp, internal0(rows.Err())
}

func internal0(err error) error {
	if err == nil {
		return nil
	}
	return internal(err)
}
