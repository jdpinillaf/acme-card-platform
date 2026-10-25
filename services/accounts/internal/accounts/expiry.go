package accounts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

// expiryRetryAfter is how long a row whose expiry failed is skipped before being retried.
const expiryRetryAfter = time.Hour

// ExpireDue releases holds of approved authorizations past expires_at, one row per transaction.
// Rows with an operation in flight are left alone; a row whose ledger release fails is marked and
// skipped for an hour so it can never block the others (review C2). Returns how many expired.
func (s *Service) ExpireDue(ctx context.Context, batch int) (int, error) {
	n := 0
	for i := 0; i < batch; i++ {
		done, err := s.expireOne(ctx)
		if err != nil {
			return n, err
		}
		if !done {
			return n, nil
		}
		n++
	}
	return n, nil
}

// expireOne expires the next due row. done=false when nothing is due.
func (s *Service) expireOne(ctx context.Context) (done bool, err error) {
	now := s.clock.Now()
	for {
		var (
			a         authRow
			ledgerErr error
		)
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			a, err = scanAuth(tx.QueryRow(ctx, `SELECT `+authCols+` FROM authorizations
				WHERE status='approved' AND pending_op IS NULL AND expires_at < $1
				  AND (expiry_failed_at IS NULL OR expiry_failed_at < $2)
				ORDER BY expires_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now, now.Add(-expiryRetryAfter)))
			if err != nil {
				return err
			}
			if _, ledgerErr = s.ledger.ReleaseHold(ctx, &ledgerv1.ReleaseHoldRequest{
				HoldReference: "auth:" + a.NetworkRef, Reason: ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED}); ledgerErr != nil {
				_, err := tx.Exec(ctx, `UPDATE authorizations SET expiry_failed_at=$2, expiry_error=$3 WHERE id=$1`, a.ID, now, ledgerErr.Error())
				return err
			}
			after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='expired', expiry_failed_at=NULL, expiry_error=NULL
				WHERE id=$1 RETURNING `+authCols, a.ID))
			if err != nil {
				return err
			}
			return s.emitAuth(ctx, tx, "authorization.expired", a, after, nil)
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if ledgerErr == nil {
			return true, nil
		}
		slog.ErrorContext(ctx, "hold expiry failed; row skipped and retried later", "authorization_id", a.ID, "err", ledgerErr)
	}
}

// ResolvePending finishes work interrupted after a reservation (review C3/I1b):
//   - authorizations still 'pending' after PendingTimeout never got an answer to the network:
//     release any hold and mark them 'failed' (the network treats an unanswered auth as declined);
//   - captures, reversals, refunds and deposit settlements reserved but unfinished are resumed —
//     the network or bank asked for them and every ledger step is idempotent.
func (s *Service) ResolvePending(ctx context.Context) error {
	cutoff := s.clock.Now().Add(-PendingTimeout)

	rows, err := s.pool.Query(ctx, `SELECT `+authCols+` FROM authorizations WHERE status='pending' AND created_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	stale, err := collectAuths(rows)
	if err != nil {
		return err
	}
	for _, a := range stale {
		s.releaseQuietly(ctx, a.NetworkRef)
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			cur, err := lockAuth(ctx, tx, a.ID)
			if err != nil || cur.Status != "pending" {
				return err
			}
			after, err := scanAuth(tx.QueryRow(ctx, `UPDATE authorizations SET status='failed' WHERE id=$1 RETURNING `+authCols, a.ID))
			if err != nil {
				return err
			}
			return s.emitAuth(ctx, tx, "authorization.failed", cur, after, nil)
		})
		if err != nil {
			return err
		}
	}

	rows, err = s.pool.Query(ctx, `SELECT `+authCols+` FROM authorizations WHERE pending_op IS NOT NULL AND pending_op_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	ops, err := collectAuths(rows)
	if err != nil {
		return err
	}
	for _, a := range ops {
		if *a.PendingOp == "capture" {
			_, err = s.Capture(ctx, &accountsv1.CaptureRequest{AuthorizationId: a.ID, NetworkRef: *a.CaptureRef, AmountMinor: *a.PendingAmount})
		} else {
			_, err = s.Reverse(ctx, &accountsv1.ReverseRequest{AuthorizationId: a.ID, NetworkRef: *a.ReversalRef})
		}
		if err != nil {
			slog.ErrorContext(ctx, "resuming pending operation failed", "authorization_id", a.ID, "op", *a.PendingOp, "err", err)
		}
	}

	depRows, err := s.pool.Query(ctx, `SELECT id::text, external_ref, amount_minor, currency FROM deposits WHERE status='settling' AND settling_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	type pd struct {
		id, ref, cur string
		amt          int64
	}
	var deps []pd
	for depRows.Next() {
		var d pd
		if err := depRows.Scan(&d.id, &d.ref, &d.amt, &d.cur); err != nil {
			depRows.Close()
			return err
		}
		deps = append(deps, d)
	}
	depRows.Close()
	for _, d := range deps {
		if _, err := s.SettleDeposit(ctx, &accountsv1.SettleDepositRequest{DepositId: d.id, ExternalRef: d.ref, AmountMinor: d.amt, Currency: d.cur,
			Status: accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED, Meta: &commonv1.RequestMeta{Actor: "accounts.reconciler"}}); err != nil {
			slog.ErrorContext(ctx, "resuming settling deposit failed", "deposit_id", d.id, "err", err)
		}
	}

	refundRows, err := s.pool.Query(ctx, `SELECT authorization_id::text, network_ref, amount_minor FROM refunds WHERE status='pending' AND created_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	type pr struct {
		auth, ref string
		amt       int64
	}
	var refunds []pr
	for refundRows.Next() {
		var p pr
		if err := refundRows.Scan(&p.auth, &p.ref, &p.amt); err != nil {
			refundRows.Close()
			return err
		}
		refunds = append(refunds, p)
	}
	refundRows.Close()
	for _, p := range refunds {
		if _, err := s.Refund(ctx, &accountsv1.RefundRequest{AuthorizationId: p.auth, NetworkRef: p.ref, AmountMinor: p.amt}); err != nil {
			slog.ErrorContext(ctx, "resuming pending refund failed", "authorization_id", p.auth, "err", err)
		}
	}
	return nil
}

func collectAuths(rows pgx.Rows) ([]authRow, error) {
	defer rows.Close()
	var out []authRow
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RunExpiry runs expiry, pending resolution and velocity-window cleanup every interval.
func (s *Service) RunExpiry(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n, err := s.ExpireDue(ctx, 500); err != nil && ctx.Err() == nil {
			log.Error("hold expiry failed", "err", err)
		} else if n > 0 {
			log.Info("authorizations expired", "count", n)
		}
		if err := s.ResolvePending(ctx); err != nil && ctx.Err() == nil {
			log.Error("pending resolution failed", "err", err)
		}
		if err := s.velocity.Sweep(ctx, s.clock.Now()); err != nil && ctx.Err() == nil {
			log.Error("velocity window sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
