package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

const (
	// MinPaymentMinor is the smallest payment ($1.00 in a 2-decimal currency).
	MinPaymentMinor int64 = 100
	// PaymentTTL is how long a payment intent waits for the bank before expiring.
	PaymentTTL = 7 * 24 * time.Hour
	// ReturnWindow is how long after settlement the bank may return a payment.
	ReturnWindow = 120 * time.Hour
)

type paymentRow struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"account_id"`
	Amount      int64      `json:"amount_minor"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	ExternalRef *string    `json:"external_ref,omitempty"`
	ReturnRef   *string    `json:"return_ref,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SettlingAt  *time.Time `json:"settling_at,omitempty"`
	SettledAt   *time.Time `json:"settled_at,omitempty"`
	ReturningAt *time.Time `json:"returning_at,omitempty"`
	ReturnedAt  *time.Time `json:"returned_at,omitempty"`
}

const paymentCols = `id::text, account_id::text, amount_minor, currency, status, external_ref, return_ref, created_at, settling_at, settled_at, returning_at, returned_at`

func scanPayment(row pgx.Row) (paymentRow, error) {
	var p paymentRow
	err := row.Scan(&p.ID, &p.AccountID, &p.Amount, &p.Currency, &p.Status, &p.ExternalRef, &p.ReturnRef, &p.CreatedAt, &p.SettlingAt, &p.SettledAt, &p.ReturningAt, &p.ReturnedAt)
	return p, err
}

var paymentStatusProto = map[string]accountsv1.PaymentStatus{
	"pending":   accountsv1.PaymentStatus_PAYMENT_STATUS_PENDING,
	"settling":  accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLING,
	"settled":   accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED,
	"failed":    accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED,
	"expired":   accountsv1.PaymentStatus_PAYMENT_STATUS_EXPIRED,
	"returning": accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNING,
	"returned":  accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNED,
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (p paymentRow) proto() *accountsv1.Payment {
	return &accountsv1.Payment{Id: p.ID, AccountId: p.AccountID, Amount: &commonv1.Money{AmountMinor: p.Amount, Currency: p.Currency},
		Status: paymentStatusProto[p.Status], ExternalRef: strOr(p.ExternalRef), ReturnRef: strOr(p.ReturnRef),
		CreatedAt: timestamppb.New(p.CreatedAt), SettledAt: tsp(p.SettledAt), ReturnedAt: tsp(p.ReturnedAt)}
}

func (s *Service) paymentResponse(ctx context.Context, p paymentRow, replayed bool) (*accountsv1.PaymentResponse, error) {
	acc, err := s.load(ctx, s.pool, p.AccountID, false)
	if err != nil {
		return nil, err
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	return &accountsv1.PaymentResponse{Payment: p.proto(), Account: acc.proto(v), Replayed: replayed}, nil
}

// CreatePayment registers a customer payment intent. Credit is only restored when the bank settles
// it. Open payments (pending/settling) can never sum to more than the outstanding debt.
func (s *Service) CreatePayment(ctx context.Context, req *accountsv1.CreatePaymentRequest) (*accountsv1.PaymentResponse, error) {
	if req.AmountMinor < MinPaymentMinor || req.AmountMinor > MaxDepositMinor {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be at least 100 (1.00) and within limits")
	}
	acc, err := s.load(ctx, s.pool, req.AccountId, false)
	if err != nil {
		return nil, err
	}
	var keyHash *string
	if req.Meta != nil && req.Meta.IdempotencyKeyHash != "" {
		keyHash = &req.Meta.IdempotencyKeyHash
		existing, err := scanPayment(s.pool.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE idempotency_key_hash=$1`, *keyHash))
		if err == nil {
			return s.paymentResponse(ctx, existing, true)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, internal(err)
		}
	}
	// Read the debt from the ledger before taking any lock (no ledger call inside held locks).
	view, err := s.available(ctx, acc)
	if err != nil {
		return nil, err
	}
	var (
		p        paymentRow
		locked   accountRow
		replayed bool
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		locked, err = s.load(ctx, tx, acc.ID, true) // serializes cap checks per account
		if err != nil {
			return err
		}
		switch locked.State {
		case StateClosed:
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		case StatePendingCollateral:
			return grpcx.Error(codes.FailedPrecondition, "collateral_not_confirmed", "account is not active yet")
		}
		var open int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_minor),0) FROM payments WHERE account_id=$1 AND status IN ('pending','settling')`, acc.ID).Scan(&open); err != nil {
			return err
		}
		room := max(view.Receivable, 0) - open
		if room <= 0 {
			return grpcx.Error(codes.FailedPrecondition, "nothing_to_pay", "there is no outstanding balance to pay")
		}
		if req.AmountMinor > room {
			return grpcx.Error(codes.FailedPrecondition, "payment_exceeds_balance", "payment exceeds the outstanding balance")
		}
		id := uuid.Must(uuid.NewV7()).String()
		p, err = scanPayment(tx.QueryRow(ctx, `
			INSERT INTO payments (id, account_id, amount_minor, currency, status, idempotency_key_hash, created_at)
			VALUES ($1,$2,$3,$4,'pending',$5,$6)
			ON CONFLICT (idempotency_key_hash) WHERE idempotency_key_hash IS NOT NULL DO NOTHING
			RETURNING `+paymentCols, id, acc.ID, req.AmountMinor, locked.Currency, keyHash, s.clock.Now()))
		if errors.Is(err, pgx.ErrNoRows) { // concurrent duplicate with the same key won
			replayed = true
			p, err = scanPayment(tx.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE idempotency_key_hash=$1`, *keyHash))
			return err
		}
		if err != nil {
			return err
		}
		ev := event("payment.created", "payment", p.ID, req.Meta)
		ev.After = snapshot(p)
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	// Creating an intent does not touch the ledger, so the view read above is still current.
	return &accountsv1.PaymentResponse{Payment: p.proto(), Account: locked.proto(view), Replayed: replayed}, nil
}

// ExpirePayments expires pending payment intents the bank never answered (PaymentTTL), freeing
// their share of the payment cap. Returns how many expired.
func (s *Service) ExpirePayments(ctx context.Context, batch int) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+paymentCols+` FROM payments WHERE status='pending' AND created_at < $1
			ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, s.clock.Now().Add(-PaymentTTL), batch)
		if err != nil {
			return err
		}
		var due []paymentRow
		for rows.Next() {
			p, err := scanPayment(rows)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range due {
			after, err := scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='expired' WHERE id=$1 RETURNING `+paymentCols, p.ID))
			if err != nil {
				return err
			}
			ev := event("payment.expired", "payment", p.ID, nil)
			ev.Before, ev.After = snapshot(p), snapshot(after)
			if err := outbox.Enqueue(ctx, tx, ev); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// ListPayments returns the account's payments newest first, keyset-paginated.
func (s *Service) ListPayments(ctx context.Context, req *accountsv1.ListPaymentsRequest) (*accountsv1.ListPaymentsResponse, error) {
	if _, err := s.load(ctx, s.pool, req.AccountId, false); err != nil {
		return nil, err
	}
	size := int(req.PageSize)
	if size <= 0 {
		size = 20
	}
	size = min(size, 100)
	cur := pageCursor{CreatedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	if req.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		if err != nil || json.Unmarshal(raw, &cur) != nil || cur.ID == "" {
			return nil, grpcx.Error(codes.InvalidArgument, "invalid_page_token", "page_token is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+paymentCols+` FROM payments WHERE account_id=$1 AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $4`, req.AccountId, cur.CreatedAt, cur.ID, size+1)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []paymentRow
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, internal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	resp := &accountsv1.ListPaymentsResponse{}
	if len(out) > size {
		last := out[size-1]
		b, _ := json.Marshal(pageCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString(b)
		out = out[:size]
	}
	for _, p := range out {
		resp.Payments = append(resp.Payments, p.proto())
	}
	return resp, nil
}
