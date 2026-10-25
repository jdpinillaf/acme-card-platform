//go:build integration

package accounts

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

const day = 24 * time.Hour

func (f *fixture) advance(t *testing.T, acc string, days int32) *accountsv1.Account {
	t.Helper()
	r, err := f.svc.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc, Days: days})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func (f *fixture) entryEffectiveAt(t *testing.T, ref string) time.Time {
	t.Helper()
	var at time.Time
	if err := f.ledger.QueryRow(context.Background(), `SELECT effective_at FROM journal_entries WHERE reference=$1`, ref).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

func TestAdvanceClockMovesAccountTimeOnly(t *testing.T) {
	f, _ := txFixture(t)
	a := f.activeAccount(t, "cust-clk1", 1000)
	b := f.activeAccount(t, "cust-clk2", 1000)
	got := f.advance(t, a.Id, 30)
	if !got.AccountTime.AsTime().Equal(t0.Add(30 * day)) {
		t.Fatalf("account_time=%v want %v", got.AccountTime.AsTime(), t0.Add(30*day))
	}
	other, err := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: b.Id})
	if err != nil || !other.Account.AccountTime.AsTime().Equal(t0) {
		t.Fatalf("other account moved: %v %v", err, other)
	}
	for _, d := range []int32{0, 401, -1} {
		if _, err := f.svc.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: a.Id, Days: d}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("days=%d: %v", d, err)
		}
	}
	if f.events(t, "account.clock_advanced") != 1 {
		t.Fatal("clock_advanced event")
	}
}

func TestPostingsCarryAccountTime(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-clk3", 1000, 600)
	f.advance(t, acc.Id, 10)
	p, err := f.createPayment(acc.Id, 600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Payment.ReceivedAt.AsTime().Equal(t0.Add(10 * day)) {
		t.Fatalf("received_at=%v", p.Payment.ReceivedAt.AsTime())
	}
	f.advance(t, acc.Id, 2)
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-clk3", paySettled)); err != nil {
		t.Fatal(err)
	}
	if at := f.entryEffectiveAt(t, "payment:"+p.Payment.Id); !at.Equal(t0.Add(12 * day)) {
		t.Fatalf("payment effective_at=%v want %v", at, t0.Add(12*day))
	}
	// The purchase captured before any advance carries account time 0 offset.
	var capAt time.Time
	if err := f.ledger.QueryRow(ctx, `SELECT effective_at FROM journal_entries WHERE account_id=$1 AND entry_type='purchase_captured'`, acc.Id).Scan(&capAt); err != nil {
		t.Fatal(err)
	}
	if !capAt.UTC().Equal(t0) {
		t.Fatalf("capture effective_at=%v want %v", capAt.UTC(), t0)
	}
}

func TestReconcilerReusesPersistedEffectiveAt(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-clk4", 1000, 600)
	p, err := f.createPayment(acc.Id, 600, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.advance(t, acc.Id, 3)
	reserveAt := c.Now().Add(3 * day)
	fl.failPayment.Store(true)
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-clk4", paySettled)); err == nil {
		t.Fatal("expected lost response")
	}
	// The ledger committed with the reserve-time business date; the advance below must not move it.
	f.advance(t, acc.Id, 5)
	c.Advance(PendingTimeout + time.Minute)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	if at := f.entryEffectiveAt(t, "payment:"+p.Payment.Id); !at.Equal(reserveAt) {
		t.Fatalf("effective_at=%v want reserve-time %v", at, reserveAt)
	}
	var settleAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT settle_effective_at FROM payments WHERE id=$1`, p.Payment.Id).Scan(&settleAt); err != nil || !settleAt.UTC().Equal(reserveAt) {
		t.Fatalf("persisted settle_effective_at=%v err=%v", settleAt, err)
	}
}

func TestReconcilerReusesEffectiveAtWhenLedgerNeverAnswered(t *testing.T) {
	f, c, _ := flakyFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-clk5", 1000, 600)
	p, err := f.createPayment(acc.Id, 600, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.advance(t, acc.Id, 3)
	reserveAt := c.Now().Add(3 * day)
	// Reserve only: simulate a crash between the reserve and the ledger call.
	if _, err := f.pool.Exec(ctx, `UPDATE payments SET status='settling', external_ref='bank-clk5', settling_at=$2,
		settle_effective_at=$3 WHERE id=$1`, p.Payment.Id, c.Now(), reserveAt); err != nil {
		t.Fatal(err)
	}
	f.advance(t, acc.Id, 5)
	c.Advance(PendingTimeout + time.Minute)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	if at := f.entryEffectiveAt(t, "payment:"+p.Payment.Id); !at.Equal(reserveAt) {
		t.Fatalf("effective_at=%v want %v", at, reserveAt)
	}
}

func TestActivatedAtSetOnActivation(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.open(t, "cust-clk6")
	if acc.ActivatedAt != nil {
		t.Fatalf("pending account has activated_at %v", acc.ActivatedAt.AsTime())
	}
	f.advance(t, acc.Id, 4)
	dep := f.deposit(t, acc.Id, 1000)
	r, err := f.svc.SettleDeposit(context.Background(), settled(dep, "bank-clk6"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Account.ActivatedAt == nil || !r.Account.ActivatedAt.AsTime().Equal(t0.Add(4*day)) {
		t.Fatalf("activated_at=%v want %v", r.Account.ActivatedAt, t0.Add(4*day))
	}
	if r.Account.Delinquency != "none" {
		t.Fatalf("delinquency=%q", r.Account.Delinquency)
	}
	// A later top-up must not move the activation date.
	f.advance(t, acc.Id, 1)
	r2, err := f.svc.SettleDeposit(context.Background(), settled(f.deposit(t, acc.Id, 500), "bank-clk6b"))
	if err != nil || !r2.Account.ActivatedAt.AsTime().Equal(t0.Add(4*day)) {
		t.Fatalf("activated_at moved: %v %v", err, r2.Account.ActivatedAt)
	}
}

// Review I2: the gateway releases an idempotency key on a 5xx (e.g. the catch-up after the advance
// timed out), so a retry with the same key reaches accounts again and must not advance twice.
func TestAdvanceClockIdempotentOnKeyHash(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-clk7", 1000)
	key := strings.Repeat("d", 64)
	req := func(days int32) (*accountsv1.GetAccountResponse, error) {
		return f.svc.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc.Id, Days: days,
			Meta: &commonv1.RequestMeta{IdempotencyKeyHash: key}})
	}
	if _, err := req(30); err != nil {
		t.Fatal(err)
	}
	r, err := req(30)
	if err != nil || !r.Account.AccountTime.AsTime().Equal(t0.Add(30*day)) {
		t.Fatalf("retry advanced again: %v %v", err, r)
	}
	if _, err := req(31); grpcx.Reason(err) != "idempotency_key_reuse" {
		t.Fatalf("same key, different days: %v", err)
	}
	if f.events(t, "account.clock_advanced") != 1 {
		t.Fatal("one advance event")
	}
}
