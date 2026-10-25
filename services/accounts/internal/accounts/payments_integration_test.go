//go:build integration

package accounts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

// withDebt returns an active account (limit `limit`) that owes `debt` from a captured purchase.
func (f *fixture) withDebt(t *testing.T, customer string, limit, debt int64) *accountsv1.Account {
	t.Helper()
	acc := f.activeAccount(t, customer, limit)
	if debt > 0 {
		a := f.authorize(t, acc.Id, "net-debt-"+customer, debt).Authorization
		if _, err := f.capture(a.Id, "cap-debt-"+customer, debt); err != nil {
			t.Fatal(err)
		}
	}
	return acc
}

func (f *fixture) createPayment(acc string, amt int64, meta *commonv1.RequestMeta) (*accountsv1.PaymentResponse, error) {
	return f.svc.CreatePayment(context.Background(), &accountsv1.CreatePaymentRequest{AccountId: acc, AmountMinor: amt, Meta: meta})
}

func TestCreatePaymentMinimumAndCap(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.withDebt(t, "cust-pay1", 1000, 500)
	if _, err := f.createPayment(acc.Id, 99, nil); status.Code(err) != codes.InvalidArgument || grpcx.Reason(err) != "invalid_amount" {
		t.Fatalf("below minimum: %v", err)
	}
	if _, err := f.createPayment(acc.Id, 600, nil); grpcx.Reason(err) != "payment_exceeds_balance" {
		t.Fatalf("above debt: %v", err)
	}
	r, err := f.createPayment(acc.Id, 300, nil)
	if err != nil || r.Payment.Status != accountsv1.PaymentStatus_PAYMENT_STATUS_PENDING || r.Payment.Amount.Currency != "USD" {
		t.Fatalf("300: %v %+v", err, r)
	}
	if _, err := f.createPayment(acc.Id, 201, nil); grpcx.Reason(err) != "payment_exceeds_balance" {
		t.Fatalf("pending payments count toward the cap: %v", err)
	}
	if _, err := f.createPayment(acc.Id, 200, nil); err != nil {
		t.Fatalf("exactly the remaining debt: %v", err)
	}
	if f.events(t, "payment.created") != 2 {
		t.Fatal("payment.created events")
	}
}

func TestCreatePaymentNothingToPayAndState(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.withDebt(t, "cust-pay2", 1000, 0)
	if _, err := f.createPayment(acc.Id, 100, nil); grpcx.Reason(err) != "nothing_to_pay" {
		t.Fatalf("no debt: %v", err)
	}
	pend := f.open(t, "cust-pay2b")
	if _, err := f.createPayment(pend.Id, 100, nil); grpcx.Reason(err) != "collateral_not_confirmed" {
		t.Fatalf("pending account: %v", err)
	}
}

func TestConcurrentPaymentsNeverExceedDebt(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.withDebt(t, "cust-pay3", 1000, 1000)
	var ok atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.createPayment(acc.Id, 300, nil); err == nil {
				ok.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 3 {
		t.Fatalf("payments created=%d want 3", ok.Load())
	}
}

func TestCreatePaymentDedupesOnKeyHash(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.withDebt(t, "cust-pay4", 1000, 500)
	meta := &commonv1.RequestMeta{IdempotencyKeyHash: strings.Repeat("b", 64)}
	r1, err := f.createPayment(acc.Id, 200, meta)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.createPayment(acc.Id, 200, meta)
	if err != nil || r2.Payment.Id != r1.Payment.Id || !r2.Replayed {
		t.Fatalf("dedupe: %v %+v", err, r2)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM payments WHERE account_id=$1`, acc.Id); n != 1 {
		t.Fatalf("payments=%d", n)
	}
}

func TestExpirePaymentsAfter7Days(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-pay5", 1000, 500)
	if _, err := f.createPayment(acc.Id, 500, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.createPayment(acc.Id, 100, nil); grpcx.Reason(err) != "nothing_to_pay" {
		t.Fatalf("open payments already cover the debt: %v", err)
	}
	c.Advance(PaymentTTL + time.Second)
	n, err := f.svc.ExpirePayments(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("expired=%d err=%v", n, err)
	}
	if _, err := f.createPayment(acc.Id, 500, nil); err != nil {
		t.Fatalf("cap must be freed after expiry: %v", err)
	}
	if f.events(t, "payment.expired") != 1 {
		t.Fatal("payment.expired event")
	}
}

func TestListPaymentsNewestFirst(t *testing.T) {
	f, c := txFixture(t)
	acc := f.withDebt(t, "cust-pay6", 10_000, 5_000)
	for i := 0; i < 3; i++ {
		if _, err := f.createPayment(acc.Id, int64(100*(i+1)), nil); err != nil {
			t.Fatal(err)
		}
		c.Advance(time.Second)
	}
	r, err := f.svc.ListPayments(context.Background(), &accountsv1.ListPaymentsRequest{AccountId: acc.Id, PageSize: 2})
	if err != nil || len(r.Payments) != 2 || r.Payments[0].Amount.AmountMinor != 300 || r.NextPageToken == "" {
		t.Fatalf("%v %+v", err, r)
	}
	r2, _ := f.svc.ListPayments(context.Background(), &accountsv1.ListPaymentsRequest{AccountId: acc.Id, PageToken: r.NextPageToken})
	if len(r2.Payments) != 1 || r2.Payments[0].Amount.AmountMinor != 100 {
		t.Fatalf("page 2: %+v", r2)
	}
}

var _ = fmt.Sprint

func TestDebtBelowMinimumCanBePaidOff(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-small", 1000)
	a := f.authorize(t, acc.Id, "net-small", 300).Authorization
	_, _ = f.capture(a.Id, "cap-small", 300)
	if _, err := f.refund(a.Id, "rf-small", 250); err != nil { // leaves 0.50 owed
		t.Fatal(err)
	}
	if _, err := f.createPayment(acc.Id, 49, nil); grpcx.Reason(err) != "invalid_amount" {
		t.Fatalf("below the remaining debt and below 1.00: %v", err)
	}
	r, err := f.createPayment(acc.Id, 50, nil)
	if err != nil {
		t.Fatalf("paying off a debt under 1.00 must be possible: %v", err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(r.Payment, "bank-small", paySettled)); err != nil {
		t.Fatal(err)
	}
	g, _ := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if g.Account.OutstandingMinor != 0 {
		t.Fatalf("outstanding=%d", g.Account.OutstandingMinor)
	}
}
