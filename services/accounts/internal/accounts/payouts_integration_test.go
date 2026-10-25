//go:build integration

package accounts

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

const (
	payoutSettled = accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED
	payoutFailed  = accountsv1.PayoutStatus_PAYOUT_STATUS_FAILED
)

// closeToPayout closes acc and runs the saga until a payout attempt is waiting for the bank.
func (f *fixture) closeToPayout(t *testing.T, acc string) *accountsv1.Payout {
	t.Helper()
	f.close(t, acc)
	if got := f.step(t, acc); got.CloseStage != "payout" {
		t.Fatalf("stage=%s want payout", got.CloseStage)
	}
	return f.openPayout(t, acc)
}

func (f *fixture) openPayout(t *testing.T, acc string) *accountsv1.Payout {
	t.Helper()
	r, err := f.svc.ListPayouts(context.Background(), &accountsv1.ListPayoutsRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range r.Payouts {
		if p.Status == accountsv1.PayoutStatus_PAYOUT_STATUS_PENDING {
			return p
		}
	}
	t.Fatalf("no pending payout: %+v", r.Payouts)
	return nil
}

func (f *fixture) settlePayout(p *accountsv1.Payout, st accountsv1.PayoutStatus) (*accountsv1.PayoutResponse, error) {
	return f.svc.SettlePayout(context.Background(), &accountsv1.SettlePayoutRequest{AttemptId: p.AttemptId, AmountMinor: p.Amount.AmountMinor,
		Currency: p.Amount.Currency, DestinationLast4: "6789", Status: st})
}

func (f *fixture) payoutJournals(t *testing.T, acc string) int {
	return f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='payout_settled'`, acc)
}

func TestPayoutSettleReleasesOnce(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-po1", 100000)
	p := f.closeToPayout(t, acc.Id)
	if p.Amount.AmountMinor != 100000 || p.DestinationLast4 != "6789" || p.Attempt != 1 {
		t.Fatalf("payout: %+v", p)
	}
	ins, err := f.svc.GetPayoutInstruction(context.Background(), &accountsv1.GetPayoutInstructionRequest{AttemptId: p.AttemptId})
	if err != nil || ins.AccountNumber != dest.AccountNumber || ins.AmountMinor != 100000 || ins.HolderName != dest.HolderName {
		t.Fatalf("instruction: %v %+v", err, ins)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.settlePayout(p, payoutSettled); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.payoutJournals(t, acc.Id); n != 1 {
		t.Fatalf("payout journals=%d", n)
	}
	got, _ := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if got.Account.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSED || got.Account.CollateralMinor != 0 || got.Account.CloseStage != "closed" {
		t.Fatalf("after payout: %+v", got.Account)
	}
	if f.events(t, "account.closed") != 1 || f.events(t, "payout.settled") != 1 {
		t.Fatal("events")
	}
}

func TestPayoutFailRetrySettle(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-po2", 50000)
	p1 := f.closeToPayout(t, acc.Id)
	if _, err := f.settlePayout(p1, payoutFailed); err != nil {
		t.Fatal(err)
	}
	if got := f.step(t, acc.Id); got.CollateralMinor != 50000 || got.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING {
		t.Fatalf("money stays held after a failed payout: %+v", got)
	}
	r, err := f.svc.RetryPayout(ctx, &accountsv1.RetryPayoutRequest{AccountId: acc.Id, PayoutId: p1.Id})
	if err != nil || r.Payout.Attempt != 2 || r.Payout.AttemptId == p1.AttemptId {
		t.Fatalf("retry: %v %+v", err, r)
	}
	if _, err := f.settlePayout(p1, payoutSettled); grpcx.Reason(err) != "attempt_failed" {
		t.Fatalf("late settled for the failed attempt: %v", err)
	}
	if _, err := f.settlePayout(r.Payout, payoutSettled); err != nil {
		t.Fatal(err)
	}
	if n := f.payoutJournals(t, acc.Id); n != 1 {
		t.Fatalf("journals=%d", n)
	}
	if _, err := f.svc.RetryPayout(ctx, &accountsv1.RetryPayoutRequest{AccountId: acc.Id, PayoutId: p1.Id}); grpcx.Reason(err) != "payout_not_failed" {
		t.Fatalf("retry of a settled payout: %v", err)
	}
}

func TestPayoutWebhookChecks(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-po3", 1000)
	p := f.closeToPayout(t, acc.Id)
	ctx := context.Background()
	if _, err := f.svc.SettlePayout(ctx, &accountsv1.SettlePayoutRequest{AttemptId: p.AttemptId, AmountMinor: 1000, Currency: "USD", DestinationLast4: "0000", Status: payoutSettled}); grpcx.Reason(err) != "destination_mismatch" {
		t.Fatalf("last4: %v", err)
	}
	if _, err := f.svc.SettlePayout(ctx, &accountsv1.SettlePayoutRequest{AttemptId: p.AttemptId, AmountMinor: 999, Currency: "USD", DestinationLast4: "6789", Status: payoutSettled}); grpcx.Reason(err) != "payout_amount_mismatch" {
		t.Fatalf("amount: %v", err)
	}
	if _, err := f.svc.SettlePayout(ctx, &accountsv1.SettlePayoutRequest{AttemptId: "0190f6c4-0000-7000-8000-000000000000", AmountMinor: 1000, Currency: "USD", DestinationLast4: "6789", Status: payoutSettled}); grpcx.Reason(err) != "payout_not_found" {
		t.Fatalf("unknown attempt: %v", err)
	}
	if f.payoutJournals(t, acc.Id) != 0 {
		t.Fatal("no journal on a refused webhook")
	}
}

func TestPayoutIncludesCreditBalance(t *testing.T) {
	f, clk, _ := closeFixture(t)
	acc := f.withDebt(t, "cust-po4", 100000, 1000)
	p, err := f.createPayment(acc.Id, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SettlePayment(context.Background(), settlePay(p.Payment, "bank-po4", paySettled)); err != nil {
		t.Fatal(err)
	}
	var authID string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM authorizations WHERE account_id=$1 AND status='captured'`, acc.Id).Scan(&authID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.refund(authID, "rf-po4", 700); err != nil { // refund after paying → credit balance 700
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	clk.Advance(ReturnWindow + time.Hour) // the settled payment's return window passes
	if got := f.step(t, acc.Id); got.CloseStage != "payout" {
		t.Fatalf("stage=%s", got.CloseStage)
	}
	if po := f.openPayout(t, acc.Id); po.Amount.AmountMinor != 100700 {
		t.Fatalf("payout amount=%d want 100700", po.Amount.AmountMinor)
	}
}

func TestPayoutWaitsForReturnWindow(t *testing.T) {
	f, c, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-po5", 100000, 30000)
	p, err := f.createPayment(acc.Id, 30000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-po5", paySettled)); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	if got := f.step(t, acc.Id); got.CloseStage != "awaiting_returns" {
		t.Fatalf("stage=%s want awaiting_returns", got.CloseStage)
	}
	c.Advance(48 * time.Hour)
	if _, err := f.svc.ReturnPayment(ctx, &accountsv1.ReturnPaymentRequest{PaymentId: p.Payment.Id, ReturnRef: "ret-po5", AmountMinor: 30000}); err != nil {
		t.Fatal(err)
	}
	// Review I5: the return came before the final due date, so the customer can still pay; the
	// collateral is applied only once that due date passes.
	got := f.step(t, acc.Id)
	if got.CloseStage != "awaiting_due" || got.CollateralMinor != 100000 || got.OutstandingMinor != 30000 {
		t.Fatalf("return before the due date: %+v", got)
	}
	f.advance(t, acc.Id, 23)
	c.Advance(ReturnWindow) // and the returned payment's window is irrelevant now (returned)
	got = f.step(t, acc.Id)
	if got.CollateralMinor != 70000 || got.OutstandingMinor != 0 || got.CloseStage != "payout" {
		t.Fatalf("after the due date: %+v", got)
	}
	if po := f.openPayout(t, acc.Id); po.Amount.AmountMinor != 70000 {
		t.Fatalf("payout=%d want 70000", po.Amount.AmountMinor)
	}
}

func TestDestinationNeverLeaks(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-po6", 1000)
	p := f.closeToPayout(t, acc.Id)
	if _, err := f.settlePayout(p, payoutSettled); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload::text LIKE '%'||$1||'%'`, dest.AccountNumber); n != 0 {
		t.Fatalf("%d outbox events carry the full account number", n)
	}
	got, _ := f.svc.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc.Id})
	l, _ := f.svc.ListPayouts(context.Background(), &accountsv1.ListPayoutsRequest{AccountId: acc.Id})
	if strings.Contains(got.String()+l.String(), dest.AccountNumber) {
		t.Fatal("account or payout response carries the full account number")
	}
	var sealed string
	if err := f.pool.QueryRow(context.Background(), `SELECT destination_ciphertext FROM accounts WHERE id=$1`, acc.Id).Scan(&sealed); err != nil || strings.Contains(sealed, dest.AccountNumber) {
		t.Fatalf("stored destination must be encrypted: %v", err)
	}
}

func TestLateRefundAfterCloseAdditionalPayout(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-po7", 100000, 2000)
	var authID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM authorizations WHERE account_id=$1 AND status='captured'`, acc.Id).Scan(&authID); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	f.advance(t, acc.Id, 23)
	p := f.openPayout(t, f.step(t, acc.Id).Id) // 2000 applied from collateral → payout 98000
	if _, err := f.settlePayout(p, payoutSettled); err != nil {
		t.Fatal(err)
	}
	if _, err := f.refund(authID, "rf-po7", 500); err != nil {
		t.Fatalf("refund on a closed account: %v", err)
	}
	if err := f.svc.AdvanceClosing(ctx, 10); err != nil {
		t.Fatal(err)
	}
	p2 := f.openPayout(t, acc.Id)
	if p2.Amount.AmountMinor != 500 || p2.Id == p.Id {
		t.Fatalf("additional payout: %+v", p2)
	}
	if _, err := f.settlePayout(p2, payoutSettled); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if got.Account.CreditBalanceMinor != 0 || got.Account.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSED {
		t.Fatalf("after the additional payout: %+v", got.Account)
	}
}

func TestNothingToPayOut(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.withDebt(t, "cust-po8", 1000, 1000)
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	f.advance(t, acc.Id, 23)
	got := f.step(t, acc.Id)
	if got.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSED || got.CollateralMinor != 0 {
		t.Fatalf("debt = collateral → closed with no payout: %+v", got)
	}
	if r, _ := f.svc.ListPayouts(context.Background(), &accountsv1.ListPayoutsRequest{AccountId: acc.Id}); len(r.Payouts) != 0 {
		t.Fatalf("payouts: %+v", r.Payouts)
	}
}

// Review I7: with the collateral used up, a payment still waiting for the bank keeps the account
// closing (no closed account with in-flight work).
func TestPendingPaymentHoldsCloseAfterCollateralUsedUp(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-po9", 400, 400)
	if _, err := f.ledgerClient.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: acc.Id, Reference: "late_fee:po9:1",
		Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 600}); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	f.advance(t, acc.Id, 23)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET close_stage='awaiting_due' WHERE id=$1`, acc.Id); err != nil { // before the step
		t.Fatal(err)
	}
	if _, err := f.createPayment(acc.Id, 300, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.step(t, acc.Id); got.State == accountsv1.AccountState_ACCOUNT_STATE_CLOSED {
		t.Fatalf("closed while a payment is pending: %+v", got)
	}
}

// Review I4: concurrent saga runners apply the collateral once.
func TestConcurrentRunnersApplyCollateralOnce(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.withDebt(t, "cust-po10", 100000, 30000)
	f.close(t, acc.Id)
	f.advance(t, acc.Id, 23)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.svc.AdvanceClose(context.Background(), &accountsv1.AdvanceCloseRequest{AccountId: acc.Id})
		}()
	}
	wg.Wait()
	f.step(t, acc.Id)
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='collateral_applied'`, acc.Id); n != 1 {
		t.Fatalf("collateral applications=%d", n)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM payouts WHERE account_id=$1`, acc.Id); n != 1 {
		t.Fatalf("payouts=%d", n)
	}
}

// Review I3: a runner holding a stale read never creates a second payout after the first settled.
func TestConcurrentRunnersAndSettleNeverPayTwice(t *testing.T) {
	f, clk, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-po11", 100000, 1000)
	var authID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM authorizations WHERE account_id=$1 AND status='captured'`, acc.Id).Scan(&authID); err != nil {
		t.Fatal(err)
	}
	p, _ := f.createPayment(acc.Id, 1000, nil)
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-po11", paySettled)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.refund(authID, "rf-po11", 700); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	clk.Advance(ReturnWindow + time.Hour)
	f.step(t, acc.Id)
	po := f.openPayout(t, acc.Id)
	var wg sync.WaitGroup
	wg.Add(11)
	go func() { defer wg.Done(); _, _ = f.settlePayout(po, payoutSettled) }()
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			_, _ = f.svc.AdvanceClose(ctx, &accountsv1.AdvanceCloseRequest{AccountId: acc.Id})
		}()
	}
	wg.Wait()
	f.step(t, acc.Id)
	var paid int64
	if err := f.pool.QueryRow(ctx, `SELECT coalesce(sum(collateral_minor+credit_minor),0) FROM payouts WHERE account_id=$1 AND status IN ('pending','settling','settled')`, acc.Id).Scan(&paid); err != nil {
		t.Fatal(err)
	}
	if paid != 100700 {
		t.Fatalf("paid out %d want 100700 once", paid)
	}
}

// Review I2: an additional payout waits for the return window like the close payout.
func TestAdditionalPayoutWaitsForReturnWindow(t *testing.T) {
	f, clk, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-po12", 400, 400)
	var authID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM authorizations WHERE account_id=$1 AND status='captured'`, acc.Id).Scan(&authID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ledgerClient.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: acc.Id, Reference: "late_fee:po12:1",
		Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 600}); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	f.advance(t, acc.Id, 23)
	if got := f.step(t, acc.Id); got.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSED {
		t.Fatalf("fixture: %+v", got)
	}
	p, err := f.createPayment(acc.Id, 600, nil) // pays the leftover debt
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-po12", paySettled)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.refund(authID, "rf-po12", 100); err != nil { // → credit balance 100
		t.Fatal(err)
	}
	if err := f.svc.AdvanceClosing(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM payouts WHERE account_id=$1`, acc.Id); n != 0 {
		t.Fatalf("additional payout created inside the return window: %d", n)
	}
	clk.Advance(ReturnWindow + time.Hour)
	if err := f.svc.AdvanceClosing(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if po := f.openPayout(t, acc.Id); po.Amount.AmountMinor != 100 {
		t.Fatalf("additional payout: %+v", po)
	}
}

// Review I6: support can close without a destination (e.g. fraud); the payout waits until one is given.
func TestSupportCloseWithoutDestination(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-po13", 1000)
	r, err := f.svc.CloseAccount(ctx, &accountsv1.CloseAccountRequest{AccountId: acc.Id, Reason: "fraud review", Meta: &commonv1.RequestMeta{Actor: "agent-1"}})
	if err != nil || r.Account.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING {
		t.Fatalf("support close without destination: %v %+v", err, r)
	}
	if got := f.step(t, acc.Id); got.CloseStage != "payout" || got.DestinationLast4 != "" {
		t.Fatalf("stage=%s last4=%q", got.CloseStage, got.DestinationLast4)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM payouts WHERE account_id=$1`, acc.Id); n != 0 {
		t.Fatal("no payout without a destination")
	}
	if _, err := f.svc.CloseAccount(ctx, &accountsv1.CloseAccountRequest{AccountId: acc.Id, Destination: dest}); err != nil {
		t.Fatalf("customer supplies the destination: %v", err)
	}
	if po := f.openPayout(t, acc.Id); po.Amount.AmountMinor != 1000 || po.DestinationLast4 != "6789" {
		t.Fatalf("payout: %+v", po)
	}
	if _, err := f.svc.CloseAccount(ctx, &accountsv1.CloseAccountRequest{AccountId: acc.Id}); grpcx.Reason(err) != "invalid_destination" {
		t.Fatalf("customer close without destination: %v", err)
	}
}

// Review I1: parked closes never starve newer ones in the sweep.
func TestSweepRotatesParkedCloses(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	parked := f.withDebt(t, "cust-po14", 1000, 500)
	f.close(t, parked.Id) // parks in awaiting_due for 21 days
	fresh := f.activeAccount(t, "cust-po15", 1000)
	a := f.authorize(t, fresh.Id, "net-po15", 100).Authorization
	f.close(t, fresh.Id) // in_flight until the authorization resolves
	if _, err := f.reverse(a.Id, "rev-po15"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := f.svc.AdvanceClosing(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: fresh.Id})
	if got.Account.CloseStage == "in_flight" {
		t.Fatalf("newer close starved behind a parked one: %s", got.Account.CloseStage)
	}
}
