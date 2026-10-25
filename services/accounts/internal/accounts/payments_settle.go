package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

func lockPayment(ctx context.Context, tx pgx.Tx, id string) (paymentRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return paymentRow{}, grpcx.Error(codes.InvalidArgument, "invalid_payment", "payment_id must be a uuid")
	}
	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, grpcx.Error(codes.NotFound, "payment_not_found", "payment not found")
	}
	return p, err
}

func mapPaymentErr(err error, refReason string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return grpcx.Error(codes.AlreadyExists, refReason, "reference already used by another payment")
	}
	return internal(err)
}

func emitPayment(ctx context.Context, tx pgx.Tx, typ string, before, after paymentRow, meta *commonv1.RequestMeta) error {
	ev := event(typ, "payment", after.ID, meta)
	ev.Before, ev.After = snapshot(before), snapshot(after)
	return outbox.Enqueue(ctx, tx, ev)
}

// SettlePayment applies the bank's answer to a payment intent. 'failed' is final with no balance
// effect. 'settled' runs reserve (pending → settling, claim external_ref) → ledger PostPayment
// ("payment:<id>", detached from the caller) → finalize (settled), then lifts a payment_returned
// restriction if exposure is back within the limit. Replays by external_ref; the reconciler
// resumes settlements whose response was lost.
func (s *Service) SettlePayment(ctx context.Context, req *accountsv1.SettlePaymentRequest) (*accountsv1.PaymentResponse, error) {
	if req.ExternalRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_external_ref", "external_ref is required")
	}
	if req.Status != paySettledStatus && req.Status != payFailedStatus {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_status", "status must be settled or failed")
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
		switch p.Status {
		case "expired":
			// The bank moved the money after our intent expired (review I2): a settlement must still
			// be applied (any excess over the debt becomes a credit balance, as in spec I-2). A
			// failure notice for an expired intent has nothing left to do.
			if req.Status != paySettledStatus {
				return grpcx.Error(codes.FailedPrecondition, "payment_expired", "payment intent expired")
			}
		case "settled", "failed", "returning", "returned":
			done, replayed = true, true
			return settleReplayConflict(p, req)
		case "settling":
			if req.Status == paySettledStatus && strOr(p.ExternalRef) == req.ExternalRef {
				return nil // resume
			}
			return grpcx.Error(codes.FailedPrecondition, "payment_settling", "payment settlement already in progress")
		}
		if req.AmountMinor != p.Amount {
			return grpcx.Error(codes.InvalidArgument, "amount_mismatch", "settled amount differs from payment amount")
		}
		if req.Currency != p.Currency {
			return grpcx.Error(codes.InvalidArgument, "currency_mismatch", "settled currency differs from payment currency")
		}
		now := s.clock.Now()
		if req.Status == payFailedStatus {
			after, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='failed', external_ref=$2, settled_at=$3 WHERE id=$1 RETURNING `+paymentCols, p.ID, req.ExternalRef, now))
			if err != nil {
				return err
			}
			before := p
			p, done = after, true
			return emitPayment(ctx, tx, "payment.failed", before, after, req.Meta)
		}
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='settling', external_ref=$2, settling_at=$3 WHERE id=$1 RETURNING `+paymentCols, p.ID, req.ExternalRef, now))
		return err
	})
	if err != nil {
		return nil, mapPaymentErr(err, "external_ref_reused")
	}
	if done {
		return s.paymentResponse(ctx, p, replayed)
	}

	ctx, cancel := detached(ctx)
	defer cancel()
	jr, err := s.ledger.PostPayment(ctx, &ledgerv1.PostPaymentRequest{AccountId: p.AccountID, Reference: "payment:" + p.ID, AmountMinor: p.Amount, Meta: req.Meta})
	if err != nil {
		return nil, err // stays settling: a retry or ResolvePending resumes it
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockPayment(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if cur.Status != "settling" { // finished concurrently
			p, replayed = cur, true
			return nil
		}
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='settled', settled_at=$2, journal_entry_id=$3 WHERE id=$1 RETURNING `+paymentCols,
			cur.ID, s.clock.Now(), jr.JournalEntryId))
		if err != nil {
			return err
		}
		return emitPayment(ctx, tx, "payment.settled", cur, p, req.Meta)
	})
	if err != nil {
		return nil, internal(err)
	}
	if err := s.applyRestriction(ctx, p.AccountID, false, req.Meta); err != nil {
		return nil, err
	}
	return s.paymentResponse(ctx, p, replayed)
}

const (
	paySettledStatus = accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED
	payFailedStatus  = accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED
)

func settleReplayConflict(p paymentRow, req *accountsv1.SettlePaymentRequest) error {
	wantFinal := "settled"
	if req.Status == payFailedStatus {
		wantFinal = "failed"
	}
	sameRef := strOr(p.ExternalRef) == req.ExternalRef
	switch {
	case sameRef && p.Status == wantFinal:
		return nil
	case sameRef && wantFinal == "settled" && (p.Status == "returning" || p.Status == "returned"):
		return nil // settled earlier, returned since: the settlement itself replays
	case p.Status == "failed":
		return grpcx.Error(codes.FailedPrecondition, "payment_failed", "payment already failed")
	case p.Status == "expired":
		return grpcx.Error(codes.FailedPrecondition, "payment_expired", "payment intent expired")
	default:
		return grpcx.Error(codes.AlreadyExists, "payment_already_settled", "payment already settled with a different external_ref")
	}
}

// resolvePayments resumes settlements and returns left half-done by a lost response.
func (s *Service) resolvePayments(ctx context.Context, cutoff time.Time) error {
	rows, err := s.pool.Query(ctx, `SELECT `+paymentCols+` FROM payments
		WHERE (status='settling' AND settling_at < $1) OR (status='returning' AND returning_at < $1) LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	var stuck []paymentRow
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			rows.Close()
			return err
		}
		stuck = append(stuck, p)
	}
	rows.Close()
	meta := &commonv1.RequestMeta{Actor: "accounts.reconciler"}
	for _, p := range stuck {
		var err error
		if p.Status == "settling" {
			_, err = s.SettlePayment(ctx, &accountsv1.SettlePaymentRequest{PaymentId: p.ID, ExternalRef: strOr(p.ExternalRef), AmountMinor: p.Amount,
				Currency: p.Currency, Status: paySettledStatus, Meta: meta})
		} else {
			_, err = s.ReturnPayment(ctx, &accountsv1.ReturnPaymentRequest{PaymentId: p.ID, ReturnRef: strOr(p.ReturnRef), AmountMinor: p.Amount, Meta: meta})
		}
		if err != nil && status.Code(err) != codes.FailedPrecondition {
			return err
		}
	}
	return nil
}
