package accounts

import (
	"context"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// SetFrozen freezes or unfreezes the card. Freezing blocks only new authorizations.
// Repeating the current state is a no-op (no event).
func (s *Service) SetFrozen(ctx context.Context, req *accountsv1.SetFrozenRequest) (*accountsv1.GetAccountResponse, error) {
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		acc = before
		if before.Frozen == req.Frozen {
			return nil
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET frozen=$2, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols, req.AccountId, req.Frozen)); err != nil {
			return err
		}
		typ := "card.unfrozen"
		if req.Frozen {
			typ = "card.frozen"
		}
		ev := event(typ, "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(before), snapshot(acc)
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.withAvailable(ctx, acc)
}

// SetPurchaseLimit sets the customer's per-transaction cap. 0 = no cap; never above the credit limit.
func (s *Service) SetPurchaseLimit(ctx context.Context, req *accountsv1.SetPurchaseLimitRequest) (*accountsv1.GetAccountResponse, error) {
	if req.PurchaseLimitMinor < 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_purchase_limit", "purchase_limit_minor must be >= 0")
	}
	var acc accountRow
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		if before.State == StateClosed {
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		}
		if req.PurchaseLimitMinor > before.CreditLimit {
			return grpcx.Error(codes.InvalidArgument, "purchase_limit_above_credit_limit", "purchase limit cannot exceed the credit limit")
		}
		acc = before
		if before.PurchaseLimit == req.PurchaseLimitMinor {
			return nil
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET purchase_limit_minor=$2, version=version+1, updated_at=now() WHERE id=$1 RETURNING `+accountCols, req.AccountId, req.PurchaseLimitMinor)); err != nil {
			return err
		}
		ev := event("limits.updated", "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(before), snapshot(acc)
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.withAvailable(ctx, acc)
}

func (s *Service) withAvailable(ctx context.Context, acc accountRow) (*accountsv1.GetAccountResponse, error) {
	avail, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	return &accountsv1.GetAccountResponse{Account: s.accountProto(acc, avail)}, nil
}
