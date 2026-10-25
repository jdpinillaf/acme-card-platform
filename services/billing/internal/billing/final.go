package billing

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

// FinalizeStatement cuts the off-cycle final statement of a closing account (ADR-020): regular
// cycles are caught up first, then [last close, account time) is closed with interest under the
// usual grace rules and a due date 21 days later. Nothing is billed after it. Idempotent.
func (s *Service) FinalizeStatement(ctx context.Context, req *billingv1.FinalizeStatementRequest) (*billingv1.FinalizeStatementResponse, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	if _, err := s.CatchUpAccount(ctx, req.AccountId); err != nil {
		return nil, err
	}
	release, err := s.acquireLease(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	defer release()
	acc, err := s.account(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	if acc.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING || acc.ActivatedAt == nil {
		return nil, grpcx.Error(codes.FailedPrecondition, "not_closing", "only a closing account gets a final statement")
	}
	r := &run{s: s, acc: acc, act: acc.ActivatedAt.AsTime(), now: acc.AccountTime.AsTime(), stage: acc.Delinquency,
		chargedOff: acc.Delinquency == "charged_off"}
	if err := r.load(ctx); err != nil {
		return nil, err
	}
	if n := len(r.stmts); n == 0 || !r.stmts[n-1].Final {
		if err := r.close(ctx, n+1, engine.CycleClose(r.act, n), r.now, true); err != nil {
			return nil, err
		}
	}
	final := r.stmts[len(r.stmts)-1]
	due := engine.DueDate(final.PeriodEnd)
	return &billingv1.FinalizeStatementResponse{Statement: final.proto(), DueDate: timestamppb.New(due)}, nil
}
