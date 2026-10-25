package engine

import (
	"bytes"
	"testing"
	"time"
)

var act = time.Date(2027, 1, 10, 15, 0, 0, 0, time.UTC)

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }

func TestCycleClose(t *testing.T) {
	end := time.Date(2027, 1, 31, 9, 0, 0, 0, time.UTC)
	if got := CycleClose(end, 1); !got.Equal(d(2027, 2, 28)) {
		t.Fatalf("31st activation close 1 = %v", got)
	}
	if got := CycleClose(end, 2); !got.Equal(d(2027, 3, 28)) {
		t.Fatalf("31st activation close 2 = %v", got)
	}
	if got := CycleClose(act, 1); !got.Equal(d(2027, 2, 10)) {
		t.Fatalf("close 1 = %v", got)
	}
	if got := CycleClose(act, 0); !got.Equal(act) {
		t.Fatalf("close 0 must be activation, got %v", got)
	}
	if got := DueDate(d(2027, 2, 10)); !got.Equal(d(2027, 3, 3)) {
		t.Fatalf("due = %v", got)
	}
	n := 0
	for CycleClose(act, n+1).Before(act.Add(400 * 24 * time.Hour)) {
		n++
	}
	if n != 13 {
		t.Fatalf("closes within 400 days = %d want 13", n)
	}
}

func e(ref, typ string, at time.Time, delta int64) Entry {
	return Entry{ID: ref, Reference: ref, Type: typ, EffectiveAt: at, Delta: delta}
}

func TestReplayAllocation(t *testing.T) {
	t1 := d(2027, 1, 11)
	es := []Entry{
		e("capture:a", "purchase_captured", t1, 100000),
		e("late_fee:x:1", "fee_charged", t1.Add(time.Hour), 2500),
		e("interest:x:1", "interest_charged", t1.Add(2*time.Hour), 1500),
	}
	if r := Replay(es, act, d(2028, 1, 1)); r.Closing.Total() != 104000 {
		t.Fatalf("total=%d", r.Closing.Total())
	}
	es = append(es, e("payment:p1", "payment_settled", t1.Add(3*time.Hour), -3000))
	if r := Replay(es, act, d(2028, 1, 1)); r.Closing != (Buckets{Principal: 100000, Interest: 1000, Fees: 0}) {
		t.Fatalf("after payment %+v", r.Closing)
	}
	es = append(es, e("refund:r1", "purchase_refunded", t1.Add(4*time.Hour), -1000))
	if r := Replay(es, act, d(2028, 1, 1)); r.Closing.Principal != 99000 {
		t.Fatalf("after refund %+v", r.Closing)
	}
	es = append(es, e("payment_return:p1", "payment_returned", t1.Add(5*time.Hour), 3000))
	if r := Replay(es, act, d(2028, 1, 1)); r.Closing != (Buckets{Principal: 99000, Interest: 1500, Fees: 2500}) || r.Closing.Total() != 103000 {
		t.Fatalf("after return %+v", r.Closing)
	}
	es = append(es, e("waiver:w1", "interest_waived", t1.Add(6*time.Hour), -500))
	r := Replay(es, act, d(2028, 1, 1))
	if r.Closing.Interest != 1000 {
		t.Fatalf("after waiver %+v", r.Closing)
	}
	want := Totals{Purchases: 100000, Refunds: 1000, Payments: 3000, Returns: 3000, Interest: 1500, Fees: 2500, Waivers: 500}
	if r.Totals != want {
		t.Fatalf("totals %+v want %+v", r.Totals, want)
	}
	if r.Opening.Total()+want.Purchases-want.Refunds-want.Payments+want.Returns+want.Interest+want.Fees-want.Waivers-want.CollateralApplied != r.Closing.Total() {
		t.Fatal("totals do not reconcile")
	}
}

func TestReplayWindowOpeningAndDays(t *testing.T) {
	es := []Entry{
		e("capture:a", "purchase_captured", d(2027, 1, 20).Add(10*time.Hour), 500),
		e("capture:b", "purchase_captured", d(2027, 2, 15).Add(10*time.Hour), 700),
	}
	r := Replay(es, d(2027, 2, 10), d(2027, 3, 10))
	if r.Opening.Principal != 500 || r.Closing.Principal != 1200 || r.Totals.Purchases != 700 {
		t.Fatalf("%+v", r)
	}
	if len(r.Days) != 28 {
		t.Fatalf("days=%d want 28", len(r.Days))
	}
	if r.Days[4].Buckets.Principal != 500 || r.Days[5].Buckets.Principal != 1200 { // Feb 14 end-of-day vs Feb 15
		t.Fatalf("day balances %+v %+v", r.Days[4], r.Days[5])
	}
}

func TestReplayExcessPaymentMakesCredit(t *testing.T) {
	r := Replay([]Entry{e("capture:a", "purchase_captured", d(2027, 1, 11), 1000), e("payment:p", "payment_settled", d(2027, 1, 12), -1500)}, act, d(2027, 2, 10))
	if r.Closing.Principal != -500 {
		t.Fatalf("%+v", r.Closing)
	}
}

func days(n int, b Buckets) []DayBalance {
	out := make([]DayBalance, n)
	for i := range out {
		out[i] = DayBalance{Day: d(2027, 2, 10).AddDate(0, 0, i), Buckets: b}
	}
	return out
}

func never(time.Time) bool  { return false }
func always(time.Time) bool { return true }

func TestInterestGraceKept(t *testing.T) {
	if got := Interest(days(30, Buckets{Principal: 100000}), 2280, true, never); got != 0 {
		t.Fatalf("grace kept: %d", got)
	}
}

func TestInterestGraceLost(t *testing.T) {
	if got := Interest(days(30, Buckets{Principal: 100000, Interest: 2000, Fees: 2500}), 2280, false, never); got != 1873 {
		t.Fatalf("grace lost: %d want 1873", got)
	}
}

func TestInterestPastDueIncludesInterestAndFees(t *testing.T) {
	if got := Interest(days(30, Buckets{Principal: 100000, Interest: 2000, Fees: 2500}), 2280, false, always); got != 1958 {
		t.Fatalf("past due: %d want 1958", got)
	}
	// Past due also overrides a kept grace period.
	if got := Interest(days(30, Buckets{Principal: 100000, Interest: 2000, Fees: 2500}), 2280, true, always); got != 1958 {
		t.Fatalf("past due with grace: %d want 1958", got)
	}
}

func TestInterestCreditBalanceZero(t *testing.T) {
	if got := Interest(days(30, Buckets{Principal: -5000}), 2280, false, always); got != 0 {
		t.Fatalf("credit balance: %d", got)
	}
}

func TestMinimum(t *testing.T) {
	cases := []struct {
		name                                 string
		newBalance                           int64
		b                                    Buckets
		interest, fees, pastDue, floor, want int64
	}{
		{"one percent plus charges", 104000, Buckets{Principal: 100000, Interest: 1500, Fees: 2500}, 1500, 2500, 0, 2500, 5000},
		{"floor capped at balance", 1000, Buckets{Principal: 1000}, 0, 0, 0, 2500, 1000},
		{"zero balance", 0, Buckets{}, 0, 0, 0, 2500, 0},
		{"credit balance", -500, Buckets{Principal: -500}, 0, 0, 0, 2500, 0},
		{"past due added", 104000, Buckets{Principal: 100000, Interest: 1500, Fees: 2500}, 1500, 2500, 5000, 2500, 10000},
		{"no floor currency", 50000, Buckets{Principal: 50000}, 0, 0, 0, 0, 500},
	}
	for _, c := range cases {
		if got := Minimum(c.newBalance, c.b, c.interest, c.fees, c.pastDue, c.floor); got != c.want {
			t.Errorf("%s: %d want %d", c.name, got, c.want)
		}
	}
}

func stmt(cycle int, close time.Time, minimum, newBalance int64) StatementFacts {
	return StatementFacts{Cycle: cycle, Close: close, Due: DueDate(close), Minimum: minimum, NewBalance: newBalance}
}

func TestGrace(t *testing.T) {
	s := stmt(1, d(2027, 2, 10), 2500, 50000)
	cut := s.Due.Add(24 * time.Hour)
	if !Grace(nil, nil) {
		t.Fatal("first cycle has grace")
	}
	if !Grace([]StatementFacts{s}, []Credit{{ReceivedAt: cut.Add(-time.Second), Amount: 50000}}) {
		t.Fatal("paid in full by due")
	}
	if Grace([]StatementFacts{s}, []Credit{{ReceivedAt: cut.Add(-time.Second), Amount: 49999}}) {
		t.Fatal("short by one")
	}
	if Grace([]StatementFacts{s}, []Credit{{ReceivedAt: cut, Amount: 50000}}) {
		t.Fatal("received after the due date")
	}
	if !Grace([]StatementFacts{s}, []Credit{{ReceivedAt: s.Close.Add(time.Hour), Amount: 50000, Pending: true}}) {
		t.Fatal("pending counts as paid")
	}
	zero := stmt(1, d(2027, 2, 10), 0, 0)
	if !Grace([]StatementFacts{zero}, nil) {
		t.Fatal("zero balance keeps grace")
	}
}

func TestDelinquency(t *testing.T) {
	s1 := stmt(1, d(2027, 2, 10), 5000, 100000)
	now := s1.Due.Add(24*time.Hour + 14*24*time.Hour)
	r := Delinquency([]StatementFacts{s1}, nil, now)
	if r.DPD != 15 || r.PastDue != 5000 || len(r.Missed) != 1 || r.Missed[0].Cycle != 1 || r.Missed[0].Shortfall != 5000 {
		t.Fatalf("missed: %+v", r)
	}
	if r := Delinquency([]StatementFacts{s1}, nil, s1.Due.Add(24*time.Hour-time.Second)); r.DPD != 0 || len(r.Missed) != 0 {
		t.Fatalf("before DPD 1: %+v", r)
	}
	if r := Delinquency([]StatementFacts{s1}, nil, s1.Due.Add(24*time.Hour)); r.DPD != 1 {
		t.Fatalf("DPD 1 at due+24h: %+v", r)
	}
	pending := []Credit{{ReceivedAt: s1.Due, Amount: 5000, Pending: true}}
	if r := Delinquency([]StatementFacts{s1}, pending, now); r.DPD != 0 || r.PastDue != 0 || len(r.Missed) != 0 {
		t.Fatalf("pending before due: %+v", r)
	}
	partialPending := []Credit{{ReceivedAt: s1.Due, Amount: 1000, Pending: true}}
	if r := Delinquency([]StatementFacts{s1}, partialPending, now); r.DPD != 15 || len(r.Missed) != 0 {
		t.Fatalf("partial pending defers the fee but not DPD: %+v", r)
	}
	s2 := stmt(2, d(2027, 3, 10), 10000, 104000)
	now2 := s2.Due.Add(24*time.Hour + 5*24*time.Hour)
	r = Delinquency([]StatementFacts{s1, s2}, nil, now2)
	wantDPD := int(now2.Sub(s1.Due.Add(24*time.Hour))/(24*time.Hour)) + 1
	if r.DPD != wantDPD || r.PastDue != 10000 || len(r.Missed) != 2 {
		t.Fatalf("two missed: %+v want dpd %d", r, wantDPD)
	}
	late := []Credit{{ReceivedAt: now.Add(-time.Hour), Amount: 5000}}
	if r := Delinquency([]StatementFacts{s1}, late, now); r.DPD != 0 || r.PastDue != 0 || len(r.Missed) != 1 {
		t.Fatalf("late payment cures, fee still owed: %+v", r)
	}
	waiver := []Credit{{ReceivedAt: now.Add(-time.Hour), Amount: 5000}}
	if r := Delinquency([]StatementFacts{s1}, waiver, now); r.DPD != 0 {
		t.Fatalf("waiver cures: %+v", r)
	}
	bo := s1
	bo.BalanceOnly = true
	if r := Delinquency([]StatementFacts{bo}, nil, now); r.DPD != 0 || len(r.Missed) != 0 {
		t.Fatalf("balance-only statements never go past due: %+v", r)
	}
}

func TestRenderStatementDeterministic(t *testing.T) {
	doc := StatementDoc{AccountID: "a", Currency: "USD", Cycle: 1, PeriodStart: act, PeriodEnd: d(2027, 2, 10), NewBalanceMinor: 100,
		Transactions: []DocLine{{EffectiveAt: d(2027, 1, 11), Type: "purchase_captured", Reference: "capture:x", AmountMinor: 100}}}
	b1, h1 := RenderStatement(doc)
	b2, h2 := RenderStatement(doc)
	if !bytes.Equal(b1, b2) || h1 != h2 || len(h1) != 64 {
		t.Fatalf("not deterministic: %s %s", h1, h2)
	}
	doc.NewBalanceMinor = 101
	if _, h3 := RenderStatement(doc); h3 == h1 {
		t.Fatal("different docs, same hash")
	}
}

// C1: a payment received before a close but posted after it is not in that statement's balance,
// so it must count toward that statement's minimum and grace (as if received at the close).
func TestPaymentReceivedBeforeClosePostedAfterCounts(t *testing.T) {
	s1 := stmt(1, d(2027, 2, 10), 2500, 12000)
	before := s1.Close.Add(-time.Hour)
	now := s1.Due.Add(24*time.Hour + 20*24*time.Hour)
	settledLate := []Credit{{ReceivedAt: before, PostedAt: s1.Close.Add(2 * time.Hour), Amount: 12000}}
	if r := Delinquency([]StatementFacts{s1}, settledLate, now); r.DPD != 0 || r.PastDue != 0 || len(r.Missed) != 0 {
		t.Fatalf("settled after the close: %+v", r)
	}
	if !Grace([]StatementFacts{s1}, settledLate) {
		t.Fatal("paid in full (posted after the close) keeps grace")
	}
	stillPending := []Credit{{ReceivedAt: before, Amount: 12000, Pending: true}}
	if r := Delinquency([]StatementFacts{s1}, stillPending, now); r.DPD != 0 || len(r.Missed) != 0 {
		t.Fatalf("pending across the close: %+v", r)
	}
	// Posted before the close it is already in the balance: it does not pay statement 1's minimum.
	reflected := []Credit{{ReceivedAt: before, PostedAt: before.Add(time.Minute), Amount: 1000}}
	if r := Delinquency([]StatementFacts{s1}, reflected, now); r.PastDue != 2500 {
		t.Fatalf("reflected payment counted twice: %+v", r)
	}
}

// I1: the late fee is capped at the unpaid part of the minimum.
func TestMissedReportsShortfall(t *testing.T) {
	s1 := stmt(1, d(2027, 2, 10), 2500, 100000)
	paid := []Credit{{ReceivedAt: s1.Close.Add(24 * time.Hour), PostedAt: s1.Close.Add(25 * time.Hour), Amount: 2400}}
	r := Delinquency([]StatementFacts{s1}, paid, s1.Due.Add(48*time.Hour))
	if len(r.Missed) != 1 || r.Missed[0].Cycle != 1 || r.Missed[0].Shortfall != 100 {
		t.Fatalf("missed: %+v", r.Missed)
	}
}

// A payment received inside statement 1's on-time window that is still pending at close 2 pays
// statement 1; it must not be moved to statement 2's window.
func TestPendingPaymentInPreviousWindowStaysThere(t *testing.T) {
	s1 := stmt(1, d(2027, 2, 10), 2500, 100000)
	s2 := stmt(2, d(2027, 3, 10), 5000, 101000)
	pending := []Credit{{ReceivedAt: s1.Close.Add(time.Hour), Amount: 2500, Pending: true}}
	r := Delinquency([]StatementFacts{s1, s2}, pending, s1.Due.Add(24*time.Hour+20*24*time.Hour))
	if r.DPD != 0 || len(r.Missed) != 0 {
		t.Fatalf("statement 1 was paid on time (pending): %+v", r)
	}
	if !Grace([]StatementFacts{s1}, []Credit{{ReceivedAt: s1.Close.Add(-time.Hour), Amount: 100000, Pending: true}}) {
		t.Fatal("full payment before close 1, pending across it, keeps grace")
	}
}
