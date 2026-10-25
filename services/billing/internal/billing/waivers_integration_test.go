//go:build integration

package billing

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

// withLateFee returns an account owing a 2500 late fee for cycle 1, at DPD 1.
func (f *fixture) withLateFee(t *testing.T) (string, *billingv1.Charge) {
	t.Helper()
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	f.advanceTo(t, acc.Id, engine.OnTimeCutoff(engine.DueDate(f.closeN(t, acc, 1))))
	fees := f.charges(t, acc.Id, "fee")
	if len(fees) != 1 || fees[0].AmountMinor != 2500 {
		t.Fatalf("fixture fee: %+v", fees)
	}
	return acc.Id, fees[0]
}

func (f *fixture) waive(acc, charge string, amt int64, key string) (*billingv1.WaiveChargeResponse, error) {
	meta := &commonv1.RequestMeta{Actor: "agent-7"}
	if key != "" {
		meta.IdempotencyKeyHash = key
	}
	return f.svc.WaiveCharge(context.Background(), &billingv1.WaiveChargeRequest{AccountId: acc, ChargeId: charge, AmountMinor: amt, Reason: "goodwill", Meta: meta})
}

func (f *fixture) ledgerWaived(t *testing.T, acc string) int64 {
	t.Helper()
	var s int64
	if err := f.ledgerDB.QueryRow(context.Background(), `SELECT coalesce(sum(p.amount_minor),0) FROM journal_entries j JOIN postings p ON p.journal_entry_id=j.id
		AND p.sub_account='receivable' AND p.side='credit' WHERE j.account_id=$1 AND j.entry_type IN ('fee_waived','interest_waived')`, acc).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWaiverPartialThenFull(t *testing.T) {
	f := setup(t)
	acc, fee := f.withLateFee(t)
	if r, err := f.waive(acc, fee.Id, 1000, ""); err != nil || r.Charge.WaivedMinor != 1000 || r.Waiver.AmountMinor != 1000 {
		t.Fatalf("first waiver: %v %+v", err, r)
	}
	if r, err := f.waive(acc, fee.Id, 1500, ""); err != nil || r.Charge.WaivedMinor != 2500 {
		t.Fatalf("second waiver: %v %+v", err, r)
	}
	if _, err := f.waive(acc, fee.Id, 1, ""); grpcx.Reason(err) != "waiver_exceeds_charge" {
		t.Fatalf("over-waiver: %v", err)
	}
	if got := f.ledgerWaived(t, acc); got != 2500 {
		t.Fatalf("ledger waived=%d", got)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'='charge.waived'`); n != 2 {
		t.Fatalf("charge.waived events=%d", n)
	}
}

func TestWaiverValidation(t *testing.T) {
	f := setup(t)
	acc, fee := f.withLateFee(t)
	for _, c := range []struct {
		name   string
		req    *billingv1.WaiveChargeRequest
		reason string
	}{
		{"zero amount", &billingv1.WaiveChargeRequest{AccountId: acc, ChargeId: fee.Id, AmountMinor: 0, Reason: "x"}, "invalid_amount"},
		{"no reason", &billingv1.WaiveChargeRequest{AccountId: acc, ChargeId: fee.Id, AmountMinor: 1}, "invalid_reason"},
		{"long reason", &billingv1.WaiveChargeRequest{AccountId: acc, ChargeId: fee.Id, AmountMinor: 1, Reason: strings.Repeat("x", 201)}, "invalid_reason"},
		{"unknown charge", &billingv1.WaiveChargeRequest{AccountId: acc, ChargeId: "0190f6c4-0000-7000-8000-000000000000", AmountMinor: 1, Reason: "x"}, "charge_not_found"},
		{"other account", &billingv1.WaiveChargeRequest{AccountId: "0190f6c4-0000-7000-8000-000000000001", ChargeId: fee.Id, AmountMinor: 1, Reason: "x"}, "account_not_found"},
	} {
		if _, err := f.svc.WaiveCharge(context.Background(), c.req); grpcx.Reason(err) != c.reason {
			t.Errorf("%s: %v want %s", c.name, err, c.reason)
		}
	}
}

func TestWaiversConcurrentNeverExceed(t *testing.T) {
	f := setup(t)
	acc, fee := f.withLateFee(t)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.waive(acc, fee.Id, 200, ""); err == nil {
				ok.Add(1)
			} else if grpcx.Reason(err) != "waiver_exceeds_charge" {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 12 {
		t.Fatalf("successful waivers=%d want 12", ok.Load())
	}
	if c := f.charges(t, acc, "fee")[0]; c.WaivedMinor != 2400 {
		t.Fatalf("waived_minor=%d", c.WaivedMinor)
	}
	if got := f.ledgerWaived(t, acc); got != 2400 {
		t.Fatalf("ledger waived=%d", got)
	}
}

func TestWaiverCuresPastDue(t *testing.T) {
	f := setup(t)
	acc, fee := f.withLateFee(t)
	f.advanceTo(t, acc, engine.OnTimeCutoff(fee.EffectiveAt.AsTime()).Add(13*dayDur)) // DPD ≥ 15
	if a := f.account(t, acc); a.Delinquency != "past_due" {
		t.Fatalf("stage=%s", a.Delinquency)
	}
	if _, err := f.waive(acc, fee.Id, 2500, ""); err != nil {
		t.Fatal(err)
	}
	if a := f.account(t, acc); a.Delinquency != "none" {
		t.Fatalf("stage after waiver=%s", a.Delinquency)
	}
}

func TestWaiverIdempotentKey(t *testing.T) {
	f := setup(t)
	acc, fee := f.withLateFee(t)
	key := strings.Repeat("c", 64)
	var wg sync.WaitGroup
	var replayed atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.waive(acc, fee.Id, 500, key)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Replayed {
				replayed.Add(1)
			}
		}()
	}
	wg.Wait()
	if replayed.Load() != 49 {
		t.Fatalf("replayed=%d want 49", replayed.Load())
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM waivers WHERE charge_id=$1`, fee.Id); n != 1 {
		t.Fatalf("waivers=%d", n)
	}
	if _, err := f.waive(acc, fee.Id, 600, key); grpcx.Reason(err) != "idempotency_key_reuse" {
		t.Fatalf("same key, different amount: %v", err)
	}
}
