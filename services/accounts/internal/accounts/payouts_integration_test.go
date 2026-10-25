//go:build integration

package accounts

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
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
	if _, err := f.svc.SettlePayout(ctx, &accountsv1.SettlePayoutRequest{AttemptId: p.AttemptId, AmountMinor: 999, Currency: "USD", DestinationLast4: "6789", Status: payoutSettled}); grpcx.Reason(err) != "amount_mismatch" {
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
	got := f.step(t, acc.Id) // the return re-raised the debt after the due date: collateral covers it
	if got.CollateralMinor != 70000 || got.OutstandingMinor != 0 || got.CloseStage != "payout" {
		t.Fatalf("after the return: %+v", got)
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
