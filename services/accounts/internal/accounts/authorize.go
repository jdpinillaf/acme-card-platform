package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/money"
	"github.com/acme/acme-card/pkg/outbox"
)

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
}

const authCols = `id::text, account_id::text, network_ref, request_hash, amount_minor, currency, merchant_id, merchant_name, mcc,
	status, decline_reason, captured_minor, refunded_minor, capture_network_ref, reversal_network_ref,
	created_at, expires_at, captured_at, reversed_at`

func scanAuth(row pgx.Row) (authRow, error) {
	var a authRow
	err := row.Scan(&a.ID, &a.AccountID, &a.NetworkRef, &a.RequestHash, &a.Amount, &a.Currency, &a.MerchantID, &a.MerchantName, &a.MCC,
		&a.Status, &a.DeclineReason, &a.Captured, &a.Refunded, &a.CaptureRef, &a.ReversalRef,
		&a.CreatedAt, &a.ExpiresAt, &a.CapturedAt, &a.ReversedAt)
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

// Authorize evaluates a card network authorization. The authorization row is inserted first and
// its transaction stays open until the decision is final, so duplicates of the same network_ref
// wait and then replay. The ledger hold reference is deterministic ("auth:<network_ref>"), so a
// retry after a crash re-uses the same hold.
func (s *Service) Authorize(ctx context.Context, req *accountsv1.AuthorizeRequest) (*accountsv1.AuthorizationResponse, error) {
	if err := validateAuthorize(req); err != nil {
		return nil, err
	}
	acc, err := s.load(ctx, s.pool, req.CardId, false)
	if grpcx.Reason(err) == "account_not_found" {
		return nil, grpcx.Error(codes.NotFound, "card_not_found", "card not found")
	}
	if err != nil {
		return nil, err
	}
	reqHash := hashParts(req.CardId, req.AmountMinor, req.Currency, req.Merchant.Id, req.Merchant.Name, req.Merchant.Mcc)
	now := s.clock.Now()

	var resp *accountsv1.AuthorizationResponse
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		id := uuid.Must(uuid.NewV7()).String()
		tag, err := tx.Exec(ctx, `
			INSERT INTO authorizations (id, account_id, network_ref, request_hash, amount_minor, currency, merchant_id, merchant_name, mcc, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'approved',$10) ON CONFLICT (network_ref) DO NOTHING`,
			id, acc.ID, req.NetworkRef, reqHash, req.AmountMinor, req.Currency, req.Merchant.Id, req.Merchant.Name, req.Merchant.Mcc, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			existing, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authCols+` FROM authorizations WHERE network_ref=$1`, req.NetworkRef))
			if err != nil {
				return err
			}
			if existing.RequestHash != reqHash {
				return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "network_ref already used for a different authorization")
			}
			resp = &accountsv1.AuthorizationResponse{Authorization: existing.proto(), Replayed: true}
			return nil
		}

		// Every attempt that reaches here counts toward velocity, whatever the outcome.
		velocityOK, _, err := s.velocity.AllowTx(ctx, tx, "card:"+acc.ID, now)
		if err != nil {
			return err
		}
		reason := EvaluateDecline(DeclineInput{State: acc.State, Frozen: acc.Frozen, AccountCurrency: acc.Currency, Currency: req.Currency,
			VelocityOK: velocityOK, PurchaseLimit: acc.PurchaseLimit, Amount: req.AmountMinor})
		if reason == "" {
			hold, err := s.ledger.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{
				AccountId: acc.ID, Reference: "auth:" + req.NetworkRef, AmountMinor: req.AmountMinor, Currency: req.Currency, Meta: req.Meta})
			if err != nil {
				return err
			}
			if !hold.Approved {
				reason = DeclineInsufficientCredit
			}
		}

		var a authRow
		if reason != "" {
			a, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='declined', decline_reason=$2 WHERE id=$1 RETURNING `+authCols, id, string(reason)))
		} else {
			a, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET expires_at=$2 WHERE id=$1 RETURNING `+authCols, id, now.Add(HoldTTL)))
		}
		if err != nil {
			return err
		}
		typ := "authorization.approved"
		if reason != "" {
			typ = "authorization.declined"
		}
		ev := event(typ, "authorization", a.ID, req.Meta)
		ev.After = snapshot(a)
		if err := outbox.Enqueue(ctx, tx, ev); err != nil {
			return err
		}
		resp = &accountsv1.AuthorizationResponse{Authorization: a.proto()}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return resp, nil
}
