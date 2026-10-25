package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

const waiverCols = `id::text, charge_id::text, account_id::text, amount_minor, reason, actor, effective_at, status, created_at`

type waiverRow struct {
	ID, ChargeID, AccountID, Reason, Actor, Status string
	Amount                                         int64
	EffectiveAt, CreatedAt                         time.Time
}

func scanWaiver(row pgx.Row) (waiverRow, error) {
	var w waiverRow
	err := row.Scan(&w.ID, &w.ChargeID, &w.AccountID, &w.Amount, &w.Reason, &w.Actor, &w.EffectiveAt, &w.Status, &w.CreatedAt)
	return w, err
}

func (w waiverRow) proto() *billingv1.Waiver {
	return &billingv1.Waiver{Id: w.ID, ChargeId: w.ChargeID, AmountMinor: w.Amount, Reason: w.Reason, Actor: w.Actor, CreatedAt: timestamppb.New(w.CreatedAt)}
}

var waiverKinds = map[string]ledgerv1.ChargeKind{"interest": ledgerv1.ChargeKind_CHARGE_KIND_INTEREST_WAIVER, "fee": ledgerv1.ChargeKind_CHARGE_KIND_FEE_WAIVER}

// WaiveCharge reverses part or all of a posted interest or fee charge (support role, enforced at
// the gateway). Saga: lock the charge, check Σ waived + amount ≤ charged, reserve a pending waiver
// and bump waived_minor → ledger PostCharge(waiver:<id>) → mark posted, emit, then catch up (a
// waiver counts toward the minimum, so it can cure delinquency). Idempotent on the meta key.
func (s *Service) WaiveCharge(ctx context.Context, req *billingv1.WaiveChargeRequest) (*billingv1.WaiveChargeResponse, error) {
	if req.AmountMinor < 1 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be at least 1")
	}
	if n := utf8.RuneCountInString(req.Reason); n < 1 || n > 200 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_reason", "reason is required (1-200 characters)")
	}
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.ChargeId); err != nil {
		return nil, grpcx.Error(codes.NotFound, "charge_not_found", "charge not found")
	}
	acc, err := s.account(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	var keyHash *string
	actor := "support"
	if m := req.Meta; m != nil {
		if m.IdempotencyKeyHash != "" {
			keyHash = &m.IdempotencyKeyHash
		}
		if m.Actor != "" {
			actor = m.Actor
		}
	}
	var (
		c        chargeRow
		w        waiverRow
		replayed bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		c, err = scanCharge(tx.QueryRow(ctx, `SELECT `+chargeCols+` FROM charges WHERE id=$1 AND account_id=$2 FOR UPDATE`, req.ChargeId, req.AccountId))
		if errors.Is(err, pgx.ErrNoRows) {
			return grpcx.Error(codes.NotFound, "charge_not_found", "charge not found")
		}
		if err != nil {
			return err
		}
		if keyHash != nil {
			w, err = scanWaiver(tx.QueryRow(ctx, `SELECT `+waiverCols+` FROM waivers WHERE idempotency_key_hash=$1`, *keyHash))
			if err == nil {
				if w.ChargeID != c.ID || w.Amount != req.AmountMinor {
					return grpcx.Error(codes.AlreadyExists, "idempotency_key_reuse", "idempotency key already used for a different waiver")
				}
				replayed = true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if c.Status != "posted" {
			return grpcx.Error(codes.FailedPrecondition, "charge_not_posted", "only posted charges can be waived")
		}
		if c.Waived+req.AmountMinor > c.Amount {
			return grpcx.Error(codes.FailedPrecondition, "waiver_exceeds_charge", "waivers cannot exceed the charged amount")
		}
		w, err = scanWaiver(tx.QueryRow(ctx, `INSERT INTO waivers (id, charge_id, account_id, amount_minor, reason, actor, idempotency_key_hash, effective_at, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending') RETURNING `+waiverCols,
			uuid.Must(uuid.NewV7()), c.ID, c.AccountID, req.AmountMinor, req.Reason, actor, keyHash, acc.AccountTime.AsTime()))
		if err != nil {
			return err
		}
		c, err = scanCharge(tx.QueryRow(ctx, `UPDATE charges SET waived_minor = waived_minor + $2 WHERE id=$1 RETURNING `+chargeCols, c.ID, req.AmountMinor))
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	if !replayed {
		if err := s.finishWaiver(ctx, w, c.Kind); err != nil {
			return nil, err
		}
		if _, err := s.CatchUpAccount(ctx, req.AccountId); err != nil {
			slog.WarnContext(ctx, "catch-up after waiver failed; the sweep retries", "account_id", req.AccountId, "err", err)
		}
	}
	if c, err = scanCharge(s.pool.QueryRow(ctx, `SELECT `+chargeCols+` FROM charges WHERE id=$1`, c.ID)); err != nil {
		return nil, internal(err)
	}
	if w, err = scanWaiver(s.pool.QueryRow(ctx, `SELECT `+waiverCols+` FROM waivers WHERE id=$1`, w.ID)); err != nil {
		return nil, internal(err)
	}
	if w.Status == "failed" {
		return nil, grpcx.Error(codes.FailedPrecondition, "waiver_failed", "the ledger refused the waiver")
	}
	return &billingv1.WaiveChargeResponse{Charge: c.proto(), Waiver: w.proto(), Replayed: replayed}, nil
}

// finishWaiver posts a reserved waiver to the ledger and finalizes it. A permanent refusal undoes
// the reservation; a transient error leaves it pending for the next catch-up.
func (s *Service) finishWaiver(ctx context.Context, w waiverRow, kind string) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	_, err := s.ledger.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: w.AccountID, Reference: "waiver:" + w.ID, Kind: waiverKinds[kind],
		AmountMinor: w.Amount, Meta: &commonv1.RequestMeta{Actor: w.Actor, EffectiveAt: timestamppb.New(w.EffectiveAt)}})
	if err != nil && !terminal(err) {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err != nil {
			slog.ErrorContext(ctx, "ledger refused a waiver; reservation undone", "waiver_id", w.ID, "err", err)
			tag, uerr := tx.Exec(ctx, `UPDATE waivers SET status='failed' WHERE id=$1 AND status='pending'`, w.ID)
			if uerr != nil || tag.RowsAffected() == 0 {
				return uerr
			}
			_, uerr = tx.Exec(ctx, `UPDATE charges SET waived_minor = waived_minor - $2 WHERE id=$1`, w.ChargeID, w.Amount)
			return uerr
		}
		tag, uerr := tx.Exec(ctx, `UPDATE waivers SET status='posted' WHERE id=$1 AND status='pending'`, w.ID)
		if uerr != nil || tag.RowsAffected() == 0 {
			return uerr
		}
		w.Status = "posted"
		return outbox.Enqueue(ctx, tx, event("charge.waived", "charge", w.ChargeID, w))
	})
}

// resumeWaivers finishes waivers left pending by a lost ledger response.
func (s *Service) resumeWaivers(ctx context.Context, accountID string) error {
	rows, err := s.pool.Query(ctx, `SELECT `+waiverCols+`, (SELECT kind FROM charges c WHERE c.id = w.charge_id) FROM waivers w
		WHERE account_id=$1 AND status='pending'`, accountID)
	if err != nil {
		return internal(err)
	}
	type item struct {
		w    waiverRow
		kind string
	}
	var pending []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.w.ID, &it.w.ChargeID, &it.w.AccountID, &it.w.Amount, &it.w.Reason, &it.w.Actor, &it.w.EffectiveAt, &it.w.Status,
			&it.w.CreatedAt, &it.kind); err != nil {
			rows.Close()
			return internal(err)
		}
		pending = append(pending, it)
	}
	rows.Close()
	for _, it := range pending {
		if err := s.finishWaiver(ctx, it.w, it.kind); err != nil {
			return err
		}
	}
	return nil
}
