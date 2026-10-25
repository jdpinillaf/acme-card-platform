//go:build integration

package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

func (f *fixture) statements(t *testing.T, acc string) []*billingv1.Statement {
	t.Helper()
	r, err := f.svc.ListStatements(context.Background(), &billingv1.ListStatementsRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r.Statements
}

func (f *fixture) charges(t *testing.T, acc, kind string) []*billingv1.Charge {
	t.Helper()
	r, err := f.svc.ListCharges(context.Background(), &billingv1.ListChargesRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	var out []*billingv1.Charge
	for _, c := range r.Charges {
		if kind == "" || c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (f *fixture) entries(t *testing.T, acc string) []engine.Entry {
	t.Helper()
	var out []engine.Entry
	req := &ledgerv1.ListEntriesRequest{AccountId: acc}
	for {
		r, err := f.ledger.ListEntries(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Entries {
			out = append(out, engine.Entry{ID: e.Id, Reference: e.Reference, Type: e.EntryType, EffectiveAt: e.EffectiveAt.AsTime(), Delta: e.ReceivableDeltaMinor})
		}
		if r.NextPageToken == "" {
			return out
		}
		req.PageToken = r.NextPageToken
	}
}

// assertReconciles checks every statement against the ledger and its S3 document.
func (f *fixture) assertReconciles(t *testing.T, acc string) {
	t.Helper()
	for _, s := range f.statements(t, acc) {
		if got := f.ledgerReceivableBefore(t, acc, s.PeriodEnd.AsTime()); got != s.NewBalanceMinor {
			t.Fatalf("cycle %d: new balance %d != ledger %d", s.Cycle, s.NewBalanceMinor, got)
		}
		if b := s.PrincipalMinor + s.InterestBucketMinor + s.FeeBucketMinor; b != s.NewBalanceMinor {
			t.Fatalf("cycle %d: buckets %d != new balance %d", s.Cycle, b, s.NewBalanceMinor)
		}
		lines := s.PreviousBalanceMinor + s.PurchasesMinor - s.RefundsMinor - s.PaymentsMinor + s.ReturnsMinor + s.InterestMinor + s.FeesMinor - s.WaiversMinor - s.CollateralAppliedMinor
		if lines != s.NewBalanceMinor {
			t.Fatalf("cycle %d: lines %d != new balance %d", s.Cycle, lines, s.NewBalanceMinor)
		}
		body, err := f.store.Get(context.Background(), s.S3Key)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != s.Sha256 {
			t.Fatalf("cycle %d: S3 hash mismatch", s.Cycle)
		}
	}
}

func TestCatchUpEmptyAccount400Days(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 100000)
	f.advanceTo(t, acc.Id, acc.AccountTime.AsTime().Add(400*dayDur))
	ss := f.statements(t, acc.Id)
	want := 0
	for engine.CycleClose(acc.ActivatedAt.AsTime(), want+1).Before(f.account(t, acc.Id).AccountTime.AsTime().Add(time.Nanosecond)) {
		want++
	}
	if len(ss) != want || want < 13 {
		t.Fatalf("statements=%d want %d (≥13)", len(ss), want)
	}
	for i, s := range ss {
		if int(s.Cycle) != i+1 || s.NewBalanceMinor != 0 || s.MinimumDueMinor != 0 {
			t.Fatalf("statement %d: %+v", i, s)
		}
	}
	if n := len(f.charges(t, acc.Id, "")); n != 0 {
		t.Fatalf("charges=%d", n)
	}
	if a := f.account(t, acc.Id); a.Delinquency != "none" {
		t.Fatalf("delinquency=%s", a.Delinquency)
	}
	f.assertReconciles(t, acc.Id)
}

func TestCreditBalanceAccount400Days(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 100000)
	f.purchase(t, acc.Id, 1000)
	f.pay(t, acc.Id, 1000)
	// A refund after paying leaves a credit balance (Sprint 3).
	a := f.authorize(t, acc.Id, 500)
	if _, err := f.accounts.Capture(context.Background(), &accountsv1.CaptureRequest{AuthorizationId: a.Id, NetworkRef: f.ref("cap"), AmountMinor: 500}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.accounts.Refund(context.Background(), &accountsv1.RefundRequest{AuthorizationId: a.Id, NetworkRef: f.ref("rf"), AmountMinor: 500}); err != nil {
		t.Fatal(err)
	}
	f.advanceTo(t, acc.Id, acc.AccountTime.AsTime().Add(400*dayDur))
	if n := len(f.charges(t, acc.Id, "")); n != 0 {
		t.Fatalf("charges on a zero/credit account: %d", n)
	}
	f.assertReconciles(t, acc.Id)
}

func TestStatementReconcilesWithLedger(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	f.advanceTo(t, acc.Id, f.closeN(t, acc, 1))
	ss := f.statements(t, acc.Id)
	if len(ss) != 1 {
		t.Fatalf("statements=%d", len(ss))
	}
	s := ss[0]
	if s.PurchasesMinor != 100000 || s.InterestMinor != 0 || s.NewBalanceMinor != 100000 || s.MinimumDueMinor != 2500 || s.DueDate == nil {
		t.Fatalf("statement 1: %+v", s)
	}
	if !s.DueDate.AsTime().Equal(engine.DueDate(f.closeN(t, acc, 1))) {
		t.Fatalf("due=%v", s.DueDate.AsTime())
	}
	f.assertReconciles(t, acc.Id)
	got, err := f.svc.GetStatement(context.Background(), &billingv1.GetStatementRequest{AccountId: acc.Id, StatementId: s.Id})
	if err != nil || len(got.BodyJson) == 0 {
		t.Fatalf("get statement: %v", err)
	}
}

func TestInterestAfterGraceLost(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1, c2 := f.closeN(t, acc, 1), f.closeN(t, acc, 2)
	f.advanceTo(t, acc.Id, c2)
	fees := f.charges(t, acc.Id, "fee")
	if len(fees) != 1 || fees[0].AmountMinor != 2500 || fees[0].Cycle != 1 {
		t.Fatalf("late fee: %+v", fees)
	}
	if !fees[0].EffectiveAt.AsTime().Equal(engine.OnTimeCutoff(engine.DueDate(c1))) {
		t.Fatalf("fee effective_at=%v", fees[0].EffectiveAt.AsTime())
	}
	ss := f.statements(t, acc.Id)
	if len(ss) != 2 {
		t.Fatalf("statements=%d", len(ss))
	}
	s2 := ss[1]
	// Recompute statement 2's interest from the same journal, excluding the interest entry itself.
	var es []engine.Entry
	for _, e := range f.entries(t, acc.Id) {
		if e.Type != "interest_charged" {
			es = append(es, e)
		}
	}
	rr := engine.Replay(es, c1, c2)
	facts := []engine.StatementFacts{{Cycle: 1, Close: c1, Due: engine.DueDate(c1), Minimum: ss[0].MinimumDueMinor, NewBalance: ss[0].NewBalanceMinor}}
	// A day is past due when DPD ≥ 1 at its start: the due date itself is not (spec B-6).
	pastDue := func(day time.Time) bool { return engine.Delinquency(facts, nil, day).DPD >= 1 }
	want := engine.Interest(rr.Days, 2280, false, pastDue)
	if want == 0 || s2.InterestMinor != want || s2.FeesMinor != 2500 {
		t.Fatalf("statement 2 interest=%d fees=%d want interest %d", s2.InterestMinor, s2.FeesMinor, want)
	}
	if s2.MinimumDueMinor != engine.Minimum(s2.NewBalanceMinor, engine.Buckets{Principal: s2.PrincipalMinor}, s2.InterestMinor, s2.FeesMinor, 2500, 2500) {
		t.Fatalf("minimum 2 = %d (must include past due 2500)", s2.MinimumDueMinor)
	}
	if len(f.charges(t, acc.Id, "interest")) != 1 {
		t.Fatal("one interest charge")
	}
	f.assertReconciles(t, acc.Id)
}

func TestPendingPaymentAtDueDateThenFails(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1)
	p := f.createPayment(t, acc.Id, 2500) // received before the due date, still pending
	cut := engine.OnTimeCutoff(engine.DueDate(c1))
	f.advanceTo(t, acc.Id, cut.Add(19*dayDur))
	if n := len(f.charges(t, acc.Id, "fee")); n != 0 {
		t.Fatalf("fee while payment pending: %d", n)
	}
	if a := f.account(t, acc.Id); a.Delinquency != "none" {
		t.Fatalf("stage=%s", a.Delinquency)
	}
	f.bank(t, p, accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.CatchUpAccount(context.Background(), acc.Id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	fees := f.charges(t, acc.Id, "fee")
	if len(fees) != 1 {
		t.Fatalf("fees=%d want 1", len(fees))
	}
	now := f.account(t, acc.Id).AccountTime.AsTime()
	if d := fees[0].EffectiveAt.AsTime().Sub(now); d > time.Minute || d < -time.Minute {
		t.Fatalf("deferred fee effective_at=%v want ≈ account time %v", fees[0].EffectiveAt.AsTime(), now)
	}
	if a := f.account(t, acc.Id); a.Delinquency != "past_due" {
		t.Fatalf("stage=%s want past_due (DPD 20)", a.Delinquency)
	}
	f.assertReconciles(t, acc.Id)
}

func TestPendingPaymentSettlesLate(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1)
	p := f.createPayment(t, acc.Id, 2500)
	f.advanceTo(t, acc.Id, engine.OnTimeCutoff(engine.DueDate(c1)).Add(19*dayDur))
	f.bank(t, p, accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED)
	f.catchUp(t, acc.Id)
	if n := len(f.charges(t, acc.Id, "fee")); n != 0 {
		t.Fatalf("fee for an on-time payment confirmed late: %d", n)
	}
	if a := f.account(t, acc.Id); a.Delinquency != "none" {
		t.Fatalf("stage=%s", a.Delinquency)
	}
}

func TestBlockAndCure(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	cut := engine.OnTimeCutoff(engine.DueDate(c1))
	f.advanceTo(t, acc.Id, cut.Add(14*dayDur)) // DPD 15
	if a := f.account(t, acc.Id); a.Delinquency != "past_due" {
		t.Fatalf("stage=%s want past_due", a.Delinquency)
	}
	if d := f.authorize(t, acc.Id, 100); d.Status != accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED {
		t.Fatalf("purchase while past due: %+v", d)
	}
	sum, err := f.svc.GetSummary(context.Background(), &billingv1.GetSummaryRequest{AccountId: acc.Id})
	if err != nil || sum.PastDueMinor != 2500 || sum.Dpd != 15 || sum.Stage != "past_due" {
		t.Fatalf("summary: %v %+v", err, sum)
	}
	f.pay(t, acc.Id, sum.PastDueMinor)
	f.catchUp(t, acc.Id)
	if a := f.account(t, acc.Id); a.Delinquency != "none" {
		t.Fatalf("stage after cure=%s", a.Delinquency)
	}
	if d := f.authorize(t, acc.Id, 100); d.Status != accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED {
		t.Fatalf("purchase after cure: %+v", d)
	}
}

func TestChargeOffAtDPD60(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	cut := engine.OnTimeCutoff(engine.DueDate(c1))
	f.advanceTo(t, acc.Id, cut.Add(59*dayDur).Add(-dayDur)) // DPD 59
	if a := f.account(t, acc.Id); a.Delinquency != "past_due" {
		t.Fatalf("DPD 59 stage=%s", a.Delinquency)
	}
	got := f.advanceTo(t, acc.Id, cut.Add(59*dayDur).Add(40*dayDur)) // well past DPD 60
	if got.Delinquency != "charged_off" || got.OutstandingMinor != 0 || got.CollateralMinor >= 200000 {
		t.Fatalf("after DPD 60: %+v", got)
	}
	var at time.Time
	if err := f.ledgerDB.QueryRow(context.Background(), `SELECT effective_at FROM journal_entries WHERE reference=$1`, "chargeoff:"+acc.Id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if !at.UTC().Equal(cut.Add(59 * dayDur)) {
		t.Fatalf("charge-off business date=%v want DPD 60 %v", at.UTC(), cut.Add(59*dayDur))
	}
	interestBefore := len(f.charges(t, acc.Id, "interest"))
	feesBefore := len(f.charges(t, acc.Id, "fee"))
	f.advanceTo(t, acc.Id, got.AccountTime.AsTime().Add(70*dayDur))
	if len(f.charges(t, acc.Id, "interest")) != interestBefore || len(f.charges(t, acc.Id, "fee")) != feesBefore {
		t.Fatal("no interest or fees after charge-off")
	}
	ss := f.statements(t, acc.Id)
	last := ss[len(ss)-1]
	if last.MinimumDueMinor != 0 || last.DueDate != nil {
		t.Fatalf("balance-only statement expected: %+v", last)
	}
	f.assertReconciles(t, acc.Id)
}

func TestSettlementDuringCatchUp(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 500000)
	f.purchase(t, acc.Id, 300000)
	var ps []*accountsv1.Payment
	for i := 0; i < 10; i++ {
		ps = append(ps, f.createPayment(t, acc.Id, 1000))
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.advanceTo(t, acc.Id, f.closeN(t, acc, 2))
	}()
	go func() {
		defer wg.Done()
		for _, p := range ps {
			f.bank(t, p, accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED)
		}
	}()
	wg.Wait()
	f.catchUp(t, acc.Id)
	if len(f.statements(t, acc.Id)) < 2 {
		t.Fatal("statements")
	}
	f.assertReconciles(t, acc.Id)
}

func TestCatchUpConcurrent50(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	target := f.closeN(t, acc, 2)
	now := acc.AccountTime.AsTime()
	if _, err := f.accounts.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc.Id, Days: int32((target.Sub(now) + dayDur - 1) / dayDur)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.CatchUp(context.Background(), &billingv1.CatchUpRequest{AccountId: acc.Id}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(f.statements(t, acc.Id)); n != 2 {
		t.Fatalf("statements=%d", n)
	}
	if n := len(f.charges(t, acc.Id, "interest")); n != 1 {
		t.Fatalf("interest charges=%d", n)
	}
	if n := len(f.charges(t, acc.Id, "fee")); n != 1 {
		t.Fatalf("fees=%d", n)
	}
	if n := f.count(t, f.ledgerDB, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type IN ('interest_charged','fee_charged')`, acc.Id); n != 2 {
		t.Fatalf("ledger charge entries=%d", n)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'='statement.generated'`); n != 2 {
		t.Fatalf("statement events=%d", n)
	}
	f.assertReconciles(t, acc.Id)
}

// A saga that reserved before a close but reaches the ledger after the statement was generated
// must land in the next cycle (sealed period), so the closed statement still reconciles.
func TestLateSagaPostingLandsInNextCycle(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 50000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1)
	if _, err := f.ledger.PostPayment(context.Background(), &ledgerv1.PostPaymentRequest{AccountId: acc.Id, Reference: "payment:late-saga", AmountMinor: 1000,
		Meta: &commonv1.RequestMeta{EffectiveAt: timestamppb.New(c1.Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	f.assertReconciles(t, acc.Id)
	f.advanceTo(t, acc.Id, f.closeN(t, acc, 2))
	if s2 := f.statements(t, acc.Id)[1]; s2.PaymentsMinor != 1000 {
		t.Fatalf("late posting must appear on statement 2: %+v", s2)
	}
	f.assertReconciles(t, acc.Id)
}

func TestSweepCatchesUpEveryActiveAccount(t *testing.T) {
	f := setup(t)
	a := f.activeAccount(t, 1000)
	b := f.activeAccount(t, 1000)
	for _, id := range []string{a.Id, b.Id} {
		if _, err := f.accounts.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: id, Days: 40}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := f.svc.SweepOnce(context.Background(), slog.Default())
	if err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if len(f.statements(t, a.Id)) != 1 || len(f.statements(t, b.Id)) != 1 {
		t.Fatal("each account gets its statement")
	}
}

// Waiting catch-ups must not hold pool connections the lock holder needs (pool-starvation
// deadlock): 50 concurrent catch-ups on a 4-connection pool all finish.
func TestConcurrentCatchUpsOnSmallPool(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	if _, err := f.accounts.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc.Id, Days: 70}); err != nil {
		t.Fatal(err)
	}
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 4
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	svc := NewService(small, f.ledger, f.accounts, f.store, f.svc.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.CatchUpAccount(ctx, acc.Id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(f.statements(t, acc.Id)); n < 2 {
		t.Fatalf("statements=%d", n)
	}
}

// Catch-ups of many different accounts must not starve the pool either: each one needs a second
// connection for its writes, so a lock that pins a connection deadlocks once concurrency ≥ pool.
func TestConcurrentCatchUpsOfDistinctAccountsOnSmallPool(t *testing.T) {
	f := setup(t)
	var ids []string
	for i := 0; i < 24; i++ {
		acc := f.activeAccount(t, 1000)
		if _, err := f.accounts.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc.Id, Days: 40}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, acc.Id)
	}
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 4
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(small.Close)
	svc := NewService(small, f.ledger, f.accounts, f.store, f.svc.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.CatchUpAccount(ctx, id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if len(f.statements(t, id)) != 1 {
			t.Fatalf("account %s missing its statement", id)
		}
	}
}

// Review C1: pay in full the day before the close, the bank settles after it → no fee, grace
// kept (no interest next cycle), never past due.
func TestPaymentBeforeCloseSettledAfterIsOnTime(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 12000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1.Add(-dayDur))
	p := f.createPayment(t, acc.Id, 12000)
	f.advanceTo(t, acc.Id, c1)
	f.bank(t, p, accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED)
	f.advanceTo(t, acc.Id, f.closeN(t, acc, 2).Add(20*dayDur))
	if n := len(f.charges(t, acc.Id, "")); n != 0 {
		t.Fatalf("charges for an on-time full payment: %+v", f.charges(t, acc.Id, ""))
	}
	if a := f.account(t, acc.Id); a.Delinquency != "none" {
		t.Fatalf("stage=%s", a.Delinquency)
	}
	f.assertReconciles(t, acc.Id)
}

// Review I1: the late fee is capped at the unpaid part of the minimum.
func TestLateFeeCappedAtShortfall(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1)
	f.pay(t, acc.Id, 2400) // minimum is 2500
	f.advanceTo(t, acc.Id, engine.OnTimeCutoff(engine.DueDate(c1)))
	fees := f.charges(t, acc.Id, "fee")
	if len(fees) != 1 || fees[0].AmountMinor != 100 {
		t.Fatalf("fee: %+v want 100", fees)
	}
}

// Review M6: on the due date itself the account is not yet past due, so that day's interest base
// is principal only even when interest and fees are outstanding (spec B-6).
func TestDueDateIsNotAPastDueDay(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 500000)
	f.purchase(t, acc.Id, 300000)
	c1, c2, c3 := f.closeN(t, acc, 1), f.closeN(t, acc, 2), f.closeN(t, acc, 3)
	f.advanceTo(t, acc.Id, c1)
	f.pay(t, acc.Id, 3000)     // statement 1 minimum paid on time (grace lost: statement 2 has interest)
	f.advanceTo(t, acc.Id, c3) // statement 2 minimum missed: on due date 2 interest is outstanding
	ss := f.statements(t, acc.Id)
	if ss[1].InterestMinor == 0 {
		t.Fatal("fixture: statement 2 must carry interest")
	}
	var es []engine.Entry
	for _, e := range f.entries(t, acc.Id) {
		if e.Reference != "interest:"+acc.Id+":3" {
			es = append(es, e)
		}
	}
	credits := []engine.Credit{{ReceivedAt: c1, PostedAt: c1, Amount: 3000}}
	var facts []engine.StatementFacts
	for _, s := range ss[:2] {
		facts = append(facts, engine.StatementFacts{Cycle: int(s.Cycle), Close: s.PeriodEnd.AsTime(), Due: s.DueDate.AsTime(), Minimum: s.MinimumDueMinor, NewBalance: s.NewBalanceMinor})
	}
	pastDue := func(day time.Time) bool { return engine.Delinquency(facts, credits, day).DPD >= 1 }
	want := engine.Interest(engine.Replay(es, c2, c3).Days, 2280, false, pastDue)
	if ss[2].InterestMinor != want {
		t.Fatalf("statement 3 interest=%d want %d", ss[2].InterestMinor, want)
	}
}
