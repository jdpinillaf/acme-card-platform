package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// PostPayment posts a confirmed customer payment: debit cash_clearing / credit receivable.
func (s *Service) PostPayment(ctx context.Context, req *ledgerv1.PostPaymentRequest) (*ledgerv1.PostJournalResponse, error) {
	return s.postPaymentLeg(ctx, req.AccountId, req.Reference, req.AmountMinor, req.Meta, "payment_settled", "ledger.payment_posted",
		ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING, ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE)
}

// PostPaymentReturn reverses a returned payment: debit receivable / credit cash_clearing. The
// receivable may exceed the limit afterwards (accepted risk, ADR-018); PlaceHold then declines.
func (s *Service) PostPaymentReturn(ctx context.Context, req *ledgerv1.PostPaymentReturnRequest) (*ledgerv1.PostJournalResponse, error) {
	return s.postPaymentLeg(ctx, req.AccountId, req.Reference, req.AmountMinor, req.Meta, "payment_returned", "ledger.payment_returned",
		ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING)
}

func (s *Service) postPaymentLeg(ctx context.Context, accountID, reference string, amount int64, meta *commonv1.RequestMeta,
	entryType, eventType string, debit, credit ledgerv1.SubAccount) (*ledgerv1.PostJournalResponse, error) {
	if reference == "" || amount <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_payment", "reference and a positive amount are required")
	}
	if _, err := uuid.Parse(accountID); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	var resp *ledgerv1.PostJournalResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockAccount(ctx, tx, accountID); err != nil {
			return err
		}
		var err error
		resp, err = postJournalTx(ctx, tx, &ledgerv1.PostJournalRequest{
			AccountId: accountID, Reference: reference, EntryType: entryType, Meta: meta,
			Postings: []*ledgerv1.Posting{
				{SubAccount: debit, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: amount},
				{SubAccount: credit, Side: ledgerv1.Side_SIDE_CREDIT, AmountMinor: amount},
			},
		})
		if err != nil || resp.Replayed {
			return err
		}
		return outbox.Enqueue(ctx, tx, holdEvent(eventType, resp.JournalEntryId, accountID, meta,
			map[string]any{"reference": reference, "amount_minor": amount}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}
