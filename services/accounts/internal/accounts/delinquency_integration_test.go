//go:build integration

package accounts

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func (f *fixture) chargeOff(t *testing.T, acc string) *accountsv1.Account {
	t.Helper()
	r, err := f.svc.ChargeOff(context.Background(), &accountsv1.ChargeOffRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func (f *fixture) authStatus(t *testing.T, id string) string {
	var st string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM authorizations WHERE id=$1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestChargeOffAppliesCollateralAndBlocks(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-co1", 100000, 30000)
	open := f.authorize(t, acc.Id, "net-co1-open", 5000).Authorization
	got := f.chargeOff(t, acc.Id)
	if got.Delinquency != DelinquencyChargedOff || got.CollateralMinor != 70000 || got.CreditLimitMinor != 70000 || got.OutstandingMinor != 0 {
		t.Fatalf("after charge-off: %+v", got)
	}
	if st := f.authStatus(t, open.Id); st != "expired" {
		t.Fatalf("open authorization status=%s want expired", st)
	}
	if st := f.holdStatus(t, "net-co1-open"); st != "expired" {
		t.Fatalf("hold status=%s want expired", st)
	}
	if r := f.authorize(t, acc.Id, "net-co1-after", 100).Authorization; r.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_CHARGED_OFF {
		t.Fatalf("authorization after charge-off: %+v", r)
	}
	if _, err := f.createPayment(acc.Id, 100, nil); grpcx.Reason(err) != "nothing_to_pay" {
		t.Fatalf("payment after charge-off: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.ChargeOff(ctx, &accountsv1.ChargeOffRequest{AccountId: acc.Id}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='collateral_applied'`, acc.Id); n != 1 {
		t.Fatalf("collateral_applied entries=%d", n)
	}
	if f.events(t, "account.charged_off") != 1 {
		t.Fatal("charged_off event")
	}
}

func TestChargeOffDebtAboveCollateral(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-co2", 50000, 50000)
	if _, err := f.ledgerClient.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: acc.Id, Reference: "late_fee:co2:1",
		Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 10000}); err != nil {
		t.Fatal(err)
	}
	got := f.chargeOff(t, acc.Id)
	if got.CollateralMinor != 0 || got.CreditLimitMinor != 0 || got.OutstandingMinor != 10000 {
		t.Fatalf("after charge-off: %+v", got)
	}
	// Remaining debt can still be paid.
	if _, err := f.createPayment(acc.Id, 10000, nil); err != nil {
		t.Fatalf("payment of remaining debt: %v", err)
	}
}

func TestChargeOffRacesCapture(t *testing.T) {
	for i := 0; i < 5; i++ {
		f, _ := txFixture(t)
		ctx := context.Background()
		acc := f.withDebt(t, "cust-co3", 100000, 30000)
		a := f.authorize(t, acc.Id, "net-co3", 5000).Authorization
		var wg sync.WaitGroup
		wg.Add(2)
		var capErr, coErr error
		go func() { defer wg.Done(); _, capErr = f.capture(a.Id, "cap-co3", 5000) }()
		go func() {
			defer wg.Done()
			for {
				_, coErr = f.svc.ChargeOff(ctx, &accountsv1.ChargeOffRequest{AccountId: acc.Id})
				if status.Code(coErr) != codes.Unavailable { // an op in flight: retry like the billing catch-up does
					return
				}
			}
		}()
		wg.Wait()
		if coErr != nil {
			t.Fatalf("charge-off: %v", coErr)
		}
		got, err := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
		if err != nil {
			t.Fatal(err)
		}
		st := f.authStatus(t, a.Id)
		switch {
		case capErr == nil && st == "captured":
			if got.Account.CreditLimitMinor != 65000 || got.Account.OutstandingMinor != 0 {
				t.Fatalf("capture first: %+v", got.Account)
			}
		case capErr != nil && st == "expired":
			if got.Account.CreditLimitMinor != 70000 || got.Account.OutstandingMinor != 0 {
				t.Fatalf("charge-off first: %+v", got.Account)
			}
		default:
			t.Fatalf("inconsistent: capErr=%v status=%s account=%+v", capErr, st, got.Account)
		}
		if h := f.ledgerHolds(t, acc.Id); h != 0 {
			t.Fatalf("active holds after charge-off: %d", h)
		}
	}
}

func TestCheckExposureRestrictsOverLimit(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-ol1", 1000, 1000)
	if _, err := f.ledgerClient.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: acc.Id, Reference: "interest:ol1:1",
		Kind: ledgerv1.ChargeKind_CHARGE_KIND_INTEREST, AmountMinor: 50}); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.CheckExposure(ctx, &accountsv1.CheckExposureRequest{AccountId: acc.Id})
	if err != nil || r.Account.RestrictedReason != RestrictionOverLimit {
		t.Fatalf("check exposure: %v %+v", err, r)
	}
	if d := f.authorize(t, acc.Id, "net-ol1", 1).Authorization; d.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_OVER_LIMIT {
		t.Fatalf("decline=%s", d.DeclineReason)
	}
	p, err := f.createPayment(acc.Id, 1050, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-ol1", paySettled))
	if err != nil || s.Account.RestrictedReason != "" {
		t.Fatalf("settlement must lift over_limit: %v %+v", err, s)
	}
	// Within the limit: no restriction.
	r, err = f.svc.CheckExposure(ctx, &accountsv1.CheckExposureRequest{AccountId: acc.Id})
	if err != nil || r.Account.RestrictedReason != "" {
		t.Fatalf("within limit: %v %+v", err, r)
	}
}

func TestSetDelinquency(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-dq1", 1000, 500)
	set := func(stage string) (*accountsv1.GetAccountResponse, error) {
		return f.svc.SetDelinquency(ctx, &accountsv1.SetDelinquencyRequest{AccountId: acc.Id, Stage: stage})
	}
	if r, err := set(DelinquencyPastDue); err != nil || r.Account.Delinquency != DelinquencyPastDue {
		t.Fatalf("past_due: %v %+v", err, r)
	}
	if d := f.authorize(t, acc.Id, "net-dq1", 1).Authorization; d.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_PAST_DUE {
		t.Fatalf("decline=%s", d.DeclineReason)
	}
	set(DelinquencyPastDue) // no-op, no second event
	if f.events(t, "account.delinquency_changed") != 1 {
		t.Fatal("delinquency_changed must be emitted only on change")
	}
	if _, err := set("charged_off"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("charged_off via SetDelinquency: %v", err)
	}
	if r, err := set(DelinquencyNone); err != nil || r.Account.Delinquency != DelinquencyNone {
		t.Fatalf("none: %v %+v", err, r)
	}
	f.chargeOff(t, acc.Id)
	if _, err := set(DelinquencyNone); grpcx.Reason(err) != "charged_off" {
		t.Fatalf("leaving charged_off: %v", err)
	}
}

func TestListAccountIDsPagesActive(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	a := f.activeAccount(t, "cust-ls1", 100)
	b := f.activeAccount(t, "cust-ls2", 100)
	f.open(t, "cust-ls3") // pending_collateral: not billable
	var got []string
	after := ""
	for {
		r, err := f.svc.ListAccountIDs(ctx, &accountsv1.ListAccountIDsRequest{AfterId: after, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Ids) == 0 {
			break
		}
		got = append(got, r.Ids...)
		after = r.Ids[len(r.Ids)-1]
	}
	if len(got) != 2 || !(got[0] == a.Id || got[0] == b.Id) || got[0] == got[1] {
		t.Fatalf("ids=%v", got)
	}
}

func TestChargeOffHonoursRequestedBusinessDate(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.withDebt(t, "cust-co4", 1000, 400)
	f.advance(t, acc.Id, 90)
	at := t0.Add(60 * day)
	if _, err := f.svc.ChargeOff(context.Background(), &accountsv1.ChargeOffRequest{AccountId: acc.Id,
		Meta: &commonv1.RequestMeta{EffectiveAt: timestamppb.New(at)}}); err != nil {
		t.Fatal(err)
	}
	if got := f.entryEffectiveAt(t, "chargeoff:"+acc.Id); !got.Equal(at) {
		t.Fatalf("collateral_applied effective_at=%v want %v", got, at)
	}
}
