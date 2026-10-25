package ledger

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// exposure returns credit limit (= collateral held) and current exposure
// (Σ active holds + max(receivable, 0)). Caller must hold the account mutex.
func exposure(ctx context.Context, tx pgx.Tx, accountID string) (limit, holds, receivable int64, err error) {
	err = tx.QueryRow(ctx, `
		SELECT
		  -(SELECT raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account='collateral_held'),
		  coalesce((SELECT sum(amount_minor) FROM holds WHERE account_id=$1 AND status='active'), 0),
		  (SELECT raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account='receivable')`, accountID).
		Scan(&limit, &holds, &receivable)
	return
}

// availableOf applies the strict-limit rule: a credit balance (negative receivable) never adds
// spending power.
func availableOf(limit, holds, receivable int64) int64 {
	return max(0, limit-max(receivable, 0)-holds)
}

func holdEvent(typ, holdID, accountID string, meta *commonv1.RequestMeta, after map[string]any) events.Event {
	ev := events.New(typ, "hold", holdID)
	if meta != nil {
		ev.Actor, ev.IdempotencyKeyHash = meta.Actor, meta.IdempotencyKeyHash
	}
	after["account_id"] = accountID
	ev.After, _ = json.Marshal(after)
	return ev
}

// PlaceHold reserves credit if Σ active holds + max(receivable,0) + amount ≤ limit.
func (s *Service) PlaceHold(ctx context.Context, req *ledgerv1.PlaceHoldRequest) (*ledgerv1.PlaceHoldResponse, error) {
	if req.Reference == "" || req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_hold", "reference and a positive amount are required")
	}
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	var resp *ledgerv1.PlaceHoldResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		currency, err := lockAccount(ctx, tx, req.AccountId)
		if err != nil {
			return err
		}
		if currency != req.Currency {
			return grpcx.Error(codes.InvalidArgument, "currency_mismatch", "hold currency differs from account currency")
		}
		limit, holds, receivable, err := exposure(ctx, tx, req.AccountId)
		if err != nil {
			return err
		}
		var existingID, existingAcc string
		var existingAmt int64
		err = tx.QueryRow(ctx, `SELECT id::text, account_id::text, amount_minor FROM holds WHERE reference=$1`, req.Reference).Scan(&existingID, &existingAcc, &existingAmt)
		if err == nil {
			if existingAcc != req.AccountId || existingAmt != req.AmountMinor {
				return grpcx.Error(codes.AlreadyExists, "hold_reference_reuse", "reference already used for a different hold")
			}
			resp = &ledgerv1.PlaceHoldResponse{HoldId: existingID, Approved: true, AvailableMinor: availableOf(limit, holds, receivable), Replayed: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if holds+max(receivable, 0)+req.AmountMinor > limit {
			resp = &ledgerv1.PlaceHoldResponse{Approved: false, AvailableMinor: availableOf(limit, holds, receivable)}
			return nil
		}
		id := uuid.Must(uuid.NewV7()).String()
		if _, err := tx.Exec(ctx, `INSERT INTO holds (id, account_id, reference, amount_minor, status) VALUES ($1,$2,$3,$4,'active')`,
			id, req.AccountId, req.Reference, req.AmountMinor); err != nil {
			return err
		}
		avail := availableOf(limit, holds+req.AmountMinor, receivable)
		if err := outbox.Enqueue(ctx, tx, holdEvent("ledger.hold_placed", id, req.AccountId, req.Meta,
			map[string]any{"reference": req.Reference, "amount_minor": req.AmountMinor, "available_minor": avail})); err != nil {
			return err
		}
		resp = &ledgerv1.PlaceHoldResponse{HoldId: id, Approved: true, AvailableMinor: avail}
		return nil
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

type holdRow struct {
	id, accountID, status string
	amount                int64
	captureRef            *string
}

// lockHold resolves the hold's account, takes the account mutex, then reads the hold.
func lockHold(ctx context.Context, tx pgx.Tx, reference string) (holdRow, error) {
	var h holdRow
	if err := tx.QueryRow(ctx, `SELECT account_id::text FROM holds WHERE reference=$1`, reference).Scan(&h.accountID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return h, grpcx.Error(codes.NotFound, "hold_not_found", "hold not found")
		}
		return h, err
	}
	if _, err := lockAccount(ctx, tx, h.accountID); err != nil {
		return h, err
	}
	err := tx.QueryRow(ctx, `SELECT id::text, status, amount_minor, capture_reference FROM holds WHERE reference=$1 FOR UPDATE`, reference).
		Scan(&h.id, &h.status, &h.amount, &h.captureRef)
	return h, err
}

// CaptureHold posts debit receivable / credit cash_clearing and ends the hold.
func (s *Service) CaptureHold(ctx context.Context, req *ledgerv1.CaptureHoldRequest) (*ledgerv1.PostJournalResponse, error) {
	if req.HoldReference == "" || req.JournalReference == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_capture", "hold_reference and journal_reference are required")
	}
	var resp *ledgerv1.PostJournalResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, req.HoldReference)
		if err != nil {
			return err
		}
		if h.status == "captured" && h.captureRef != nil && *h.captureRef == req.JournalReference {
			var id string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM journal_entries WHERE reference=$1`, req.JournalReference).Scan(&id); err != nil {
				return err
			}
			resp = &ledgerv1.PostJournalResponse{JournalEntryId: id, Replayed: true}
			return nil
		}
		if h.status != "active" {
			return grpcx.Error(codes.FailedPrecondition, "hold_not_active", "hold is "+h.status)
		}
		if req.AmountMinor <= 0 || req.AmountMinor > h.amount {
			return grpcx.Error(codes.InvalidArgument, "invalid_capture_amount", "capture must be positive and at most the held amount")
		}
		resp, err = postJournalTx(ctx, tx, &ledgerv1.PostJournalRequest{
			AccountId: h.accountID, Reference: req.JournalReference, EntryType: "purchase_captured", Meta: req.Meta,
			Postings: []*ledgerv1.Posting{
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: req.AmountMinor},
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING, Side: ledgerv1.Side_SIDE_CREDIT, AmountMinor: req.AmountMinor},
			},
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET status='captured', captured_minor=$2, capture_reference=$3, finished_at=now() WHERE id=$1`,
			h.id, req.AmountMinor, req.JournalReference); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, tx, holdEvent("ledger.hold_captured", h.id, h.accountID, req.Meta,
			map[string]any{"reference": req.HoldReference, "held_minor": h.amount, "captured_minor": req.AmountMinor, "journal_entry_id": resp.JournalEntryId}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

// ReleaseHold ends an active hold without posting.
func (s *Service) ReleaseHold(ctx context.Context, req *ledgerv1.ReleaseHoldRequest) (*ledgerv1.ReleaseHoldResponse, error) {
	var target string
	switch req.Reason {
	case ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED:
		target = "released"
	case ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED:
		target = "expired"
	default:
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_reason", "reason must be released or expired")
	}
	var resp *ledgerv1.ReleaseHoldResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		h, err := lockHold(ctx, tx, req.HoldReference)
		if err != nil {
			return err
		}
		if h.status == target {
			resp = &ledgerv1.ReleaseHoldResponse{Status: target, Replayed: true}
			return nil
		}
		if h.status != "active" {
			return grpcx.Error(codes.FailedPrecondition, "hold_not_active", "hold is "+h.status)
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET status=$2, finished_at=now() WHERE id=$1`, h.id, target); err != nil {
			return err
		}
		resp = &ledgerv1.ReleaseHoldResponse{Status: target}
		return outbox.Enqueue(ctx, tx, holdEvent("ledger.hold_released", h.id, h.accountID, req.Meta,
			map[string]any{"reference": req.HoldReference, "amount_minor": h.amount, "status": target}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

// PostRefund posts debit cash_clearing / credit receivable. The receivable may go negative
// (credit balance); per-purchase refund caps are enforced by accounts.
func (s *Service) PostRefund(ctx context.Context, req *ledgerv1.PostRefundRequest) (*ledgerv1.PostJournalResponse, error) {
	if req.Reference == "" || req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_refund", "reference and a positive amount are required")
	}
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "account_id must be a uuid")
	}
	var resp *ledgerv1.PostJournalResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockAccount(ctx, tx, req.AccountId); err != nil {
			return err
		}
		var err error
		resp, err = postJournalTx(ctx, tx, &ledgerv1.PostJournalRequest{
			AccountId: req.AccountId, Reference: req.Reference, EntryType: "purchase_refunded", Meta: req.Meta,
			Postings: []*ledgerv1.Posting{
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: req.AmountMinor},
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, Side: ledgerv1.Side_SIDE_CREDIT, AmountMinor: req.AmountMinor},
			},
		})
		if err != nil || resp.Replayed {
			return err
		}
		return outbox.Enqueue(ctx, tx, holdEvent("ledger.refund_posted", resp.JournalEntryId, req.AccountId, req.Meta,
			map[string]any{"reference": req.Reference, "amount_minor": req.AmountMinor}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}
