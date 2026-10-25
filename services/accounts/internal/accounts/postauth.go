package accounts

import (
	"context"
	"errors"
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
	"github.com/acme/acme-card/pkg/outbox"
)

// ReversalWindow is how long after authorization an uncaptured authorization may be reversed.
const ReversalWindow = 72 * time.Hour

// lockAuth loads an authorization FOR UPDATE; every follow-up message serializes on this row.
func lockAuth(ctx context.Context, tx pgx.Tx, id string) (authRow, error) {
	if _, err := uuid.Parse(id); err != nil {
		return authRow{}, grpcx.Error(codes.InvalidArgument, "invalid_authorization", "authorization_id must be a uuid")
	}
	a, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authCols+` FROM authorizations WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, grpcx.Error(codes.NotFound, "authorization_not_found", "authorization not found")
	}
	return a, err
}

func notActive(a authRow) error {
	switch a.Status {
	case "captured":
		return grpcx.Error(codes.FailedPrecondition, "already_captured", "authorization already captured")
	case "reversed":
		return grpcx.Error(codes.FailedPrecondition, "already_reversed", "authorization already reversed")
	case "expired":
		return grpcx.Error(codes.FailedPrecondition, "authorization_expired", "authorization expired")
	default:
		return grpcx.Error(codes.FailedPrecondition, "authorization_declined", "authorization was declined")
	}
}

func mapNetErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "network_ref already used by another message")
	}
	return internal(err)
}

func (s *Service) emitAuth(ctx context.Context, tx pgx.Tx, typ string, before, after authRow, meta *commonv1.RequestMeta) error {
	ev := event(typ, "authorization", after.ID, meta)
	ev.Before, ev.After = snapshot(before), snapshot(after)
	return outbox.Enqueue(ctx, tx, ev)
}

// Capture posts the purchase (amount ≤ authorized, exactly once) and frees the rest of the hold.
func (s *Service) Capture(ctx context.Context, req *accountsv1.CaptureRequest) (*accountsv1.AuthorizationResponse, error) {
	if req.NetworkRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required")
	}
	var resp *accountsv1.AuthorizationResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		a, err := lockAuth(ctx, tx, req.AuthorizationId)
		if err != nil {
			return err
		}
		if a.CaptureRef != nil && *a.CaptureRef == req.NetworkRef {
			if a.Captured != req.AmountMinor {
				return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "capture network_ref reused with a different amount")
			}
			resp = &accountsv1.AuthorizationResponse{Authorization: a.proto(), Replayed: true}
			return nil
		}
		if a.Status != "approved" {
			return notActive(a)
		}
		if !s.clock.Now().Before(*a.ExpiresAt) {
			return grpcx.Error(codes.FailedPrecondition, "authorization_expired", "authorization expired")
		}
		acc, err := s.load(ctx, tx, a.AccountID, false)
		if err != nil {
			return err
		}
		if acc.State == StateClosed {
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		}
		if req.AmountMinor <= 0 || req.AmountMinor > a.Amount {
			return grpcx.Error(codes.InvalidArgument, "invalid_capture_amount", "capture must be positive and at most the authorized amount")
		}
		if _, err := s.ledger.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
			HoldReference: "auth:" + a.NetworkRef, JournalReference: "capture:" + a.ID, AmountMinor: req.AmountMinor, Meta: req.Meta}); err != nil {
			return err
		}
		after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='captured', captured_minor=$2, capture_network_ref=$3, captured_at=$4
			WHERE id=$1 RETURNING `+authCols, a.ID, req.AmountMinor, req.NetworkRef, s.clock.Now()))
		if err != nil {
			return err
		}
		resp = &accountsv1.AuthorizationResponse{Authorization: after.proto()}
		return s.emitAuth(ctx, tx, "authorization.captured", a, after, req.Meta)
	})
	if err != nil {
		return nil, mapNetErr(err)
	}
	return resp, nil
}

// Reverse voids an uncaptured authorization within ReversalWindow and releases its hold.
func (s *Service) Reverse(ctx context.Context, req *accountsv1.ReverseRequest) (*accountsv1.AuthorizationResponse, error) {
	if req.NetworkRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required")
	}
	var resp *accountsv1.AuthorizationResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		a, err := lockAuth(ctx, tx, req.AuthorizationId)
		if err != nil {
			return err
		}
		if a.ReversalRef != nil && *a.ReversalRef == req.NetworkRef {
			resp = &accountsv1.AuthorizationResponse{Authorization: a.proto(), Replayed: true}
			return nil
		}
		if a.Status != "approved" {
			return notActive(a)
		}
		if s.clock.Now().Sub(a.CreatedAt) > ReversalWindow {
			return grpcx.Error(codes.FailedPrecondition, "reversal_window_expired", "reversals are only allowed within 3 days")
		}
		if _, err := s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{
			HoldReference: "auth:" + a.NetworkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED, Meta: req.Meta}); err != nil {
			return err
		}
		after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='reversed', reversal_network_ref=$2, reversed_at=$3
			WHERE id=$1 RETURNING `+authCols, a.ID, req.NetworkRef, s.clock.Now()))
		if err != nil {
			return err
		}
		resp = &accountsv1.AuthorizationResponse{Authorization: after.proto()}
		return s.emitAuth(ctx, tx, "authorization.reversed", a, after, req.Meta)
	})
	if err != nil {
		return nil, mapNetErr(err)
	}
	return resp, nil
}

// Refund credits a captured purchase; partial refunds are allowed while Σ refunds ≤ captured.
// Accepted on closed accounts (it reduces exposure).
func (s *Service) Refund(ctx context.Context, req *accountsv1.RefundRequest) (*accountsv1.RefundResponse, error) {
	if req.NetworkRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required")
	}
	if req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be positive")
	}
	var resp *accountsv1.RefundResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		a, err := lockAuth(ctx, tx, req.AuthorizationId)
		if err != nil {
			return err
		}
		var (
			existingID, existingAuth string
			existingAmt              int64
			existingAt               time.Time
		)
		err = tx.QueryRow(ctx, `SELECT id::text, authorization_id::text, amount_minor, created_at FROM refunds WHERE network_ref=$1`, req.NetworkRef).
			Scan(&existingID, &existingAuth, &existingAmt, &existingAt)
		if err == nil {
			if existingAuth != a.ID || existingAmt != req.AmountMinor {
				return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "refund network_ref reused with a different request")
			}
			resp = &accountsv1.RefundResponse{Refund: refundProto(existingID, a, req.NetworkRef, existingAmt, existingAt), Authorization: a.proto(), Replayed: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if a.Status != "captured" {
			return grpcx.Error(codes.FailedPrecondition, "not_captured", "only captured purchases can be refunded")
		}
		if a.Refunded+req.AmountMinor > a.Captured {
			return grpcx.Error(codes.FailedPrecondition, "refund_exceeds_captured", "refunds cannot exceed the captured amount")
		}
		id, now := uuid.Must(uuid.NewV7()).String(), s.clock.Now()
		if _, err := tx.Exec(ctx, `INSERT INTO refunds (id, authorization_id, network_ref, amount_minor, created_at) VALUES ($1,$2,$3,$4,$5)`,
			id, a.ID, req.NetworkRef, req.AmountMinor, now); err != nil {
			return err
		}
		if _, err := s.ledger.PostRefund(ctx, &ledgerv1.PostRefundRequest{
			AccountId: a.AccountID, Reference: "refund:" + req.NetworkRef, AmountMinor: req.AmountMinor, Meta: req.Meta}); err != nil {
			return err
		}
		after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET refunded_minor = refunded_minor + $2 WHERE id=$1 RETURNING `+authCols, a.ID, req.AmountMinor))
		if err != nil {
			return err
		}
		ev := event("refund.posted", "refund", id, req.Meta)
		ev.Before = snapshot(a)
		ev.After = snapshot(map[string]any{"authorization": after, "refund_id": id, "amount_minor": req.AmountMinor, "network_ref": req.NetworkRef})
		if err := outbox.Enqueue(ctx, tx, ev); err != nil {
			return err
		}
		resp = &accountsv1.RefundResponse{Refund: refundProto(id, a, req.NetworkRef, req.AmountMinor, now), Authorization: after.proto()}
		return nil
	})
	if err != nil {
		return nil, mapNetErr(err)
	}
	return resp, nil
}

func refundProto(id string, a authRow, ref string, amt int64, at time.Time) *accountsv1.Refund {
	return &accountsv1.Refund{Id: id, AuthorizationId: a.ID, NetworkRef: ref,
		Amount: &commonv1.Money{AmountMinor: amt, Currency: a.Currency}, CreatedAt: timestamppb.New(at)}
}
