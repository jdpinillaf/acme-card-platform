package accounts

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
)

// Payout kinds: the close payout, and additional ones for refunds landing after the close.
const (
	payoutKindClose      = "close"
	payoutKindAdditional = "additional"
)

const payoutCols = `p.id::text, p.account_id::text, p.kind, p.collateral_minor, p.credit_minor, p.currency, p.status, p.attempt, p.created_at, p.settled_at,
	(SELECT a.id::text FROM payout_attempts a WHERE a.payout_id=p.id AND a.attempt=p.attempt),
	coalesce((SELECT destination_last4 FROM accounts x WHERE x.id=p.account_id), '')`

type payoutRow struct {
	ID, AccountID, Kind, Currency, Status, AttemptID, Last4 string
	Collateral, Credit                                      int64
	Attempt                                                 int32
	CreatedAt                                               time.Time
	SettledAt                                               *time.Time
}

func scanPayout(row pgx.Row) (payoutRow, error) {
	var p payoutRow
	err := row.Scan(&p.ID, &p.AccountID, &p.Kind, &p.Collateral, &p.Credit, &p.Currency, &p.Status, &p.Attempt, &p.CreatedAt, &p.SettledAt, &p.AttemptID, &p.Last4)
	return p, err
}

var payoutStatusProto = map[string]accountsv1.PayoutStatus{
	"pending":  accountsv1.PayoutStatus_PAYOUT_STATUS_PENDING,
	"settling": accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLING,
	"settled":  accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED,
	"failed":   accountsv1.PayoutStatus_PAYOUT_STATUS_FAILED,
}

func (p payoutRow) proto() *accountsv1.Payout {
	return &accountsv1.Payout{Id: p.ID, AccountId: p.AccountID, Amount: &commonv1.Money{AmountMinor: p.Collateral + p.Credit, Currency: p.Currency},
		Status: payoutStatusProto[p.Status], Attempt: p.Attempt, AttemptId: p.AttemptID, DestinationLast4: p.Last4,
		CreatedAt: timestamppb.New(p.CreatedAt), SettledAt: tsp(p.SettledAt)}
}

// payoutEvent carries ids, amounts and the masked destination only; never the account number.
func payoutEvent(typ string, p payoutRow, meta *commonv1.RequestMeta) (ev events.Event) {
	ev = event(typ, "payout", p.ID, meta)
	ev.After = snapshot(map[string]any{"account_id": p.AccountID, "kind": p.Kind, "attempt": p.Attempt, "attempt_id": p.AttemptID,
		"amount_minor": p.Collateral + p.Credit, "collateral_minor": p.Collateral, "credit_minor": p.Credit, "currency": p.Currency,
		"status": p.Status, "destination_last4": p.Last4})
	return ev
}

// createPayout opens a payout with its first attempt inside tx and emits payout.requested (the
// bank's instruction, fetched through the bank-signed instruction route).
func (s *Service) createPayout(ctx context.Context, tx pgx.Tx, acc accountRow, kind string, collateral, credit int64) error {
	id, attempt := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	now := s.clock.Now()
	if _, err := tx.Exec(ctx, `INSERT INTO payouts (id, account_id, kind, collateral_minor, credit_minor, currency, status, created_at) VALUES ($1,$2,$3,$4,$5,$6,'pending',$7)`,
		id, acc.ID, kind, collateral, credit, acc.Currency, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payout_attempts (id, payout_id, attempt, status, created_at) VALUES ($1,$2,1,'pending',$3)`, attempt, id, now); err != nil {
		return err
	}
	p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.id=$1`, id))
	if err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, payoutEvent("payout.requested", p, &commonv1.RequestMeta{Actor: "accounts.close_saga"}))
}

// payoutStep: create the close payout (remaining collateral + credit balance), wait for the bank,
// and close the account once nothing is owed to the customer.
func (s *Service) payoutStep(ctx context.Context, acc accountRow) (bool, error) {
	var open, failed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('pending','settling')), count(*) FILTER (WHERE status='failed')
		FROM payouts WHERE account_id=$1`, acc.ID).Scan(&open, &failed); err != nil {
		return false, internal(err)
	}
	if open > 0 || failed > 0 { // waiting for the bank, or for support to retry
		return false, nil
	}
	v, err := s.available(ctx, acc)
	if err != nil {
		return false, err
	}
	credit := max(-v.Receivable, 0)
	if acc.Collateral+credit == 0 {
		return s.finishClose(ctx, acc.ID)
	}
	if acc.DestLast4 == nil { // support closed without a destination: wait for one (review I6)
		return false, nil
	}
	return false, internal0(s.createPayoutLocked(ctx, acc, payoutKindClose, acc.Collateral, credit))
}

// createPayoutLocked creates a payout sized from a ledger read taken before the lock, only if the
// account is unchanged since that read (version), no payout is open and the bank can no longer
// change any payment (return windows re-checked under the lock, review I3/M1). A stale read is
// simply retried by the next step.
func (s *Service) createPayoutLocked(ctx context.Context, read accountRow, kind string, collateral, credit int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		locked, err := s.load(ctx, tx, read.ID, true)
		if err != nil || locked.Version != read.Version {
			return err
		}
		if kind == payoutKindClose && (locked.CloseStage == nil || *locked.CloseStage != StagePayout) {
			return nil
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE account_id=$1 AND status IN ('pending','settling','failed')`, read.ID).Scan(&n); err != nil || n > 0 {
			return err
		}
		if open, err := s.paymentsUnsettled(ctx, tx, read.ID); err != nil || open {
			return err
		}
		if err := s.createPayout(ctx, tx, locked, kind, collateral, credit); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET version=version+1 WHERE id=$1`, read.ID) // later stale readers abort
		return err
	})
}

func (s *Service) finishClose(ctx context.Context, accountID string) (bool, error) {
	moved, err := s.setStage(ctx, accountID, StagePayout, StageClosed, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, accountID, true)
		if err != nil {
			return err
		}
		next, err := Transition(before.State, EventCloseCompleted)
		if err != nil {
			return err
		}
		after, err := scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET state=$2 WHERE id=$1 RETURNING `+accountCols, accountID, next))
		if err != nil {
			return err
		}
		ev := event("account.closed", "account", accountID, &commonv1.RequestMeta{Actor: "accounts.close_saga"})
		ev.Before, ev.After = snapshot(before), snapshot(after)
		return outbox.Enqueue(ctx, tx, ev)
	})
	return moved, err
}

func lockAttempt(ctx context.Context, tx pgx.Tx, attemptID string) (payoutRow, string, *time.Time, error) {
	if _, err := uuid.Parse(attemptID); err != nil {
		return payoutRow{}, "", nil, grpcx.Error(codes.NotFound, "payout_not_found", "payout attempt not found")
	}
	var payoutID, status string
	var eff *time.Time
	err := tx.QueryRow(ctx, `SELECT payout_id::text, status, effective_at FROM payout_attempts WHERE id=$1 FOR UPDATE`, attemptID).Scan(&payoutID, &status, &eff)
	if errors.Is(err, pgx.ErrNoRows) {
		return payoutRow{}, "", nil, grpcx.Error(codes.NotFound, "payout_not_found", "payout attempt not found")
	}
	if err != nil {
		return payoutRow{}, "", nil, err
	}
	p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.id=$1 FOR UPDATE`, payoutID))
	return p, status, eff, err
}

// SettlePayout applies the bank's answer for one attempt. failed is final for that attempt;
// settled runs reserve (attempt settling, business date) → ledger PostPayout("payout:<attempt>")
// → finalize (collateral and limit drop), then lets the close saga finish.
func (s *Service) SettlePayout(ctx context.Context, req *accountsv1.SettlePayoutRequest) (*accountsv1.PayoutResponse, error) {
	if req.Status != accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED && req.Status != accountsv1.PayoutStatus_PAYOUT_STATUS_FAILED {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_status", "status must be settled or failed")
	}
	settle := req.Status == accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED
	var (
		p        payoutRow
		eff      *time.Time
		done     bool
		replayed bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var st string
		var err error
		if p, st, eff, err = lockAttempt(ctx, tx, req.AttemptId); err != nil {
			return err
		}
		switch st {
		case "failed":
			if settle {
				return grpcx.Error(codes.FailedPrecondition, "attempt_failed", "this payout attempt already failed")
			}
			done, replayed = true, true
			return nil
		case "settled":
			if !settle {
				return grpcx.Error(codes.FailedPrecondition, "payout_already_settled", "this payout attempt already settled")
			}
			done, replayed = true, true
			return nil
		case "settling":
			if !settle {
				return grpcx.Error(codes.FailedPrecondition, "payout_settling", "payout settlement already in progress")
			}
			return nil // resume
		}
		if req.Currency != p.Currency || req.AmountMinor != p.Collateral+p.Credit {
			return grpcx.Error(codes.InvalidArgument, "payout_amount_mismatch", "amount or currency differs from the payout")
		}
		if req.DestinationLast4 != p.Last4 {
			return grpcx.Error(codes.InvalidArgument, "destination_mismatch", "destination does not match the payout")
		}
		now := s.clock.Now()
		if !settle {
			if _, err := tx.Exec(ctx, `UPDATE payout_attempts SET status='failed', settled_at=$2 WHERE id=$1`, req.AttemptId, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payouts SET status='failed' WHERE id=$1`, p.ID); err != nil {
				return err
			}
			p.Status, done = "failed", true
			return outbox.Enqueue(ctx, tx, payoutEvent("payout.failed", p, req.Meta))
		}
		if err := tx.QueryRow(ctx, `UPDATE payout_attempts SET status='settling', settling_at=$2, effective_at=`+acctNowSQL("$2", "$3::uuid")+`
			WHERE id=$1 RETURNING effective_at`, req.AttemptId, now, p.AccountID).Scan(&eff); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payouts SET status='settling' WHERE id=$1`, p.ID)
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	if done {
		return s.payoutResponse(ctx, p.ID, replayed)
	}

	ctx, cancel := detached(ctx)
	defer cancel()
	jr, err := s.ledger.PostPayout(ctx, &ledgerv1.PostPayoutRequest{AccountId: p.AccountID, Reference: "payout:" + req.AttemptId,
		CollateralMinor: p.Collateral, CreditMinor: p.Credit, Meta: withEffective(req.Meta, eff)})
	if err != nil {
		return nil, err // stays settling: a retry or ResolvePending resumes it
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, st, _, err := lockAttempt(ctx, tx, req.AttemptId)
		if err != nil {
			return err
		}
		if st != "settling" { // finished concurrently
			replayed = true
			return nil
		}
		now := s.clock.Now()
		if _, err := tx.Exec(ctx, `UPDATE payout_attempts SET status='settled', settled_at=$2, journal_entry_id=$3 WHERE id=$1`, req.AttemptId, now, jr.JournalEntryId); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payouts SET status='settled', settled_at=$2 WHERE id=$1`, cur.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE accounts SET collateral_minor = collateral_minor - $2, credit_limit_minor = credit_limit_minor - $2,
			version=version+1, updated_at=now() WHERE id=$1`, cur.AccountID, cur.Collateral); err != nil {
			return err
		}
		cur.Status = "settled"
		return outbox.Enqueue(ctx, tx, payoutEvent("payout.settled", cur, req.Meta))
	})
	if err != nil {
		return nil, internal(err)
	}
	if _, err := s.AdvanceClose(ctx, &accountsv1.AdvanceCloseRequest{AccountId: p.AccountID}); err != nil {
		slog.WarnContext(ctx, "close saga after payout deferred to the sweep", "account_id", p.AccountID, "err", err)
	}
	return s.payoutResponse(ctx, p.ID, replayed)
}

func (s *Service) payoutResponse(ctx context.Context, payoutID string, replayed bool) (*accountsv1.PayoutResponse, error) {
	p, err := scanPayout(s.pool.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.id=$1`, payoutID))
	if err != nil {
		return nil, internal(err)
	}
	acc, err := s.accountResponse(ctx, accountRow{ID: p.AccountID})
	if err != nil {
		return nil, err
	}
	return &accountsv1.PayoutResponse{Payout: p.proto(), Account: acc.Account, Replayed: replayed}, nil
}

// RetryPayout opens the next attempt of a failed payout (support).
func (s *Service) RetryPayout(ctx context.Context, req *accountsv1.RetryPayoutRequest) (*accountsv1.PayoutResponse, error) {
	if _, err := uuid.Parse(req.PayoutId); err != nil {
		return nil, grpcx.Error(codes.NotFound, "payout_not_found", "payout not found")
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		p, err := scanPayout(tx.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.id=$1 AND p.account_id=$2 FOR UPDATE`, req.PayoutId, req.AccountId))
		if errors.Is(err, pgx.ErrNoRows) {
			return grpcx.Error(codes.NotFound, "payout_not_found", "payout not found")
		}
		if err != nil {
			return err
		}
		if p.Status != "failed" {
			return grpcx.Error(codes.FailedPrecondition, "payout_not_failed", "only a failed payout can be retried")
		}
		next := p.Attempt + 1
		if _, err := tx.Exec(ctx, `INSERT INTO payout_attempts (id, payout_id, attempt, status, created_at) VALUES ($1,$2,$3,'pending',$4)`,
			uuid.Must(uuid.NewV7()), p.ID, next, s.clock.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payouts SET status='pending', attempt=$2 WHERE id=$1`, p.ID, next); err != nil {
			return err
		}
		if p, err = scanPayout(tx.QueryRow(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.id=$1`, p.ID)); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, tx, payoutEvent("payout.retried", p, req.Meta), payoutEvent("payout.requested", p, req.Meta))
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.payoutResponse(ctx, req.PayoutId, false)
}

// ListPayouts lists the account's payouts, newest first.
func (s *Service) ListPayouts(ctx context.Context, req *accountsv1.ListPayoutsRequest) (*accountsv1.ListPayoutsResponse, error) {
	if _, err := s.load(ctx, s.pool, req.AccountId, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+payoutCols+` FROM payouts p WHERE p.account_id=$1 ORDER BY p.created_at DESC, p.id DESC`, req.AccountId)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	resp := &accountsv1.ListPayoutsResponse{}
	for rows.Next() {
		p, err := scanPayout(rows)
		if err != nil {
			return nil, internal(err)
		}
		resp.Payouts = append(resp.Payouts, p.proto())
	}
	return resp, internal0(rows.Err())
}

// GetPayoutInstruction gives the bank the decrypted destination of an open attempt. It is the
// only place the full account number leaves accounts (bank-signed route at the gateway).
func (s *Service) GetPayoutInstruction(ctx context.Context, req *accountsv1.GetPayoutInstructionRequest) (*accountsv1.PayoutInstruction, error) {
	var out *accountsv1.PayoutInstruction
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		p, st, _, err := lockAttempt(ctx, tx, req.AttemptId)
		if err != nil {
			return err
		}
		if st != "pending" && st != "settling" {
			return grpcx.Error(codes.FailedPrecondition, "payout_not_open", "this payout attempt is no longer open")
		}
		var sealed string
		if err := tx.QueryRow(ctx, `SELECT destination_ciphertext FROM accounts WHERE id=$1`, p.AccountID).Scan(&sealed); err != nil {
			return err
		}
		d, err := s.openDestination(sealed)
		if err != nil {
			return err
		}
		out = &accountsv1.PayoutInstruction{AttemptId: req.AttemptId, AmountMinor: p.Collateral + p.Credit, Currency: p.Currency,
			BankCode: d.BankCode, AccountNumber: d.AccountNumber, HolderName: d.HolderName}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// additionalPayouts pays out credit balances that arise on closed accounts (late refunds, payments
// of leftover debt): same return-window rule and stale-read guard as the close payout (review I2).
// Candidates are closed accounts with money movements that can create a credit; the ledger decides.
func (s *Service) additionalPayouts(ctx context.Context, batch int) {
	rows, err := s.pool.Query(ctx, `SELECT a.id::text FROM accounts a
		WHERE a.state='closed' AND a.destination_ciphertext IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM payouts p WHERE p.account_id=a.id AND p.status IN ('pending','settling','failed'))
		  AND (EXISTS (SELECT 1 FROM authorizations au JOIN refunds r ON r.authorization_id=au.id AND r.status='posted' WHERE au.account_id=a.id)
		    OR EXISTS (SELECT 1 FROM payments m WHERE m.account_id=a.id AND m.status='settled'))
		ORDER BY a.close_checked_at NULLS FIRST LIMIT $1`, batch)
	if err != nil {
		slog.ErrorContext(ctx, "additional payout scan failed", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		func() {
			release, ok, err := s.acquireCloseLease(ctx, id)
			if err != nil || !ok {
				return
			}
			defer release()
			acc, err := s.load(ctx, s.pool, id, false)
			if err != nil {
				return
			}
			v, err := s.available(ctx, acc)
			if err != nil || v.Receivable >= 0 {
				return
			}
			if err := s.createPayoutLocked(ctx, acc, payoutKindAdditional, 0, -v.Receivable); err != nil {
				slog.WarnContext(ctx, "additional payout failed; retried next pass", "account_id", id, "err", err)
			}
		}()
	}
}

// resolvePayouts resumes settlements whose response was lost.
func (s *Service) resolvePayouts(ctx context.Context, cutoff time.Time) {
	rows, err := s.pool.Query(ctx, `SELECT a.id::text FROM payout_attempts a WHERE a.status='settling' AND a.settling_at < $1 LIMIT 100`, cutoff)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		var p payoutRow
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			p, _, _, err = lockAttempt(ctx, tx, id)
			return err
		})
		if err != nil {
			continue
		}
		if _, err := s.SettlePayout(ctx, &accountsv1.SettlePayoutRequest{AttemptId: id, AmountMinor: p.Collateral + p.Credit, Currency: p.Currency,
			DestinationLast4: p.Last4, Status: accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED, Meta: &commonv1.RequestMeta{Actor: "accounts.reconciler"}}); err != nil {
			slog.WarnContext(ctx, "resuming payout failed; retried next pass", "attempt_id", id, "err", err)
		}
	}
}
