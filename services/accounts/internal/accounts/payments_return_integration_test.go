//go:build integration

package accounts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func (f *fixture) settledPayment(t *testing.T, acc string, amt int64, ref string) *accountsv1.Payment {
	t.Helper()
	p, err := f.createPayment(acc, amt, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.SettlePayment(context.Background(), settlePay(p.Payment, ref, paySettled))
	if err != nil {
		t.Fatal(err)
	}
	return r.Payment
}

func (f *fixture) returnPayment(p *accountsv1.Payment, ref string) (*accountsv1.PaymentResponse, error) {
	return f.svc.ReturnPayment(context.Background(), &accountsv1.ReturnPaymentRequest{PaymentId: p.Id, ReturnRef: ref, AmountMinor: p.Amount.AmountMinor})
}

func (f *fixture) spendCaptured(t *testing.T, acc, ref string, amt int64) {
	t.Helper()
	a := f.authorize(t, acc, ref, amt).Authorization
	if a.Status != approved {
		t.Fatalf("authorize %s: %+v", ref, a)
	}
	if _, err := f.capture(a.Id, "cap-"+ref, amt); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) restricted(t *testing.T, acc string) string {
	g, _ := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc})
	return g.Account.RestrictedReason
}

func TestReturnWithin5DaysRestrictsWhenOverLimit(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-ret1", 1000)
	f.spendCaptured(t, acc.Id, "ret1-a", 1000)
	pay := f.settledPayment(t, acc.Id, 1000, "bank-ret1")
	f.spendCaptured(t, acc.Id, "ret1-b", 1000)
	r, err := f.returnPayment(pay, "ret-ret1")
	if err != nil || r.Payment.Status != accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNED || r.Account.OutstandingMinor != 2000 {
		t.Fatalf("%v %+v", err, r)
	}
	if f.restricted(t, acc.Id) != "payment_returned" || f.events(t, "card.restricted") != 1 {
		t.Fatal("over-limit return must restrict")
	}
	if a := f.authorize(t, acc.Id, "ret1-c", 1).Authorization; a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_PAYMENT_RETURNED {
		t.Fatalf("decline: %+v", a)
	}
	if r2, err := f.returnPayment(pay, "ret-ret1"); err != nil || !r2.Replayed {
		t.Fatalf("return replay: %v", err)
	}
	f.settledPayment(t, acc.Id, 1000, "bank-ret1-b")
	if f.restricted(t, acc.Id) != "" || f.events(t, "card.unrestricted") != 1 {
		t.Fatal("paying back within the limit must lift the restriction")
	}
}

func TestReturnWindowExpired(t *testing.T) {
	f, c := txFixture(t)
	acc := f.activeAccount(t, "cust-ret2", 1000)
	f.spendCaptured(t, acc.Id, "ret2-a", 500)
	pay := f.settledPayment(t, acc.Id, 500, "bank-ret2")
	c.Advance(ReturnWindow + time.Second)
	if _, err := f.returnPayment(pay, "ret-ret2"); grpcx.Reason(err) != "return_window_expired" {
		t.Fatalf("late return: %v", err)
	}
}

func TestReturnUnderLimitDoesNotRestrict(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-ret3", 1000)
	f.spendCaptured(t, acc.Id, "ret3-a", 300)
	pay := f.settledPayment(t, acc.Id, 300, "bank-ret3")
	if _, err := f.returnPayment(pay, "ret-ret3"); err != nil {
		t.Fatal(err)
	}
	if f.restricted(t, acc.Id) != "" {
		t.Fatal("exposure 300 ≤ 1000 must not restrict")
	}
	g, _ := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if g.Account.OutstandingMinor != 300 || g.Account.AvailableCreditMinor != 700 {
		t.Fatalf("%+v", g.Account)
	}
	other, _ := f.createPayment(acc.Id, 300, nil)
	if _, err := f.svc.ReturnPayment(context.Background(), &accountsv1.ReturnPaymentRequest{PaymentId: other.Payment.Id, ReturnRef: "r-x", AmountMinor: 300}); grpcx.Reason(err) != "not_settled" {
		t.Fatalf("returning an unsettled payment: %v", err)
	}
	if _, err := f.svc.ReturnPayment(context.Background(), &accountsv1.ReturnPaymentRequest{PaymentId: pay.Id, ReturnRef: "ret-ret3", AmountMinor: 299}); grpcx.Reason(err) != "amount_mismatch" {
		t.Fatalf("return replay with different amount: %v", err)
	}
}

func TestReturnAfterLiftRestrictsAgain(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-ret4", 1000)
	f.spendCaptured(t, acc.Id, "ret4-a", 1000)
	p1 := f.settledPayment(t, acc.Id, 1000, "bank-ret4-1")
	f.spendCaptured(t, acc.Id, "ret4-b", 1000)
	if _, err := f.returnPayment(p1, "ret-ret4-1"); err != nil { // debt 2000 → restricted
		t.Fatal(err)
	}
	if f.restricted(t, acc.Id) != "payment_returned" {
		t.Fatal("precondition: first return restricts")
	}
	p2 := f.settledPayment(t, acc.Id, 1000, "bank-ret4-2")
	if f.restricted(t, acc.Id) != "" {
		t.Fatal("lifted after paying back to the limit")
	}
	if _, err := f.returnPayment(p2, "ret-ret4-2"); err != nil {
		t.Fatal(err)
	}
	if f.restricted(t, acc.Id) != "payment_returned" {
		t.Fatal("a second return over the limit must restrict again")
	}
}

func TestReturnRacingPurchaseNeverOverLimitUnrestricted(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		acc := f.activeAccount(t, fmt.Sprintf("cust-ret5-%d", i), 1000)
		f.spendCaptured(t, acc.Id, fmt.Sprintf("ret5-a-%d", i), 1000)
		pay := f.settledPayment(t, acc.Id, 1000, fmt.Sprintf("bank-ret5-%d", i))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = f.returnPayment(pay, fmt.Sprintf("ret-ret5-%d", i)) }()
		go func() {
			defer wg.Done()
			_, _ = f.svc.Authorize(ctx, authReq(acc.Id, fmt.Sprintf("ret5-b-%d", i), 1000))
		}()
		wg.Wait()
		v, _ := f.svc.available(ctx, accountRow{ID: acc.Id})
		over := v.Holds+v.Receivable > 1000
		if over && f.restricted(t, acc.Id) == "" {
			t.Fatalf("round %d: exposure %d over the limit but not restricted", i, v.Holds+v.Receivable)
		}
		if v.Holds+v.Receivable > 2000 {
			t.Fatalf("round %d: purchase approved beyond what the return explains: %d", i, v.Holds+v.Receivable)
		}
	}
}

func TestReevaluateLiftsAfterHoldRelease(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-ret6", 1000)
	f.spendCaptured(t, acc.Id, "ret6-a", 500)
	pay := f.settledPayment(t, acc.Id, 500, "bank-ret6")
	hold := f.authorize(t, acc.Id, "ret6-hold", 1000).Authorization
	_, _ = f.returnPayment(pay, "ret-ret6") // exposure 1000 hold + 500 debt = 1500 → restricted
	if f.restricted(t, acc.Id) == "" {
		t.Fatal("precondition: restricted")
	}
	if _, err := f.reverse(hold.Id, "rev-ret6"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReevaluateRestrictions(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if f.restricted(t, acc.Id) != "" {
		t.Fatal("exposure 500 ≤ 1000 after the reversal: sweep must lift")
	}
}

func TestReturningPaymentResumedByReconciler(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-ret7", 1000)
	f.spendCaptured(t, acc.Id, "ret7-a", 400)
	pay := f.settledPayment(t, acc.Id, 400, "bank-ret7")
	fl.failPaymentReturn.Store(true)
	if _, err := f.returnPayment(pay, "ret-ret7"); err == nil {
		t.Fatal("expected lost response")
	}
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	_ = f.pool.QueryRow(ctx, `SELECT status FROM payments WHERE id=$1`, pay.Id).Scan(&st)
	if st != "returned" {
		t.Fatalf("status=%s", st)
	}
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE entry_type='payment_returned' AND account_id=$1`, acc.Id); n != 1 {
		t.Fatalf("return journals=%d", n)
	}
}

func (l *flakyLedger) GetBalances(ctx context.Context, r *ledgerv1.GetBalancesRequest, o ...grpc.CallOption) (*ledgerv1.GetBalancesResponse, error) {
	if l.failBalances.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return l.LedgerServiceClient.GetBalances(ctx, r, o...)
}

// Review I1: the restriction must not depend on a ledger read after the return committed.
func TestReturnRestrictsEvenIfLedgerReadFailsAfterCommit(t *testing.T) {
	f, _, fl := flakyFixture(t)
	acc := f.activeAccount(t, "cust-ret8", 1000)
	f.spendCaptured(t, acc.Id, "ret8-a", 1000)
	pay := f.settledPayment(t, acc.Id, 1000, "bank-ret8")
	f.spendCaptured(t, acc.Id, "ret8-b", 1000)
	fl.failBalances.Store(true) // the first ledger read after the return's finalize fails
	_, _ = f.returnPayment(pay, "ret-ret8")
	var reason *string
	_ = f.pool.QueryRow(context.Background(), `SELECT restricted_reason FROM accounts WHERE id=$1`, acc.Id).Scan(&reason)
	if reason == nil || *reason != "payment_returned" {
		t.Fatal("over-limit return must leave the card restricted even when the post-commit ledger read fails")
	}
}

// Review M1: a lift decided on an exposure reading taken before a later return must not apply.
func TestStaleLiftDoesNotClearNewerRestriction(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-ret9", 1000)
	f.spendCaptured(t, acc.Id, "ret9-a", 1000)
	pay := f.settledPayment(t, acc.Id, 1000, "bank-ret9")
	f.spendCaptured(t, acc.Id, "ret9-b", 1000)
	_, _ = f.returnPayment(pay, "ret-ret9")
	var seen int64
	_ = f.pool.QueryRow(ctx, `SELECT exposure_epoch FROM accounts WHERE id=$1`, acc.Id).Scan(&seen)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET exposure_epoch = exposure_epoch + 1 WHERE id=$1`, acc.Id); err != nil { // a newer return
		t.Fatal(err)
	}
	if err := f.svc.clearRestriction(ctx, acc.Id, seen, nil); err != nil {
		t.Fatal(err)
	}
	if f.restricted(t, acc.Id) != "payment_returned" {
		t.Fatal("a lift based on a stale exposure reading must not clear the restriction")
	}
}
