//go:build integration

package accounts

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/clock"
	"github.com/acme/acme-card/pkg/grpcx"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// txFixture is a fixture whose service uses a fake clock.
func txFixture(t *testing.T) (*fixture, *clock.Fake) {
	f := setup(t)
	c := clock.NewFake(t0)
	f.svc = NewService(f.pool, f.ledgerClient, WithClock(c))
	return f, c
}

func authReq(card, ref string, amt int64) *accountsv1.AuthorizeRequest {
	return &accountsv1.AuthorizeRequest{NetworkRef: ref, CardId: card, AmountMinor: amt, Currency: "USD",
		Merchant: &accountsv1.Merchant{Id: "m-1", Name: "Coffee Shop", Mcc: "5814"}}
}

func (f *fixture) authorize(t *testing.T, card, ref string, amt int64) *accountsv1.AuthorizationResponse {
	t.Helper()
	r, err := f.svc.Authorize(context.Background(), authReq(card, ref, amt))
	if err != nil {
		t.Fatalf("authorize %s: %v", ref, err)
	}
	return r
}

func (f *fixture) ledgerHolds(t *testing.T, acc string) int64 {
	b, err := f.ledgerClient.GetBalances(context.Background(), &ledgerv1.GetBalancesRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return b.HoldsMinor
}

const (
	approved = accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED
	declined = accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED
)

func TestAuthorizeApprovesAndHolds(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a1", 1000)
	r := f.authorize(t, acc.Id, "net-1", 300)
	a := r.Authorization
	if a.Status != approved || a.Merchant.Mcc != "5814" || a.Amount.AmountMinor != 300 {
		t.Fatalf("%+v", a)
	}
	if !a.ExpiresAt.AsTime().Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("expires_at=%v", a.ExpiresAt.AsTime())
	}
	g, _ := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if g.Account.AvailableCreditMinor != 700 {
		t.Fatalf("available=%d", g.Account.AvailableCreditMinor)
	}
	if f.events(t, "authorization.approved") != 1 {
		t.Fatal("missing authorization.approved event")
	}
}

func TestAuthorizeDeclinesAre201Records(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a2", 1000)
	if _, err := f.svc.SetFrozen(context.Background(), &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	a := f.authorize(t, acc.Id, "net-frozen", 100).Authorization
	if a.Status != declined || a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_CARD_FROZEN {
		t.Fatalf("%+v", a)
	}
	if c := f.count(t, f.pool, `SELECT count(*) FROM authorizations WHERE network_ref='net-frozen' AND status='declined' AND decline_reason='card_frozen'`); c != 1 {
		t.Fatalf("declined row not stored: %d", c)
	}
	if f.events(t, "authorization.declined") != 1 || f.ledgerHolds(t, acc.Id) != 0 {
		t.Fatal("decline must be evented and place no hold")
	}
}

func TestAuthorizeCurrencyNotSupported(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a3", 1000)
	req := authReq(acc.Id, "net-cop", 100)
	req.Currency = "COP"
	r, err := f.svc.Authorize(context.Background(), req)
	if err != nil || r.Authorization.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_CURRENCY_NOT_SUPPORTED {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestAuthorizePurchaseLimit(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a4", 1000)
	if _, err := f.svc.SetPurchaseLimit(context.Background(), &accountsv1.SetPurchaseLimitRequest{AccountId: acc.Id, PurchaseLimitMinor: 100}); err != nil {
		t.Fatal(err)
	}
	if a := f.authorize(t, acc.Id, "net-pl", 101).Authorization; a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_PURCHASE_LIMIT_EXCEEDED {
		t.Fatalf("%+v", a)
	}
	if a := f.authorize(t, acc.Id, "net-pl-ok", 100).Authorization; a.Status != approved {
		t.Fatalf("%+v", a)
	}
}

func TestAuthorizePendingCollateral(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.open(t, "cust-a5")
	if a := f.authorize(t, acc.Id, "net-pend", 1).Authorization; a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_COLLATERAL_NOT_CONFIRMED {
		t.Fatalf("%+v", a)
	}
}

func TestConcurrentAuthorizationsNeverExceedLimit(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a6", 1000)
	var ok, insufficient atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := f.svc.Authorize(context.Background(), authReq(acc.Id, fmt.Sprintf("net-c-%d", i), 250))
			if err != nil {
				t.Error(err)
				return
			}
			switch {
			case r.Authorization.Status == approved:
				ok.Add(1)
			case r.Authorization.DeclineReason == commonv1.DeclineReason_DECLINE_REASON_INSUFFICIENT_CREDIT:
				insufficient.Add(1)
			default:
				t.Errorf("unexpected %+v", r.Authorization)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if ok.Load() != 4 || insufficient.Load() != 6 {
		t.Fatalf("approved=%d insufficient=%d want 4/6", ok.Load(), insufficient.Load())
	}
	if h := f.ledgerHolds(t, acc.Id); h != 1000 {
		t.Fatalf("ledger holds=%d want 1000", h)
	}
}

func TestVelocityCountsDeclinedAttempts(t *testing.T) {
	f, c := txFixture(t)
	acc := f.activeAccount(t, "cust-a7", 1000)
	for i := 0; i < 10; i++ {
		if a := f.authorize(t, acc.Id, fmt.Sprintf("net-v-%d", i), 2000).Authorization; a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_INSUFFICIENT_CREDIT {
			t.Fatalf("attempt %d: %+v", i, a)
		}
	}
	if a := f.authorize(t, acc.Id, "net-v-11", 10).Authorization; a.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_VELOCITY_LIMIT_EXCEEDED {
		t.Fatalf("11th attempt: %+v", a)
	}
	c.Advance(2 * time.Minute)
	if a := f.authorize(t, acc.Id, "net-v-12", 10).Authorization; a.Status != approved {
		t.Fatalf("after window: %+v", a)
	}
}

func TestVelocityExactUnderConcurrency(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a8", 1000)
	var passed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := f.svc.Authorize(context.Background(), authReq(acc.Id, fmt.Sprintf("net-vc-%d", i), 1))
			if err != nil {
				t.Error(err)
				return
			}
			if r.Authorization.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_VELOCITY_LIMIT_EXCEEDED {
				passed.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if passed.Load() != 10 {
		t.Fatalf("passed velocity=%d want 10", passed.Load())
	}
}

func TestAuthorizeSameNetworkRefOnce(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-a9", 1000)
	var replayed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := f.svc.Authorize(context.Background(), authReq(acc.Id, "net-dup", 100))
			if err != nil {
				t.Error(err)
				return
			}
			if r.Replayed {
				replayed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if replayed.Load() != 49 || f.count(t, f.pool, `SELECT count(*) FROM authorizations WHERE network_ref='net-dup'`) != 1 || f.ledgerHolds(t, acc.Id) != 100 {
		t.Fatalf("replayed=%d", replayed.Load())
	}
	_, err := f.svc.Authorize(context.Background(), authReq(acc.Id, "net-dup", 101))
	if status.Code(err) != codes.AlreadyExists || grpcx.Reason(err) != "network_ref_reuse" {
		t.Fatalf("mismatch: %v", err)
	}
}

func TestAuthorizeUnknownCardAndValidation(t *testing.T) {
	f, _ := txFixture(t)
	unknown := "01a17300-0000-7000-8000-000000000000"
	_, err := f.svc.Authorize(context.Background(), authReq(unknown, "net-x", 1))
	if status.Code(err) != codes.NotFound || grpcx.Reason(err) != "card_not_found" {
		t.Fatalf("unknown card: %v", err)
	}
	if f.count(t, f.pool, `SELECT count(*) FROM authorizations`) != 0 || f.count(t, f.pool, `SELECT count(*) FROM rate_limit_windows`) != 0 {
		t.Fatal("unknown card must store nothing and consume no velocity")
	}
	acc := f.activeAccount(t, "cust-a10", 1000)
	for name, mutate := range map[string]func(*accountsv1.AuthorizeRequest){
		"zero amount":   func(r *accountsv1.AuthorizeRequest) { r.AmountMinor = 0 },
		"huge amount":   func(r *accountsv1.AuthorizeRequest) { r.AmountMinor = MaxDepositMinor + 1 },
		"no ref":        func(r *accountsv1.AuthorizeRequest) { r.NetworkRef = "" },
		"no merchant":   func(r *accountsv1.AuthorizeRequest) { r.Merchant = nil },
		"long mcc":      func(r *accountsv1.AuthorizeRequest) { r.Merchant.Mcc = "58140" },
		"bad currency":  func(r *accountsv1.AuthorizeRequest) { r.Currency = "usd" },
		"empty m. name": func(r *accountsv1.AuthorizeRequest) { r.Merchant.Name = "" },
	} {
		req := authReq(acc.Id, "net-val-"+name, 10)
		mutate(req)
		if _, err := f.svc.Authorize(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument got %v", name, err)
		}
	}
}
