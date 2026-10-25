package accounts

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// CancelAccount ends an account that never activated (ADR-020): instant, and every pending deposit
// is voided, so a late bank settlement is rejected and no money is ever recorded.
func (s *Service) CancelAccount(ctx context.Context, req *accountsv1.CancelAccountRequest) (*accountsv1.GetAccountResponse, error) {
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		acc = before
		if before.State == StateCancelled {
			return nil // replay
		}
		next, err := Transition(before.State, EventCancel)
		if err != nil {
			return grpcx.Error(codes.FailedPrecondition, "not_cancellable", "only accounts waiting for collateral can be cancelled")
		}
		var settling int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deposits WHERE account_id=$1 AND status='settling'`, before.ID).Scan(&settling); err != nil {
			return err
		}
		if settling > 0 {
			return grpcx.Error(codes.FailedPrecondition, "deposit_settling", "a deposit is being settled; retry shortly")
		}
		if err := s.voidPendingDeposits(ctx, tx, before.ID, req.Meta); err != nil {
			return err
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET state=$2, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols,
			before.ID, next)); err != nil {
			return err
		}
		ev := event("account.cancelled", "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(before), snapshot(acc)
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return &accountsv1.GetAccountResponse{Account: s.accountProto(acc, ledgerView{})}, nil
}

// voidPendingDeposits voids every deposit the bank has not answered yet (cancel and close).
func (s *Service) voidPendingDeposits(ctx context.Context, tx pgx.Tx, accountID string, meta *commonv1.RequestMeta) error {
	rows, err := tx.Query(ctx, `UPDATE deposits SET status='voided' WHERE account_id=$1 AND status='pending' RETURNING id::text, amount_minor`, accountID)
	if err != nil {
		return err
	}
	type voided struct {
		id     string
		amount int64
	}
	var vs []voided
	for rows.Next() {
		var v voided
		if err := rows.Scan(&v.id, &v.amount); err != nil {
			rows.Close()
			return err
		}
		vs = append(vs, v)
	}
	rows.Close()
	for _, v := range vs {
		ev := event("deposit.voided", "deposit", v.id, meta)
		ev.After = snapshot(map[string]any{"account_id": accountID, "amount_minor": v.amount, "status": "voided"})
		if err := outbox.Enqueue(ctx, tx, ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

// checkPreviousDebt refuses a new account while a closed account of the same customer and
// currency still owes money (ADR-020); it is allowed again once that debt is paid.
func (s *Service) checkPreviousDebt(ctx context.Context, customerID, currency string) error {
	rows, err := s.pool.Query(ctx, `SELECT `+accountCols+` FROM accounts WHERE customer_id=$1 AND currency=$2 AND state='closed'`, customerID, currency)
	if err != nil {
		return internal(err)
	}
	var closed []accountRow
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return internal(err)
		}
		closed = append(closed, a)
	}
	rows.Close()
	for _, a := range closed {
		v, err := s.available(ctx, a)
		if err != nil {
			return err
		}
		if v.Receivable > 0 {
			return grpcx.Error(codes.FailedPrecondition, "previous_account_has_debt", "a closed account in this currency still has an outstanding balance")
		}
	}
	return internal0(errors.Join(rows.Err()))
}
