//go:build integration

package accounts

import (
	"context"
	"testing"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func (f *fixture) cancel(acc string) (*accountsv1.GetAccountResponse, error) {
	return f.svc.CancelAccount(context.Background(), &accountsv1.CancelAccountRequest{AccountId: acc})
}

func TestCancelVoidsPendingDeposits(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.open(t, "cust-cx1")
	d1, d2 := f.deposit(t, acc.Id, 1000), f.deposit(t, acc.Id, 2000)
	r, err := f.cancel(acc.Id)
	if err != nil || r.Account.State != accountsv1.AccountState_ACCOUNT_STATE_CANCELLED {
		t.Fatalf("cancel: %v %+v", err, r)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM deposits WHERE account_id=$1 AND status='voided'`, acc.Id); n != 2 {
		t.Fatalf("voided=%d", n)
	}
	if f.events(t, "deposit.voided") != 2 || f.events(t, "account.cancelled") != 1 {
		t.Fatal("events")
	}
	if _, err := f.svc.SettleDeposit(context.Background(), settled(d1, "bank-cx1")); grpcx.Reason(err) != "deposit_voided" {
		t.Fatalf("late settlement: %v", err)
	}
	_ = d2
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1`, acc.Id); n != 0 {
		t.Fatalf("journals=%d", n)
	}
	if _, err := f.cancel(acc.Id); err != nil {
		t.Fatalf("cancel twice must replay: %v", err)
	}
	if _, err := f.svc.CreateDeposit(context.Background(), &accountsv1.CreateDepositRequest{AccountId: acc.Id, AmountMinor: 100}); grpcx.Reason(err) != "account_closed" {
		t.Fatalf("deposit on cancelled: %v", err)
	}
}

func TestCancelWhileDepositSettling(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.open(t, "cust-cx2")
	d := f.deposit(t, acc.Id, 1000)
	if _, err := f.pool.Exec(context.Background(), `UPDATE deposits SET status='settling', external_ref='b', settling_at=now() WHERE id=$1`, d.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cancel(acc.Id); grpcx.Reason(err) != "deposit_settling" {
		t.Fatalf("cancel while settling: %v", err)
	}
}

func TestCancelActiveAccountRejected(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-cx3", 1000)
	if _, err := f.cancel(acc.Id); grpcx.Reason(err) != "not_cancellable" {
		t.Fatalf("cancel active: %v", err)
	}
}

func TestReopenRules(t *testing.T) {
	f, _ := txFixture(t)
	ctx := context.Background()
	create := func(cust string) (*accountsv1.CreateAccountResponse, error) {
		return f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: cust, Currency: "USD"})
	}
	// Cancelled → a new account in the same currency.
	a := f.open(t, "cust-ro1")
	if _, err := f.cancel(a.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := create("cust-ro1"); err != nil {
		t.Fatalf("after cancel: %v", err)
	}
	// Active → still one per currency.
	if _, err := create("cust-ro1"); grpcx.Reason(err) != "account_exists" {
		t.Fatalf("duplicate: %v", err)
	}
	// Closed with no debt → allowed; closed with debt → refused until paid.
	b := f.activeAccount(t, "cust-ro2", 1000)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET state='closed' WHERE id=$1`, b.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := create("cust-ro2"); err != nil {
		t.Fatalf("after clean close: %v", err)
	}
	c := f.withDebt(t, "cust-ro3", 1000, 400)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET state='closed' WHERE id=$1`, c.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := create("cust-ro3"); grpcx.Reason(err) != "previous_account_has_debt" {
		t.Fatalf("closed with debt: %v", err)
	}
	if _, err := f.ledgerClient.PostPayment(ctx, &ledgerv1.PostPaymentRequest{AccountId: c.Id, Reference: "payment:ro3", AmountMinor: 400}); err != nil {
		t.Fatal(err)
	}
	if _, err := create("cust-ro3"); err != nil {
		t.Fatalf("after the leftover debt is paid: %v", err)
	}
}
