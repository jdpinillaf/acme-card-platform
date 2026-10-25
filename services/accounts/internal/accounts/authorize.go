package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/money"
	"github.com/acme/acme-card/pkg/outbox"
)

const foreignKeyViolation = "23503"

const (
	// HoldTTL is how long an uncaptured authorization reserves credit.
	HoldTTL = 7 * 24 * time.Hour
	// VelocityLimit attempts per VelocityWindow per card (every attempt counts).
	VelocityLimit  = 10
	VelocityWindow = time.Minute
)

var declineProto = map[DeclineReason]commonv1.DeclineReason{
	DeclineAccountClosed:          commonv1.DeclineReason_DECLINE_REASON_ACCOUNT_CLOSED,
	DeclineCollateralNotConfirmed: commonv1.DeclineReason_DECLINE_REASON_COLLATERAL_NOT_CONFIRMED,
	DeclineCurrencyNotSupported:   commonv1.DeclineReason_DECLINE_REASON_CURRENCY_NOT_SUPPORTED,
	DeclineCardFrozen:             commonv1.DeclineReason_DECLINE_REASON_CARD_FROZEN,
	DeclineVelocity:               commonv1.DeclineReason_DECLINE_REASON_VELOCITY_LIMIT_EXCEEDED,
	DeclinePurchaseLimit:          commonv1.DeclineReason_DECLINE_REASON_PURCHASE_LIMIT_EXCEEDED,
	DeclineInsufficientCredit:     commonv1.DeclineReason_DECLINE_REASON_INSUFFICIENT_CREDIT,
}

var authStatusProto = map[string]accountsv1.AuthorizationStatus{
	"approved": accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED,
	"declined": accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED,
	"captured": accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED,
	"reversed": accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_REVERSED,
	"expired":  accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_EXPIRED,
	"pending":  accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_PENDING,
	"failed":   accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED,
}

type authRow struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	NetworkRef    string     `json:"network_ref"`
	RequestHash   string     `json:"-"`
	Amount        int64      `json:"amount_minor"`
	Currency      string     `json:"currency"`
	MerchantID    string     `json:"merchant_id"`
	MerchantName  string     `json:"merchant_name"`
	MCC           string     `json:"mcc"`
	Status        string     `json:"status"`
	DeclineReason *string    `json:"decline_reason,omitempty"`
	Captured      int64      `json:"captured_minor"`
	Refunded      int64      `json:"refunded_minor"`
	CaptureRef    *string    `json:"capture_network_ref,omitempty"`
	ReversalRef   *string    `json:"reversal_network_ref,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
	ReversedAt    *time.Time `json:"reversed_at,omitempty"`
	PendingOp     *string    `json:"pending_op,omitempty"`
	PendingAmount *int64     `json:"pending_op_amount,omitempty"`
	PendingAt     *time.Time `json:"pending_op_at,omitempty"`
}

const authCols = `id::text, account_id::text, network_ref, request_hash, amount_minor, currency, merchant_id, merchant_name, mcc,
	status, decline_reason, captured_minor, refunded_minor, capture_network_ref, reversal_network_ref,
	created_at, expires_at, captured_at, reversed_at, pending_op, pending_op_amount, pending_op_at`

func scanAuth(row pgx.Row) (authRow, error) {
	var a authRow
	err := row.Scan(&a.ID, &a.AccountID, &a.NetworkRef, &a.RequestHash, &a.Amount, &a.Currency, &a.MerchantID, &a.MerchantName, &a.MCC,
		&a.Status, &a.DeclineReason, &a.Captured, &a.Refunded, &a.CaptureRef, &a.ReversalRef,
		&a.CreatedAt, &a.ExpiresAt, &a.CapturedAt, &a.ReversedAt, &a.PendingOp, &a.PendingAmount, &a.PendingAt)
	return a, err
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func (a authRow) proto() *accountsv1.Authorization {
	p := &accountsv1.Authorization{
		Id: a.ID, AccountId: a.AccountID, NetworkRef: a.NetworkRef,
		Amount:   &commonv1.Money{AmountMinor: a.Amount, Currency: a.Currency},
		Merchant: &accountsv1.Merchant{Id: a.MerchantID, Name: a.MerchantName, Mcc: a.MCC},
		Status:   authStatusProto[a.Status], CapturedAmountMinor: a.Captured, RefundedAmountMinor: a.Refunded,
		CreatedAt: timestamppb.New(a.CreatedAt), ExpiresAt: tsp(a.ExpiresAt), CapturedAt: tsp(a.CapturedAt), ReversedAt: tsp(a.ReversedAt),
	}
	if a.DeclineReason != nil {
		p.DeclineReason = declineProto[DeclineReason(*a.DeclineReason)]
	}
	return p
}

func hashParts(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(sum[:])
}

func validateAuthorize(req *accountsv1.AuthorizeRequest) error {
	m := req.Merchant
	switch {
	case req.NetworkRef == "" || len(req.NetworkRef) > 128:
		return grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required (max 128)")
	case req.AmountMinor <= 0 || req.AmountMinor > MaxDepositMinor:
		return grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be positive and within limits")
	case !money.ValidCurrency(req.Currency):
		return grpcx.Error(codes.InvalidArgument, "invalid_currency", "currency must be an ISO-4217 code")
	case m == nil || m.Id == "" || m.Name == "" || m.Mcc == "" || len(m.Id) > 64 || len(m.Name) > 128 || len(m.Mcc) > 4:
		return grpcx.Error(codes.InvalidArgument, "invalid_merchant", "merchant id (≤64), name (≤128) and mcc (≤4) are required")
	}
	if _, err := uuid.Parse(req.CardId); err != nil {
		return grpcx.Error(codes.InvalidArgument, "invalid_card", "card_id must be a uuid")
	}
	return nil
}

// PendingTimeout is how long a reserved operation may stay unfinished before the reconciler acts.
const PendingTimeout = 2 * time.Minute

// Authorize evaluates a card network authorization in three steps:
//
//  1. reserve (short tx): insert-first on network_ref, count velocity, read the account FOR SHARE,
//     evaluate policy. A policy decline is final here. Otherwise the row commits as 'pending'.
//  2. ledger PlaceHold("auth:<network_ref>") — outside any accounts lock.
//  3. finalize (short tx): pending → approved | declined(insufficient_credit).
//
// A duplicate that finds the row 'pending' resumes steps 2–3 (the ledger call is idempotent). A
// row left pending by a lost response is failed by ResolvePending and its hold released.
func (s *Service) Authorize(ctx context.Context, req *accountsv1.AuthorizeRequest) (*accountsv1.AuthorizationResponse, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}
	reqHash := hashParts(req.CardId, req.AmountMinor, req.Currency, req.Merchant.Id, req.Merchant.Name, req.Merchant.Mcc)
	now := s.clock.Now()

	var (
		row   authRow
		final *accountsv1.AuthorizationResponse
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id := uuid.Must(uuid.NewV7()).String()
		inserted, err := scanAuth(tx.QueryRow(ctx, `
			INSERT INTO authorizations (id, account_id, network_ref, request_hash, amount_minor, currency, merchant_id, merchant_name, mcc, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending',$10) ON CONFLICT (network_ref) DO NOTHING
			RETURNING `+authCols,
			id, req.CardId, req.NetworkRef, reqHash, req.AmountMinor, req.Currency, req.Merchant.Id, req.Merchant.Name, req.Merchant.Mcc, now))
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authCols+` FROM authorizations WHERE network_ref=$1`, req.NetworkRef))
			if err != nil {
				return err
			}
			if existing.RequestHash != reqHash {
				return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "network_ref already used for a different authorization")
			}
			if existing.Status != "pending" {
				final = &accountsv1.AuthorizationResponse{Authorization: existing.proto(), Replayed: true}
			}
			row = existing
			return nil
		}
		if err != nil {
			return err // includes an unknown card: the account foreign key fails
		}

		// Every attempt that reaches here counts toward velocity, whatever the outcome.
		velocityOK, _, err := s.velocity.AllowTx(ctx, tx, "card:"+req.CardId, now)
		if err != nil {
			return err
		}
		// Read the account inside the tx so a freeze/limit committed before us is honored.
		acc, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id=$1 FOR SHARE`, req.CardId))
		if err != nil {
			return err
		}
		reason := EvaluateDecline(DeclineInput{State: acc.State, Frozen: acc.Frozen, AccountCurrency: acc.Currency, Currency: req.Currency,
			VelocityOK: velocityOK, PurchaseLimit: acc.PurchaseLimit, Amount: req.AmountMinor})
		if reason == "" {
			row = inserted
			return nil
		}
		declined, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='declined', decline_reason=$2 WHERE id=$1 RETURNING `+authCols, id, string(reason)))
		if err != nil {
			return err
		}
		ev := event("authorization.declined", "authorization", declined.ID, req.Meta)
		ev.After = snapshot(declined)
		final = &accountsv1.AuthorizationResponse{Authorization: declined.proto()}
		return outbox.Enqueue(ctx, tx, ev)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return nil, grpcx.Error(codes.NotFound, "card_not_found", "card not found")
	}
	if err != nil {
		return nil, internal(err)
	}
	if final != nil {
		return final, nil
	}
	dctx, cancel := detached(ctx)
	defer cancel()
	return s.finishAuthorization(dctx, row, req.Meta)
}

// finishAuthorization places the hold and finalizes a pending authorization.
func (s *Service) finishAuthorization(ctx context.Context, a authRow, meta *commonv1.RequestMeta) (*accountsv1.AuthorizationResponse, error) {
	hold, err := s.ledger.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{
		AccountId: a.AccountID, Reference: "auth:" + a.NetworkRef, AmountMinor: a.Amount, Currency: a.Currency, Meta: meta})
	if err != nil {
		return nil, err // row stays pending: a retry resumes, or ResolvePending fails it
	}
	var (
		resp    *accountsv1.AuthorizationResponse
		release bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockAuth(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if cur.Status != "pending" { // finalized concurrently (or failed by the reconciler)
			release = hold.Approved && cur.Status != "approved" && cur.Status != "captured"
			resp = &accountsv1.AuthorizationResponse{Authorization: cur.proto(), Replayed: true}
			return nil
		}
		var after authRow
		typ := "authorization.approved"
		if hold.Approved {
			after, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='approved', expires_at=$2 WHERE id=$1 RETURNING `+authCols, a.ID, cur.CreatedAt.Add(HoldTTL)))
		} else {
			typ = "authorization.declined"
			after, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='declined', decline_reason=$2 WHERE id=$1 RETURNING `+authCols, a.ID, string(DeclineInsufficientCredit)))
		}
		if err != nil {
			return err
		}
		ev := event(typ, "authorization", after.ID, meta)
		ev.After = snapshot(after)
		// Whoever performs the transition answers fresh; every other copy is a replay. This keeps
		// "exactly one non-replayed response" even when a duplicate finalizes before the original.
		resp = &accountsv1.AuthorizationResponse{Authorization: after.proto()}
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	if release {
		s.releaseQuietly(ctx, a.NetworkRef)
	}
	return resp, nil
}

// releaseHold releases "auth:<ref>" if it is still active. Absent or already-closed holds count as
// released; any other error is returned.
func (s *Service) releaseHold(ctx context.Context, networkRef string) error {
	_, err := s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{HoldReference: "auth:" + networkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED})
	if err != nil && grpcx.Reason(err) != "hold_not_found" && grpcx.Reason(err) != "hold_not_active" {
		return err
	}
	return nil
}

// releaseQuietly is releaseHold for callers with nothing better to do on failure than log.
func (s *Service) releaseQuietly(ctx context.Context, networkRef string) {
	if err := s.releaseHold(ctx, networkRef); err != nil {
		slog.WarnContext(ctx, "release of superseded hold failed; ledger sweeper will expire it", "network_ref", networkRef, "err", err)
	}
}
