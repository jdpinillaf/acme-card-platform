//go:build integration

package accounts

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func (l *flakyLedger) PostPayment(ctx context.Context, r *ledgerv1.PostPaymentRequest, o ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	res, err := l.LedgerServiceClient.PostPayment(ctx, r, o...)
	if err == nil && l.failPayment.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

func (l *flakyLedger) PostPaymentReturn(ctx context.Context, r *ledgerv1.PostPaymentReturnRequest, o ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	res, err := l.LedgerServiceClient.PostPaymentReturn(ctx, r, o...)
	if err == nil && l.failPaymentReturn.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

func settlePay(p *accountsv1.Payment, ref string, st accountsv1.PaymentStatus) *accountsv1.SettlePaymentRequest {
	return &accountsv1.SettlePaymentRequest{PaymentId: p.Id, ExternalRef: ref, AmountMinor: p.Amount.AmountMinor, Currency: p.Amount.Currency, Status: st}
}

const (
	paySettled = accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED
	payFailed  = accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED
)

func (f *fixture) paymentJournals(t *testing.T, acc string) int {
	return f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='payment_settled'`, acc)
}

func TestSettlePaymentRestoresCreditOnlyOnConfirmation(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-s1", 1000, 600)
	p, _ := f.createPayment(acc.Id, 600, nil)
	if p.Account.AvailableCreditMinor != 400 {
		t.Fatalf("creating a payment must not restore credit: %d", p.Account.AvailableCreditMinor)
	}
	r, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s1", paySettled))
	if err != nil || r.Payment.Status != paySettled || r.Account.AvailableCreditMinor != 1000 || r.Account.OutstandingMinor != 0 {
		t.Fatalf("%v %+v", err, r)
	}
	if f.events(t, "payment.settled") != 1 || f.paymentJournals(t, acc.Id) != 1 {
		t.Fatal("settled event / journal")
	}
}

func TestFailedPaymentHasNoEffect(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-s2", 1000, 600)
	p, _ := f.createPayment(acc.Id, 600, nil)
	r, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s2", payFailed))
	if err != nil || r.Payment.Status != payFailed || r.Account.AvailableCreditMinor != 400 {
		t.Fatalf("%v %+v", err, r)
	}
	if f.paymentJournals(t, acc.Id) != 0 || f.events(t, "payment.failed") != 1 {
		t.Fatal("failed payment must post nothing and be evented")
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s2", paySettled)); grpcx.Reason(err) != "payment_failed" {
		t.Fatalf("settling a failed payment: %v", err)
	}
	if _, err := f.createPayment(acc.Id, 600, nil); err != nil {
		t.Fatalf("failed payment must free the cap: %v", err)
	}
}

func TestSettlePaymentSameWebhookTwiceOnce(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-s3", 1000, 500)
	p, _ := f.createPayment(acc.Id, 500, nil)
	var fresh atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s3", paySettled))
			if err != nil {
				t.Error(err)
				return
			}
			if !r.Replayed {
				fresh.Add(1)
			}
		}()
	}
	wg.Wait()
	if fresh.Load() != 1 || f.paymentJournals(t, acc.Id) != 1 {
		t.Fatalf("fresh=%d journals=%d", fresh.Load(), f.paymentJournals(t, acc.Id))
	}
}

func TestSettlePaymentReusedExternalRefTouchesNothing(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-s4", 1000, 600)
	p1, _ := f.createPayment(acc.Id, 300, nil)
	p2, _ := f.createPayment(acc.Id, 300, nil)
	if _, err := f.svc.SettlePayment(ctx, settlePay(p1.Payment, "bank-dup", paySettled)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p2.Payment, "bank-dup", paySettled)); grpcx.Reason(err) != "external_ref_reused" {
		t.Fatalf("reused ref: %v", err)
	}
	if f.paymentJournals(t, acc.Id) != 1 {
		t.Fatal("reused ref must not post")
	}
	bad := settlePay(p2.Payment, "bank-ok", paySettled)
	bad.AmountMinor = 299
	if _, err := f.svc.SettlePayment(ctx, bad); grpcx.Reason(err) != "amount_mismatch" {
		t.Fatalf("amount mismatch: %v", err)
	}
}

func TestSettleWhileRefundLands(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-s5", 1000)
	a := f.authorize(t, acc.Id, "net-s5", 500).Authorization
	_, _ = f.capture(a.Id, "cap-s5", 500)
	p, _ := f.createPayment(acc.Id, 500, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s5", paySettled)) }()
	go func() { defer wg.Done(); _, _ = f.refund(a.Id, "rf-s5", 200) }()
	wg.Wait()
	g, _ := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if g.Account.CreditBalanceMinor != 200 || g.Account.OutstandingMinor != 0 || g.Account.AvailableCreditMinor != 1200 {
		t.Fatalf("%+v", g.Account)
	}
}

func TestSettlingPaymentResumedByReconciler(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-s6", 1000, 400)
	p, _ := f.createPayment(acc.Id, 400, nil)
	fl.failPayment.Store(true)
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-s6", paySettled)); err == nil {
		t.Fatal("expected lost response")
	}
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	_ = f.pool.QueryRow(ctx, `SELECT status FROM payments WHERE id=$1`, p.Payment.Id).Scan(&st)
	if st != "settled" || f.paymentJournals(t, acc.Id) != 1 {
		t.Fatalf("status=%s journals=%d", st, f.paymentJournals(t, acc.Id))
	}
}

func TestLateSettlementAfterExpiryIsApplied(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-late", 1000, 400)
	p, _ := f.createPayment(acc.Id, 400, nil)
	c.Advance(PaymentTTL + time.Second)
	if n, _ := f.svc.ExpirePayments(ctx, 100); n != 1 {
		t.Fatal("precondition: intent expired")
	}
	r, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-late", paySettled))
	if err != nil || r.Payment.Status != paySettled || r.Account.OutstandingMinor != 0 {
		t.Fatalf("money moved after expiry must still be applied: %v %+v", err, r)
	}
	fail, _ := f.createPayment(f.withDebt(t, "cust-late2", 1000, 300).Id, 300, nil)
	c.Advance(PaymentTTL + time.Second)
	_, _ = f.svc.ExpirePayments(ctx, 100)
	if _, err := f.svc.SettlePayment(ctx, settlePay(fail.Payment, "bank-late2", payFailed)); grpcx.Reason(err) != "payment_expired" {
		t.Fatalf("a failed notice for an expired intent stays refused: %v", err)
	}
}
