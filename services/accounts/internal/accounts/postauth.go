package accounts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	case "pending":
		return grpcx.Error(codes.FailedPrecondition, "operation_in_progress", "authorization is still being processed")
	case "failed":
		return grpcx.Error(codes.FailedPrecondition, "authorization_failed", "authorization failed")
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

// opReq is a reserved capture or reversal.
type opReq struct {
	op     string // "capture" | "reversal"
	ref    string
	amount int64
}

// reserveOp claims the network_ref and marks the operation pending in one short transaction, so
// a reused ref fails on the unique constraint before the ledger is touched. It returns the row,
// whether the op is already final (replay), and whether it must be (re)driven against the ledger.
func (s *Service) reserveOp(ctx context.Context, authID string, o opReq) (a authRow, final, resumed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if a, err = lockAuth(ctx, tx, authID); err != nil {
			return err
		}
		claimed := a.CaptureRef
		if o.op == "reversal" {
			claimed = a.ReversalRef
		}
		if claimed != nil && *claimed == o.ref {
			if o.op == "capture" {
				got := a.Captured
				if a.PendingOp != nil {
					got = *a.PendingAmount
				}
				if got != o.amount {
					return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "capture network_ref reused with a different amount")
				}
			}
			final = a.PendingOp == nil // finished earlier: replay; else resume
			resumed = !final
			return nil
		}
		if a.PendingOp != nil {
			return grpcx.Error(codes.FailedPrecondition, "operation_in_progress", "another "+*a.PendingOp+" is in progress for this authorization")
		}
		if a.Status != "approved" {
			return notActive(a)
		}
		now := s.clock.Now()
		if o.op == "capture" {
			if !now.Before(*a.ExpiresAt) {
				return grpcx.Error(codes.FailedPrecondition, "authorization_expired", "authorization expired")
			}
			acc, err := s.load(ctx, tx, a.AccountID, false)
			if err != nil {
				return err
			}
			if acc.State == StateClosed {
				return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
			}
			if o.amount <= 0 || o.amount > a.Amount {
				return grpcx.Error(codes.InvalidArgument, "invalid_capture_amount", "capture must be positive and at most the authorized amount")
			}
			a, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET capture_network_ref=$2, pending_op='capture', pending_op_amount=$3, pending_op_at=$4,
				op_effective_at=`+acctNowSQL("$4", "authorizations.account_id")+` WHERE id=$1 RETURNING `+authCols, a.ID, o.ref, o.amount, now))
			return err
		}
		if now.Sub(a.CreatedAt) > ReversalWindow {
			return grpcx.Error(codes.FailedPrecondition, "reversal_window_expired", "reversals are only allowed within 3 days")
		}
		a, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET reversal_network_ref=$2, pending_op='reversal', pending_op_at=$3,
			op_effective_at=`+acctNowSQL("$3", "authorizations.account_id")+` WHERE id=$1 RETURNING `+authCols, a.ID, o.ref, now))
		return err
	})
	return a, final, resumed, err
}

// terminal reports whether a ledger error can never succeed on retry (the ledger refused the
// operation on its merits rather than failing to answer). Such an operation must reach a terminal
// state instead of being resumed forever.
func terminal(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.InvalidArgument, codes.OutOfRange, codes.NotFound, codes.AlreadyExists:
		return true
	}
	return false
}

// failOp ends a reserved capture/reversal the ledger refused permanently: the authorization becomes
// 'failed' and the pending op is cleared, so follow-ups see a terminal state and the reconciler
// stops retrying. The ledger hold (if any is still active) is released by the reconciler's
// failed-hold sweep. The original ledger error is returned to the caller.
func (s *Service) failOp(ctx context.Context, a authRow, o opReq, meta *commonv1.RequestMeta, cause error) error {
	slog.ErrorContext(ctx, "reserved operation refused by ledger; failing authorization", "authorization_id", a.ID, "op", o.op, "err", cause)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockAuth(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if cur.PendingOp == nil || *cur.PendingOp != o.op { // finished concurrently
			return nil
		}
		after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='failed', pending_op=NULL, pending_op_amount=NULL, pending_op_at=NULL
			WHERE id=$1 RETURNING `+authCols, a.ID))
		if err != nil {
			return err
		}
		return s.emitAuth(ctx, tx, "authorization.failed", cur, after, meta)
	})
	if err != nil {
		return internal(err)
	}
	return cause
}

// finishOp drives the reserved op against the ledger (idempotent) and finalizes the row.
func (s *Service) finishOp(ctx context.Context, a authRow, o opReq, meta *commonv1.RequestMeta) (*accountsv1.AuthorizationResponse, error) {
	var err error
	if o.op == "capture" {
		_, err = s.ledger.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{
			HoldReference: "auth:" + a.NetworkRef, JournalReference: "capture:" + a.ID, AmountMinor: o.amount, Meta: withEffective(meta, a.OpEffectiveAt)})
	} else {
		_, err = s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{
			HoldReference: "auth:" + a.NetworkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED, Meta: meta})
	}
	if err != nil {
		if terminal(err) {
			return nil, s.failOp(ctx, a, o, meta, err)
		}
		return nil, err // stays pending: retry with the same ref, or ResolvePending, resumes it
	}
	var resp *accountsv1.AuthorizationResponse
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockAuth(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		if cur.PendingOp == nil || *cur.PendingOp != o.op { // finished concurrently
			resp = &accountsv1.AuthorizationResponse{Authorization: cur.proto(), Replayed: true}
			return nil
		}
		typ := "authorization.captured"
		var after authRow
		if o.op == "capture" {
			after, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='captured', captured_minor=pending_op_amount, captured_at=$2,
				pending_op=NULL, pending_op_amount=NULL, pending_op_at=NULL WHERE id=$1 RETURNING `+authCols, a.ID, s.clock.Now()))
		} else {
			typ = "authorization.reversed"
			after, err = scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='reversed', reversed_at=$2,
				pending_op=NULL, pending_op_amount=NULL, pending_op_at=NULL WHERE id=$1 RETURNING `+authCols, a.ID, s.clock.Now()))
		}
		if err != nil {
			return err
		}
		resp = &accountsv1.AuthorizationResponse{Authorization: after.proto()}
		return s.emitAuth(ctx, tx, typ, cur, after, meta)
	})
	if err != nil {
		return nil, internal(err)
	}
	s.kickClose(ctx, a.AccountID)
	return resp, nil
}

func (s *Service) runOp(ctx context.Context, authID string, o opReq, meta *commonv1.RequestMeta) (*accountsv1.AuthorizationResponse, error) {
	if o.ref == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required")
	}
	a, final, _, err := s.reserveOp(ctx, authID, o)
	if err != nil {
		return nil, mapNetErr(err)
	}
	if final {
		return &accountsv1.AuthorizationResponse{Authorization: a.proto(), Replayed: true}, nil
	}
	dctx, cancel := detached(ctx)
	defer cancel()
	return s.finishOp(dctx, a, o, meta)
}

// Capture posts the purchase (amount ≤ authorized, exactly once) and frees the rest of the hold.
func (s *Service) Capture(ctx context.Context, req *accountsv1.CaptureRequest) (*accountsv1.AuthorizationResponse, error) {
	return s.runOp(ctx, req.AuthorizationId, opReq{op: "capture", ref: req.NetworkRef, amount: req.AmountMinor}, req.Meta)
}

// Reverse voids an uncaptured authorization within ReversalWindow and releases its hold.
func (s *Service) Reverse(ctx context.Context, req *accountsv1.ReverseRequest) (*accountsv1.AuthorizationResponse, error) {
	return s.runOp(ctx, req.AuthorizationId, opReq{op: "reversal", ref: req.NetworkRef}, req.Meta)
}

type refundRow struct {
	id, authID, status string
	amount             int64
	createdAt          time.Time
	effectiveAt        *time.Time
}

// Refund credits a captured purchase; partial refunds are allowed while Σ refunds (posted and
// pending) ≤ captured. Reserve (pending row) → ledger PostRefund("refund:<ref>") → finalize.
// Accepted on closed accounts (it reduces exposure).
func (s *Service) Refund(ctx context.Context, req *accountsv1.RefundRequest) (*accountsv1.RefundResponse, error) {
	if req.NetworkRef == "" {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_network_ref", "network_ref is required")
	}
	if req.AmountMinor <= 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_amount", "amount_minor must be positive")
	}
	var (
		a     authRow
		r     refundRow
		final bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		if a, err = lockAuth(ctx, tx, req.AuthorizationId); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT id::text, authorization_id::text, status, amount_minor, created_at, effective_at FROM refunds WHERE network_ref=$1`, req.NetworkRef).
			Scan(&r.id, &r.authID, &r.status, &r.amount, &r.createdAt, &r.effectiveAt)
		if err == nil {
			if r.authID != a.ID || r.amount != req.AmountMinor {
				return grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "refund network_ref reused with a different request")
			}
			if r.status == "failed" {
				return grpcx.Error(codes.FailedPrecondition, "refund_failed", "refund was refused by the ledger")
			}
			final = r.status == "posted"
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if a.Status != "captured" {
			return grpcx.Error(codes.FailedPrecondition, "not_captured", "only captured purchases can be refunded")
		}
		var pending int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(amount_minor),0) FROM refunds WHERE authorization_id=$1 AND status='pending'`, a.ID).Scan(&pending); err != nil {
			return err
		}
		if a.Refunded+pending+req.AmountMinor > a.Captured {
			return grpcx.Error(codes.FailedPrecondition, "refund_exceeds_captured", "refunds cannot exceed the captured amount")
		}
		r = refundRow{id: uuid.Must(uuid.NewV7()).String(), authID: a.ID, status: "pending", amount: req.AmountMinor, createdAt: s.clock.Now()}
		return tx.QueryRow(ctx, `INSERT INTO refunds (id, authorization_id, network_ref, amount_minor, created_at, status, effective_at)
			VALUES ($1,$2,$3,$4,$5,'pending',`+acctNowSQL("$5", "$6::uuid")+`) RETURNING effective_at`,
			r.id, a.ID, req.NetworkRef, r.amount, r.createdAt, a.AccountID).Scan(&r.effectiveAt)
	})
	if err != nil {
		return nil, mapNetErr(err)
	}
	if final {
		return &accountsv1.RefundResponse{Refund: refundProto(r.id, a, req.NetworkRef, r.amount, r.createdAt), Authorization: a.proto(), Replayed: true}, nil
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if _, err := s.ledger.PostRefund(ctx, &ledgerv1.PostRefundRequest{
		AccountId: a.AccountID, Reference: "refund:" + req.NetworkRef, AmountMinor: r.amount, Meta: withEffective(req.Meta, r.effectiveAt)}); err != nil {
		if terminal(err) {
			return nil, s.failRefund(ctx, a, r, req.Meta, err)
		}
		return nil, err // refund stays pending and keeps counting toward the cap; resumed later
	}
	var resp *accountsv1.RefundResponse
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockAuth(ctx, tx, a.ID)
		if err != nil {
			return err
		}
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM refunds WHERE id=$1 FOR UPDATE`, r.id).Scan(&st); err != nil {
			return err
		}
		if st == "posted" {
			resp = &accountsv1.RefundResponse{Refund: refundProto(r.id, cur, req.NetworkRef, r.amount, r.createdAt), Authorization: cur.proto(), Replayed: true}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE refunds SET status='posted' WHERE id=$1`, r.id); err != nil {
			return err
		}
		after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET refunded_minor = refunded_minor + $2 WHERE id=$1 RETURNING `+authCols, a.ID, r.amount))
		if err != nil {
			return err
		}
		ev := event("refund.posted", "refund", r.id, req.Meta)
		ev.Before = snapshot(cur)
		ev.After = snapshot(map[string]any{"authorization": after, "refund_id": r.id, "amount_minor": r.amount, "network_ref": req.NetworkRef})
		resp = &accountsv1.RefundResponse{Refund: refundProto(r.id, after, req.NetworkRef, r.amount, r.createdAt), Authorization: after.proto()}
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return resp, nil
}

func refundProto(id string, a authRow, ref string, amt int64, at time.Time) *accountsv1.Refund {
	return &accountsv1.Refund{Id: id, AuthorizationId: a.ID, NetworkRef: ref,
		Amount: &commonv1.Money{AmountMinor: amt, Currency: a.Currency}, CreatedAt: timestamppb.New(at)}
}

// failRefund marks a pending refund the ledger refused permanently as failed, so it stops counting
// toward the refund cap and the reconciler stops resuming it.
func (s *Service) failRefund(ctx context.Context, a authRow, r refundRow, meta *commonv1.RequestMeta, cause error) error {
	slog.ErrorContext(ctx, "refund refused by ledger; marking failed", "authorization_id", a.ID, "refund_id", r.id, "err", cause)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE refunds SET status='failed' WHERE id=$1 AND status='pending'`, r.id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		ev := event("refund.failed", "refund", r.id, meta)
		ev.After = snapshot(map[string]any{"authorization_id": a.ID, "amount_minor": r.amount, "error": cause.Error()})
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return internal(err)
	}
	return cause
}
