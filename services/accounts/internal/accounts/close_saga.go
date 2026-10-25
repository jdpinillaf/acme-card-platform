package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/secretbox"
)

// Close saga stages (ADR-020), in order.
const (
	StageInFlight        = "in_flight"
	StageFinalStatement  = "final_statement"
	StageAwaitingDue     = "awaiting_due"
	StageApplying        = "applying"
	StageAwaitingReturns = "awaiting_returns"
	StagePayout          = "payout"
	StageClosed          = "closed"
)

var (
	bankCodeRE      = regexp.MustCompile(`^[A-Za-z0-9]{3,11}$`)
	accountNumberRE = regexp.MustCompile(`^[0-9]{4,34}$`)
)

type destination struct {
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	HolderName    string `json:"holder_name"`
}

func validDestination(d *accountsv1.Destination) (destination, error) {
	if d == nil || !bankCodeRE.MatchString(d.BankCode) || !accountNumberRE.MatchString(d.AccountNumber) ||
		utf8.RuneCountInString(d.HolderName) < 1 || utf8.RuneCountInString(d.HolderName) > 100 {
		return destination{}, grpcx.Error(codes.InvalidArgument, "invalid_destination",
			"destination needs bank_code (3-11 alphanumerics), account_number (4-34 digits) and holder_name (1-100 characters)")
	}
	return destination{BankCode: d.BankCode, AccountNumber: d.AccountNumber, HolderName: d.HolderName}, nil
}

func (s *Service) openDestination(sealed string) (destination, error) {
	var d destination
	plain, err := s.box.Open(sealed)
	if err != nil {
		return d, fmt.Errorf("open destination: %w", err)
	}
	return d, json.Unmarshal(plain, &d)
}

// CloseAccount blocks the card at once (state closing) and starts the close saga. A repeat with
// the same destination replays; a different destination is refused.
func (s *Service) CloseAccount(ctx context.Context, req *accountsv1.CloseAccountRequest) (*accountsv1.GetAccountResponse, error) {
	if s.box == nil {
		return nil, grpcx.Error(codes.FailedPrecondition, "close_unavailable", "payout destination encryption is not configured")
	}
	dest, err := validDestination(req.Destination)
	if err != nil {
		return nil, err
	}
	var acc accountRow
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, req.AccountId, true)
		if err != nil {
			return err
		}
		acc = before
		switch before.State {
		case StateClosing:
			var sealed string
			if err := tx.QueryRow(ctx, `SELECT destination_ciphertext FROM accounts WHERE id=$1`, before.ID).Scan(&sealed); err != nil {
				return err
			}
			stored, err := s.openDestination(sealed)
			if err != nil {
				return err
			}
			if stored != dest {
				return grpcx.Error(codes.FailedPrecondition, "close_already_requested", "a close was already requested with another destination")
			}
			return nil // replay
		case StateClosed, StateCancelled:
			return grpcx.Error(codes.FailedPrecondition, "account_closed", "account is closed")
		case StatePendingCollateral:
			return grpcx.Error(codes.FailedPrecondition, "not_closable", "an account waiting for collateral is cancelled, not closed")
		}
		next, err := Transition(before.State, EventRequestClose)
		if err != nil {
			return grpcx.Error(codes.FailedPrecondition, "not_closable", err.Error())
		}
		plain, _ := json.Marshal(dest)
		sealed, err := s.box.Seal(plain)
		if err != nil {
			return err
		}
		if err := s.voidPendingDeposits(ctx, tx, before.ID, req.Meta); err != nil {
			return err
		}
		actor := ""
		if req.Meta != nil {
			actor = req.Meta.Actor
		}
		if acc, err = scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET state=$2, close_stage=$3, close_stage_at=$4, close_requested_at=$5,
			close_requested_by=$6, close_reason=nullif($7,''), destination_ciphertext=$8, destination_last4=$9, version=version+1, updated_at=now()
			WHERE id=$1 RETURNING `+accountCols, before.ID, next, StageInFlight, s.clock.Now(), s.accountNow(before), actor, req.Reason, sealed,
			secretbox.Last4(dest.AccountNumber))); err != nil {
			return err
		}
		ev := event("account.close_requested", "account", acc.ID, req.Meta)
		ev.Before, ev.After = snapshot(before), snapshot(acc) // accountRow carries only the last 4 digits
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return nil, internal(err)
	}
	return s.accountResponse(ctx, acc)
}

// AdvanceClose runs close-saga steps until one cannot progress yet.
func (s *Service) AdvanceClose(ctx context.Context, req *accountsv1.AdvanceCloseRequest) (*accountsv1.GetAccountResponse, error) {
	for i := 0; i < 8; i++ {
		moved, err := s.closeStep(ctx, req.AccountId)
		if err != nil {
			return nil, err
		}
		if !moved {
			break
		}
	}
	acc, err := s.load(ctx, s.pool, req.AccountId, false)
	if err != nil {
		return nil, err
	}
	return s.accountResponse(ctx, acc)
}

// setStage moves the saga from one stage to the next (compare-and-set, evented).
func (s *Service) setStage(ctx context.Context, accountID, from, to string, extra func(tx pgx.Tx) error) (bool, error) {
	moved := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := s.load(ctx, tx, accountID, true)
		if err != nil || before.CloseStage == nil || *before.CloseStage != from {
			return err
		}
		if extra != nil {
			if err := extra(tx); err != nil {
				return err
			}
		}
		after, err := scanAccount(tx.QueryRow(ctx, `UPDATE accounts SET close_stage=$2, close_stage_at=$3, version=version+1, updated_at=now()
			WHERE id=$1 RETURNING `+accountCols, accountID, to, s.clock.Now()))
		if err != nil {
			return err
		}
		moved = true
		ev := event("account.close_stage_changed", "account", accountID, &commonv1.RequestMeta{Actor: "accounts.close_saga"})
		ev.Before, ev.After = snapshot(map[string]any{"close_stage": from}), snapshot(map[string]any{"close_stage": to, "account": after})
		return outbox.Enqueue(ctx, tx, ev)
	})
	return moved, internal0(err)
}

// closeStep runs one stage; moved reports whether the saga advanced.
func (s *Service) closeStep(ctx context.Context, accountID string) (bool, error) {
	acc, err := s.load(ctx, s.pool, accountID, false)
	if err != nil {
		return false, err
	}
	if acc.State != StateClosing || acc.CloseStage == nil {
		return false, nil
	}
	switch *acc.CloseStage {
	case StageInFlight:
		busy, err := s.inFlight(ctx, acc.ID)
		if err != nil || busy {
			return false, err
		}
		return s.setStage(ctx, acc.ID, StageInFlight, StageFinalStatement, nil)
	case StageFinalStatement:
		if s.billing == nil {
			return false, nil
		}
		r, err := s.billing.FinalizeStatement(ctx, &billingv1.FinalizeStatementRequest{AccountId: acc.ID})
		if err != nil {
			return false, err
		}
		due := r.DueDate.AsTime()
		return s.setStage(ctx, acc.ID, StageFinalStatement, StageAwaitingDue, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE accounts SET final_due_at=$2 WHERE id=$1`, acc.ID, due)
			return err
		})
	case StageAwaitingDue:
		v, err := s.available(ctx, acc)
		if err != nil {
			return false, err
		}
		if v.Receivable <= 0 {
			return s.setStage(ctx, acc.ID, StageAwaitingDue, StageAwaitingReturns, nil)
		}
		ready, err := s.dueReached(ctx, acc)
		if err != nil || !ready {
			return false, err
		}
		return s.setStage(ctx, acc.ID, StageAwaitingDue, StageApplying, nil)
	case StageApplying:
		if err := s.applyCollateralAtClose(ctx, acc); err != nil {
			return false, err
		}
		return s.setStage(ctx, acc.ID, StageApplying, StageAwaitingReturns, nil)
	case StageAwaitingReturns:
		v, err := s.available(ctx, acc)
		if err != nil {
			return false, err
		}
		if v.Receivable > 0 && acc.Collateral > 0 { // a return re-raised the debt after the due date
			return s.setStage(ctx, acc.ID, StageAwaitingReturns, StageApplying, nil)
		}
		var open int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE account_id=$1 AND
			(status='returning' OR (status='settled' AND settled_at > $2))`, acc.ID, s.clock.Now().Add(-s.returnWindow)).Scan(&open); err != nil {
			return false, internal(err)
		}
		if open > 0 {
			return false, nil
		}
		return s.setStage(ctx, acc.ID, StageAwaitingReturns, StagePayout, nil)
	case StagePayout:
		return s.payoutStep(ctx, acc)
	}
	return false, nil
}

// inFlight reports work that must finish before the final statement: authorizations not yet
// captured/reversed/expired, payments the bank has not answered, settling deposits, pending
// refunds and a pending charge-off.
func (s *Service) inFlight(ctx context.Context, accountID string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM authorizations WHERE account_id=$1 AND (status IN ('approved','pending') OR pending_op IS NOT NULL)) +
		(SELECT count(*) FROM payments WHERE account_id=$1 AND status IN ('pending','settling')) +
		(SELECT count(*) FROM deposits WHERE account_id=$1 AND status='settling') +
		(SELECT count(*) FROM refunds r JOIN authorizations a ON a.id=r.authorization_id WHERE a.account_id=$1 AND r.status='pending') +
		(SELECT count(*) FROM accounts WHERE id=$1 AND charge_off_pending_at IS NOT NULL)`, accountID).Scan(&n)
	return n > 0, internal0(err)
}

// dueReached: account time is past the final due date's on-time cutoff and no payment received
// before that cutoff is still waiting for the bank (ADR-019 rule); charged-off accounts skip it.
func (s *Service) dueReached(ctx context.Context, acc accountRow) (bool, error) {
	if acc.Delinquency == DelinquencyChargedOff {
		return true, nil
	}
	var due *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT final_due_at FROM accounts WHERE id=$1`, acc.ID).Scan(&due); err != nil || due == nil {
		return false, internal0(err)
	}
	cutoff := due.Add(24 * time.Hour)
	if s.accountNow(acc).Before(cutoff) {
		return false, nil
	}
	var pending int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE account_id=$1 AND status IN ('pending','settling') AND received_at < $2`,
		acc.ID, cutoff).Scan(&pending); err != nil {
		return false, internal(err)
	}
	return pending == 0, nil
}

// applyCollateralAtClose applies min(debt, collateral) to the debt, dated at the final due cutoff.
// The amount and reference sequence are fixed before the ledger call so a retry posts the same
// journal; a later application (after a return) uses the next sequence.
func (s *Service) applyCollateralAtClose(ctx context.Context, acc accountRow) error {
	v, err := s.available(ctx, acc)
	if err != nil {
		return err
	}
	var amount int64
	var seq int
	var due *time.Time
	err = s.pool.QueryRow(ctx, `UPDATE accounts SET close_apply_amount = coalesce(close_apply_amount, LEAST(GREATEST($2::bigint,0), collateral_minor))
		WHERE id=$1 RETURNING close_apply_amount, close_apply_seq + 1, final_due_at`, acc.ID, v.Receivable).Scan(&amount, &seq, &due)
	if err != nil {
		return internal(err)
	}
	if amount > 0 {
		at := s.accountNow(acc)
		if due != nil && acc.Delinquency != DelinquencyChargedOff {
			at = due.Add(24 * time.Hour)
		}
		if _, err := s.ledger.PostCollateralApplication(ctx, &ledgerv1.PostCollateralApplicationRequest{AccountId: acc.ID,
			Reference: fmt.Sprintf("closeout:%s:%d", acc.ID, seq), AmountMinor: amount,
			Meta: withEffective(&commonv1.RequestMeta{Actor: "accounts.close_saga"}, &at)}); err != nil {
			return err
		}
	}
	return internal0(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE accounts SET collateral_minor = collateral_minor - $2, credit_limit_minor = credit_limit_minor - $2,
			close_apply_amount = NULL, close_apply_seq = $3 WHERE id=$1 AND close_apply_amount IS NOT NULL`, acc.ID, amount, seq)
		return err
	}))
}

// AdvanceClosing runs the saga for every closing account (worker loop).
func (s *Service) AdvanceClosing(ctx context.Context, batch int) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM accounts WHERE state='closing' ORDER BY close_stage_at LIMIT $1`, batch)
	if err != nil {
		return err
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
		if _, err := s.AdvanceClose(ctx, &accountsv1.AdvanceCloseRequest{AccountId: id}); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "close saga step failed; retried next pass", "account_id", id, "err", err)
		}
	}
	s.additionalPayouts(ctx, batch)
	return nil
}
