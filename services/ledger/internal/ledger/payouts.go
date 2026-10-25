package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// PostPayout releases money to the customer once the bank confirms a payout (Sprint 5): debit
// collateral_held for the collateral part and debit receivable for the credit-balance part, credit
// cash_clearing for the total. It never pays out more than is held.
func (s *Service) PostPayout(ctx context.Context, req *ledgerv1.PostPayoutRequest) (*ledgerv1.PostJournalResponse, error) {
	if req.Reference == "" || req.CollateralMinor < 0 || req.CreditMinor < 0 || req.CollateralMinor+req.CreditMinor == 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_payout", "reference and a positive amount are required")
	}
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	var resp *ledgerv1.PostJournalResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockAccount(ctx, tx, req.AccountId); err != nil {
			return err
		}
		var posted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_entries WHERE reference=$1)`, req.Reference).Scan(&posted); err != nil {
			return err
		}
		if !posted { // a replay is judged by postJournalTx, not by the balances left afterwards
			var coll, recv int64
			if err := tx.QueryRow(ctx, `SELECT
				(SELECT -raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account='collateral_held'),
				(SELECT raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account='receivable')`, req.AccountId).Scan(&coll, &recv); err != nil {
				return err
			}
			if req.CollateralMinor > coll || req.CreditMinor > max(-recv, 0) {
				return grpcx.Error(codes.FailedPrecondition, "insufficient_funds", "payout exceeds held collateral or credit balance")
			}
		}
		var postings []*ledgerv1.Posting
		if req.CollateralMinor > 0 {
			postings = append(postings, &ledgerv1.Posting{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: req.CollateralMinor})
		}
		if req.CreditMinor > 0 {
			postings = append(postings, &ledgerv1.Posting{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: req.CreditMinor})
		}
		postings = append(postings, &ledgerv1.Posting{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING, Side: ledgerv1.Side_SIDE_CREDIT,
			AmountMinor: req.CollateralMinor + req.CreditMinor})
		var err error
		resp, err = postJournalTx(ctx, tx, &ledgerv1.PostJournalRequest{AccountId: req.AccountId, Reference: req.Reference, EntryType: "payout_settled",
			Meta: req.Meta, Postings: postings})
		if err != nil || resp.Replayed {
			return err
		}
		return outbox.Enqueue(ctx, tx, holdEvent("ledger.payout_posted", resp.JournalEntryId, req.AccountId, req.Meta,
			map[string]any{"reference": req.Reference, "collateral_minor": req.CollateralMinor, "credit_minor": req.CreditMinor}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}
