package billing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

const day = 24 * time.Hour

// Delinquency thresholds (spec I-2).
const (
	blockDPD     = 15
	chargeOffDPD = 60
)

// CatchUp runs every due billing event for the account up to its account time.
func (s *Service) CatchUp(ctx context.Context, req *billingv1.CatchUpRequest) (*billingv1.CatchUpResponse, error) {
	if err := validAccount(req.AccountId); err != nil {
		return nil, err
	}
	n, err := s.CatchUpAccount(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	return &billingv1.CatchUpResponse{StatementsGenerated: int32(n)}, nil
}

// lockPoll is how long a catch-up waits before retrying a busy account lease.
const lockPoll = 20 * time.Millisecond

// leaseTTL bounds how long a crashed catch-up keeps its account; every effect is idempotent, so a
// takeover after expiry is safe.
const leaseTTL = 2 * time.Minute

// acquireLease takes the account's catch-up lease with one autocommit UPDATE. Nothing pins a pool
// connection while the catch-up runs: a lock held through a transaction made every catch-up need
// two connections at once, which deadlocked the pool under load (same account or not).
func (s *Service) acquireLease(ctx context.Context, accountID string) (release func(), err error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO billing_accounts (account_id) VALUES ($1) ON CONFLICT DO NOTHING`, accountID); err != nil {
		return nil, internal(err)
	}
	owner := uuid.New()
	for {
		tag, err := s.pool.Exec(ctx, `UPDATE billing_accounts SET lease_owner=$2, lease_until=now() + $3::interval, updated_at=now()
			WHERE account_id=$1 AND (lease_until IS NULL OR lease_until < now())`, accountID, owner, leaseTTL.String())
		if err != nil {
			return nil, internal(err)
		}
		if tag.RowsAffected() == 1 {
			return func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if _, err := s.pool.Exec(ctx, `UPDATE billing_accounts SET lease_until=NULL, lease_owner=NULL WHERE account_id=$1 AND lease_owner=$2`, accountID, owner); err != nil {
					slog.WarnContext(ctx, "billing lease release failed; it expires on its own", "account_id", accountID, "err", err)
				}
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, internal(ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// run is one catch-up of one account, under its billing_accounts lock.
type run struct {
	s          *Service
	acc        *accountsv1.Account
	act, now   time.Time
	stmts      []statementRow
	facts      []engine.StatementFacts
	credits    []engine.Credit
	entries    []engine.Entry
	dirty      bool // a charge was posted since entries were loaded
	charged    bool // any charge posted in this run
	chargedOff bool
	stage      string
	generated  int
}

// CatchUpAccount processes, in chronological order, every cycle close, late fee, block and
// charge-off due up to account time (spec C-9). Concurrent catch-ups of one account serialize on
// its billing_accounts row; every effect is idempotent on a deterministic reference.
func (s *Service) CatchUpAccount(ctx context.Context, accountID string) (int, error) {
	release, err := s.acquireLease(ctx, accountID)
	if err != nil {
		return 0, err
	}
	defer release()
	acc, err := s.account(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if acc.ActivatedAt == nil || (acc.State != accountsv1.AccountState_ACCOUNT_STATE_ACTIVE && acc.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING) {
		return 0, nil
	}
	if err := s.resumePending(ctx, accountID); err != nil {
		return 0, err
	}
	if err := s.resumeWaivers(ctx, accountID); err != nil {
		return 0, err
	}
	r := &run{s: s, acc: acc, act: acc.ActivatedAt.AsTime(), now: acc.AccountTime.AsTime(), stage: acc.Delinquency,
		chargedOff: acc.Delinquency == "charged_off"}
	if err := r.load(ctx); err != nil {
		return 0, err
	}
	if err := r.execute(ctx); err != nil {
		return r.generated, err
	}
	return r.generated, nil
}

func (r *run) load(ctx context.Context) error {
	var err error
	if r.stmts, err = r.s.loadStatements(ctx, r.acc.Id); err != nil {
		return internal(err)
	}
	for _, st := range r.stmts {
		r.facts = append(r.facts, st.facts())
	}
	if r.credits, err = r.s.credits(ctx, r.acc.Id); err != nil {
		return err
	}
	r.entries, err = r.s.entries(ctx, r.acc.Id)
	return err
}

func (r *run) reloadEntries(ctx context.Context) error {
	if !r.dirty {
		return nil
	}
	var err error
	r.entries, err = r.s.entries(ctx, r.acc.Id)
	r.dirty = false
	return err
}

func (r *run) execute(ctx context.Context) error {
	if n := len(r.stmts); n > 0 && r.stmts[n-1].Final {
		return nil // after the final statement nothing is billed any more (ADR-020)
	}
	last := len(r.stmts)
	cursor := engine.CycleClose(r.act, last)
	for {
		next := engine.CycleClose(r.act, last+1)
		limit := next
		if r.now.Before(limit) {
			limit = r.now
		}
		// Day boundaries since the last close, in order: fees, block and charge-off fire on the
		// day they fall due (re-evaluating already-processed days is idempotent).
		for t := nextBoundary(cursor); !t.After(limit); t = t.Add(day) {
			if err := r.delinquencyAt(ctx, t); err != nil {
				return err
			}
		}
		if next.After(r.now) {
			break
		}
		if err := r.close(ctx, last+1, engine.CycleClose(r.act, last), next, false); err != nil {
			return err
		}
		last++
		cursor = next
	}
	if err := r.delinquencyAt(ctx, r.now); err != nil {
		return err
	}
	if !r.chargedOff && r.stage != r.acc.Delinquency {
		if _, err := r.s.accounts.SetDelinquency(ctx, &accountsv1.SetDelinquencyRequest{AccountId: r.acc.Id, Stage: r.stage,
			Meta: &commonv1.RequestMeta{Actor: "billing"}}); err != nil {
			return err
		}
	}
	if r.charged {
		if _, err := r.s.accounts.CheckExposure(ctx, &accountsv1.CheckExposureRequest{AccountId: r.acc.Id, Meta: &commonv1.RequestMeta{Actor: "billing"}}); err != nil {
			slog.WarnContext(ctx, "exposure check after billing charges failed; the restriction sweep retries", "account_id", r.acc.Id, "err", err)
		}
	}
	return nil
}

// owedAt is the receivable from entries effective before t.
func (r *run) owedAt(t time.Time) int64 {
	var owed int64
	for _, e := range r.entries {
		if e.EffectiveAt.Before(t) {
			owed += e.Delta
		}
	}
	return owed
}

// nextBoundary is the first 00:00 UTC strictly after t.
func nextBoundary(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(day)
}

// delinquencyAt applies what is due at t: late fees for missed statements, then the stage.
func (r *run) delinquencyAt(ctx context.Context, t time.Time) error {
	if r.chargedOff {
		return nil
	}
	d := engine.CapToOwed(engine.Delinquency(r.facts, r.credits, t), r.owedAt(t))
	for _, m := range d.Missed {
		cycle := m.Cycle
		f := r.facts[cycle-1]
		amount := min(r.s.cfg.Fixed[r.acc.Currency], m.Shortfall) // capped at the unpaid minimum (spec I-2)
		if amount <= 0 {
			continue
		}
		// On its due day in chronological order the fee is dated then; a fee that could only be
		// decided later (a pending payment failed) is dated now (spec B-2).
		effective := t
		if !t.Equal(engine.OnTimeCutoff(f.Due)) {
			effective = r.now
		}
		posted, err := r.s.postCharge(ctx, r.acc.Id, "fee", cycle, fmt.Sprintf("late_fee:%s:%d", r.acc.Id, cycle), amount, effective)
		if err != nil {
			return err
		}
		if posted {
			r.dirty, r.charged = true, true
		}
	}
	switch {
	case d.DPD >= chargeOffDPD:
		effective := t
		if d.DPD != chargeOffDPD || !t.Equal(t.Truncate(day)) {
			effective = r.now
		}
		if _, err := r.s.accounts.ChargeOff(ctx, &accountsv1.ChargeOffRequest{AccountId: r.acc.Id,
			Meta: &commonv1.RequestMeta{Actor: "billing", EffectiveAt: timestamppb.New(effective)}}); err != nil {
			return err // e.g. a card operation in flight: the next catch-up retries
		}
		r.chargedOff, r.dirty = true, true
	case d.DPD >= blockDPD:
		r.stage = "past_due"
	default:
		r.stage = "none"
	}
	return nil
}

// close generates statement n for [prevClose, closeAt) (spec C-9 / plan "sealed periods"):
// interest, seal, totals, S3, row. final marks the off-cycle statement cut at close (ADR-020).
func (r *run) close(ctx context.Context, n int, prevClose, closeAt time.Time, final bool) error {
	acc := r.acc.Id
	if err := r.reloadEntries(ctx); err != nil {
		return err
	}
	balanceOnly := r.chargedOff
	grace := engine.Grace(r.facts, r.credits)
	interestRef := fmt.Sprintf("interest:%s:%d", acc, n)
	if !balanceOnly {
		var es []engine.Entry
		for _, e := range r.entries {
			if e.Reference != interestRef { // a resumed close must compute the same amount
				es = append(es, e)
			}
		}
		facts := r.facts
		// A day is past due when DPD ≥ 1 at its start; the due date itself is not (spec B-6).
		pastDue := func(d time.Time) bool { return engine.CapToOwed(engine.Delinquency(facts, r.credits, d), r.owedAt(d)).DPD >= 1 }
		if interest := engine.Interest(engine.Replay(es, prevClose, closeAt).Days, r.s.cfg.APRBps, grace, pastDue); interest > 0 {
			posted, err := r.s.postCharge(ctx, acc, "interest", n, interestRef, interest, closeAt.Add(-time.Second))
			if err != nil {
				return err
			}
			r.charged = r.charged || posted
		}
	}
	if _, err := r.s.ledger.SealPeriod(ctx, &ledgerv1.SealPeriodRequest{AccountId: acc, SealedThrough: timestamppb.New(closeAt)}); err != nil {
		return err
	}
	r.dirty = true
	if err := r.reloadEntries(ctx); err != nil {
		return err
	}
	rr := engine.Replay(r.entries, prevClose, closeAt)
	var pastDueAtClose, minimum int64
	var due *time.Time
	if !balanceOnly {
		pastDueAtClose = engine.Delinquency(r.facts, r.credits, closeAt).PastDue
		minimum = engine.Minimum(rr.Closing.Total(), rr.Closing, rr.Totals.Interest, rr.Totals.Fees, pastDueAtClose, r.s.cfg.Fixed[r.acc.Currency])
		d := engine.DueDate(closeAt)
		due = &d
	}
	var pending int64
	for _, c := range r.credits {
		if c.Pending && c.ReceivedAt.Before(closeAt) {
			pending += c.Amount
		}
	}
	doc := engine.StatementDoc{AccountID: acc, Currency: r.acc.Currency, Cycle: n, PeriodStart: prevClose, PeriodEnd: closeAt, DueDate: due,
		PreviousBalanceMinor: rr.Opening.Total(), PurchasesMinor: rr.Totals.Purchases, RefundsMinor: rr.Totals.Refunds,
		PaymentsMinor: rr.Totals.Payments, ReturnsMinor: rr.Totals.Returns, InterestMinor: rr.Totals.Interest, FeesMinor: rr.Totals.Fees,
		WaiversMinor: rr.Totals.Waivers, CollateralAppliedMinor: rr.Totals.CollateralApplied, NewBalanceMinor: rr.Closing.Total(),
		MinimumDueMinor: minimum, PastDueMinor: pastDueAtClose, Grace: grace, APRBps: r.s.cfg.APRBps, PendingPaymentsMinor: pending,
		BalanceOnly: balanceOnly, Final: final,
		Buckets:     engine.DocBuckets{PrincipalMinor: rr.Closing.Principal, InterestMinor: rr.Closing.Interest, FeesMinor: rr.Closing.Fees}}
	for _, e := range r.entries {
		if e.Delta != 0 && !e.EffectiveAt.Before(prevClose) && e.EffectiveAt.Before(closeAt) {
			doc.Transactions = append(doc.Transactions, engine.DocLine{EffectiveAt: e.EffectiveAt, Type: e.Type, Reference: e.Reference, AmountMinor: e.Delta})
		}
	}
	body, sum := engine.RenderStatement(doc)
	key := fmt.Sprintf("statements/%s/%d.json", acc, n)
	if err := r.s.store.Put(ctx, key, body); err != nil {
		return internal(err)
	}
	st := statementRow{ID: uuid.Must(uuid.NewV7()).String(), AccountID: acc, Cycle: n, Currency: r.acc.Currency, PeriodStart: prevClose,
		PeriodEnd: closeAt, DueDate: due, Previous: doc.PreviousBalanceMinor, Purchases: doc.PurchasesMinor, Refunds: doc.RefundsMinor,
		Payments: doc.PaymentsMinor, Returns: doc.ReturnsMinor, Interest: doc.InterestMinor, Fees: doc.FeesMinor, Waivers: doc.WaiversMinor,
		Collateral: doc.CollateralAppliedMinor, NewBalance: doc.NewBalanceMinor, Minimum: minimum, PastDue: pastDueAtClose,
		Principal: rr.Closing.Principal, IBucket: rr.Closing.Interest, FBucket: rr.Closing.Fees, Grace: grace, BalanceOnly: balanceOnly,
		S3Key: key, SHA256: sum, Final: final, GeneratedAt: time.Now().UTC()}
	err := pgx.BeginFunc(ctx, r.s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO statements (id, account_id, cycle, currency, period_start, period_end, due_date, previous_balance_minor,
			purchases_minor, refunds_minor, payments_minor, returns_minor, interest_minor, fees_minor, waivers_minor, collateral_applied_minor,
			new_balance_minor, minimum_due_minor, past_due_minor, principal_minor, interest_bucket_minor, fee_bucket_minor, grace, balance_only, s3_key, sha256, final)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)
			ON CONFLICT (account_id, cycle) DO NOTHING`,
			st.ID, acc, n, st.Currency, prevClose, closeAt, due, st.Previous, st.Purchases, st.Refunds, st.Payments, st.Returns, st.Interest,
			st.Fees, st.Waivers, st.Collateral, st.NewBalance, minimum, pastDueAtClose, st.Principal, st.IBucket, st.FBucket, grace, balanceOnly, key, sum, final)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return outbox.Enqueue(ctx, tx, event("statement.generated", "statement", st.ID, doc))
	})
	if err != nil {
		return internal(err)
	}
	r.stmts = append(r.stmts, st)
	r.facts = append(r.facts, st.facts())
	r.generated++
	return nil
}
