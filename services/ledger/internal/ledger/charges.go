package ledger

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// chargeLegs maps a billing charge kind to its entry type and (debit, credit) sub-accounts.
var chargeLegs = map[ledgerv1.ChargeKind]struct {
	entryType     string
	debit, credit ledgerv1.SubAccount
}{
	ledgerv1.ChargeKind_CHARGE_KIND_INTEREST:        {"interest_charged", ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME},
	ledgerv1.ChargeKind_CHARGE_KIND_FEE:             {"fee_charged", ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME},
	ledgerv1.ChargeKind_CHARGE_KIND_INTEREST_WAIVER: {"interest_waived", ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME, ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE},
	ledgerv1.ChargeKind_CHARGE_KIND_FEE_WAIVER:      {"fee_waived", ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME, ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE},
}

// PostCharge posts interest, a fee, or a waiver of either (Sprint 4). Charges may push receivable
// above the limit (accepted, ADR-019); accounts restricts the card in that case.
func (s *Service) PostCharge(ctx context.Context, req *ledgerv1.PostChargeRequest) (*ledgerv1.PostJournalResponse, error) {
	legs, ok := chargeLegs[req.Kind]
	if !ok {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_charge", "unknown charge kind")
	}
	return s.postPaymentLeg(ctx, req.AccountId, req.Reference, req.AmountMinor, req.Meta, legs.entryType, "ledger.charge_posted", legs.debit, legs.credit)
}

// PostCollateralApplication applies held collateral to the debt at charge-off: debit
// collateral_held / credit receivable. It never applies more collateral than is held.
func (s *Service) PostCollateralApplication(ctx context.Context, req *ledgerv1.PostCollateralApplicationRequest) (*ledgerv1.PostJournalResponse, error) {
	if req.Reference == "" || req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_application", "reference and a positive amount are required")
	}
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	var resp *ledgerv1.PostJournalResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockAccount(ctx, tx, req.AccountId); err != nil {
			return err
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_entries WHERE reference=$1)`, req.Reference).Scan(&applied); err != nil {
			return err
		}
		if !applied { // a replay is judged by postJournalTx, not by the collateral left afterwards
			var raw int64
			if err := tx.QueryRow(ctx, `SELECT raw_balance_minor FROM balances WHERE account_id=$1 AND sub_account='collateral_held'`, req.AccountId).Scan(&raw); err != nil {
				return err
			}
			if req.AmountMinor > -raw {
				return grpcx.Error(codes.FailedPrecondition, "insufficient_collateral", "amount exceeds held collateral")
			}
		}
		var err error
		resp, err = postJournalTx(ctx, tx, &ledgerv1.PostJournalRequest{
			AccountId: req.AccountId, Reference: req.Reference, EntryType: "collateral_applied", Meta: req.Meta,
			Postings: []*ledgerv1.Posting{
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD, Side: ledgerv1.Side_SIDE_DEBIT, AmountMinor: req.AmountMinor},
				{SubAccount: ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE, Side: ledgerv1.Side_SIDE_CREDIT, AmountMinor: req.AmountMinor},
			},
		})
		if err != nil || resp.Replayed {
			return err
		}
		return outbox.Enqueue(ctx, tx, holdEvent("ledger.collateral_applied", resp.JournalEntryId, req.AccountId, req.Meta,
			map[string]any{"reference": req.Reference, "amount_minor": req.AmountMinor}))
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return resp, nil
}

// SealPeriod raises the account's sealed_through watermark (monotonic) under the account mutex.
func (s *Service) SealPeriod(ctx context.Context, req *ledgerv1.SealPeriodRequest) (*ledgerv1.SealPeriodResponse, error) {
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	if req.SealedThrough == nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_seal", "sealed_through is required")
	}
	var sealed time.Time
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockAccount(ctx, tx, req.AccountId); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO account_seals (account_id, sealed_through) VALUES ($1,$2)
			ON CONFLICT (account_id) DO UPDATE SET sealed_through = GREATEST(account_seals.sealed_through, EXCLUDED.sealed_through)
			RETURNING sealed_through`, req.AccountId, req.SealedThrough.AsTime()).Scan(&sealed)
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &ledgerv1.SealPeriodResponse{SealedThrough: timestamppb.New(sealed)}, nil
}

const maxEntriesPage = 1000

// ListEntries pages an account's journal entries in (effective_at, id) order, each with its signed
// receivable movement (debit +, credit −).
func (s *Service) ListEntries(ctx context.Context, req *ledgerv1.ListEntriesRequest) (*ledgerv1.ListEntriesResponse, error) {
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_account", "account_id must be a uuid")
	}
	size := int(req.PageSize)
	if size <= 0 || size > maxEntriesPage {
		size = maxEntriesPage
	}
	var from, to *time.Time
	if req.EffectiveFrom != nil {
		t := req.EffectiveFrom.AsTime()
		from = &t
	}
	if req.EffectiveTo != nil {
		t := req.EffectiveTo.AsTime()
		to = &t
	}
	var afterAt *time.Time
	var afterID *uuid.UUID
	if req.PageToken != "" {
		at, id, err := decodeCursor(req.PageToken)
		if err != nil {
			return nil, grpcx.Error(codes.InvalidArgument, "invalid_page_token", "malformed page token")
		}
		afterAt, afterID = &at, &id
	}
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.reference, j.entry_type, j.effective_at,
		       coalesce(sum(CASE WHEN p.side='debit' THEN p.amount_minor ELSE -p.amount_minor END) FILTER (WHERE p.sub_account='receivable'), 0)
		FROM journal_entries j JOIN postings p ON p.journal_entry_id = j.id
		WHERE j.account_id = $1
		  AND ($2::timestamptz IS NULL OR j.effective_at >= $2)
		  AND ($3::timestamptz IS NULL OR j.effective_at < $3)
		  AND ($4::timestamptz IS NULL OR (j.effective_at, j.id) > ($4, $5::uuid))
		GROUP BY j.id ORDER BY j.effective_at, j.id LIMIT $6`, req.AccountId, from, to, afterAt, afterID, size+1)
	if err != nil {
		return nil, toStatus(err)
	}
	defer rows.Close()
	resp := &ledgerv1.ListEntriesResponse{}
	type last struct {
		at time.Time
		id uuid.UUID
	}
	var cursors []last
	for rows.Next() {
		var id uuid.UUID
		var e ledgerv1.JournalEntry
		var at time.Time
		if err := rows.Scan(&id, &e.Reference, &e.EntryType, &at, &e.ReceivableDeltaMinor); err != nil {
			return nil, toStatus(err)
		}
		e.Id, e.EffectiveAt = id.String(), timestamppb.New(at)
		resp.Entries = append(resp.Entries, &e)
		cursors = append(cursors, last{at, id})
	}
	if err := rows.Err(); err != nil {
		return nil, toStatus(err)
	}
	if len(resp.Entries) > size {
		resp.Entries = resp.Entries[:size]
		c := cursors[size-1]
		resp.NextPageToken = encodeCursor(c.at, c.id)
	}
	return resp, nil
}

func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s", at.UnixMicro(), id)))
}

func decodeCursor(tok string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, errors.New("bad cursor")
	}
	var us int64
	if _, err := fmt.Sscanf(at, "%d", &us); err != nil {
		return time.Time{}, uuid.Nil, err
	}
	u, err := uuid.Parse(id)
	return time.UnixMicro(us).UTC(), u, err
}
