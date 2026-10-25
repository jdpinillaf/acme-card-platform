package accounts

import (
	"context"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

// ReturnPayment applies a bank return of a settled payment within ReturnWindow: reserve
// (settled → returning, claim return_ref) → ledger PostPaymentReturn ("payment_return:<id>",
// detached) → finalize (returned, card restricted in the same transaction). Afterwards the
// restriction is lifted at once if exposure is still within the limit (accepted risk, ADR-018).
func (s *Service) ReturnPayment(ctx context.Context, req *accountsv1.ReturnPaymentRequest) (*accountsv1.PaymentResponse, error) {
	if req.ReturnRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_return_ref", "return_ref is required")
	}
	var (
		p        paymentRow
		done     bool
		replayed bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if p, err = lockPayment(ctx, tx, req.PaymentId); err != nil {
			return err
		}
		if strOr(p.ReturnRef) == req.ReturnRef {
			if req.AmountMinor != p.Amount {
				return grpcx.Error(codes.InvalidArgument, "amount_mismatch", "returned amount differs from payment amount")
			}
			if p.Status == "returned" {
				done, replayed = true, true
			}
			return nil // returned: replay; returning: resume
		}
		switch p.Status {
		case "settled":
		case "returning", "returned":
			return grpcx.Error(codes.AlreadyExists, "payment_already_returned", "payment already returned with a different return_ref")
		default:
			return grpcx.Error(codes.FailedPrecondition, "not_settled", "only settled payments can be returned")
		}
		if s.clock.Now().Sub(*p.SettledAt) > ReturnWindow {
			return grpcx.Error(codes.FailedPrecondition, "return_window_expired", "returns are only accepted within 5 days of settlement")
		}
		if req.AmountMinor != p.Amount {
			return grpcx.Error(codes.InvalidArgument, "amount_mismatch", "returned amount differs from payment amount")
		}
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='returning', return_ref=$2, returning_at=$3,
			return_effective_at=`+acctNowSQL("$3", "payments.account_id")+` WHERE id=$1 RETURNING `+paymentCols,
			p.ID, req.ReturnRef, s.clock.Now()))
		return err
	})
	if err != nil {
		return nil, mapPaymentErr(err, "return_ref_reused")
	}
	if done {
		return s.paymentResponse(ctx, p, replayed)
	}

	ctx, cancel := detached(ctx)
	defer cancel()
	jr, err := s.ledger.PostPaymentReturn(ctx, &ledgerv1.PostPaymentReturnRequest{AccountId: p.AccountID, Reference: "payment_return:" + p.ID, AmountMinor: p.Amount, Meta: withEffective(req.Meta, p.ReturnAt)})
	if err != nil {
		return nil, err // stays returning: a retry or ResolvePending resumes it
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockPayment(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if cur.Status != "returning" {
			p, replayed = cur, true
			return nil
		}
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='returned', returned_at=$2, return_journal_id=$3 WHERE id=$1 RETURNING `+paymentCols,
			cur.ID, s.clock.Now(), jr.JournalEntryId))
		if err != nil {
			return err
		}
		if err := s.restrictInTx(ctx, tx, p.AccountID, RestrictionPaymentReturned, req.Meta); err != nil {
			return err
		}
		return emitPayment(ctx, tx, "payment.returned", cur, p, req.Meta)
	})
	if err != nil {
		return nil, internal(err)
	}
	// The return restricted the card fail-safe; lift it right away if exposure is within the limit.
	s.liftQuietly(ctx, p.AccountID, req.Meta)
	return s.paymentResponse(ctx, p, replayed)
}
