//go:build integration

package billing

import (
	"context"
	"testing"
	"time"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	"github.com/acme/acme-card/services/billing/internal/engine"
)

func (f *fixture) requestClose(t *testing.T, acc string) {
	t.Helper()
	if _, err := f.accounts.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: acc,
		Destination: &accountsv1.Destination{BankCode: "BANK001", AccountNumber: "000123456789", HolderName: "Jane Doe"}}); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeStatement(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	c1 := f.closeN(t, acc, 1)
	f.advanceTo(t, acc.Id, c1.Add(10*dayDur)) // statement 1 closed; grace lost from here on
	f.requestClose(t, acc.Id)
	now := f.account(t, acc.Id).AccountTime.AsTime()
	r, err := f.svc.FinalizeStatement(ctx, &billingv1.FinalizeStatementRequest{AccountId: acc.Id})
	if err != nil {
		t.Fatal(err)
	}
	st := r.Statement
	// Account time runs with the real clock, so the cut is within a second of the earlier read.
	if d := st.PeriodEnd.AsTime().Sub(now); !st.Final || st.Cycle != 2 || !st.PeriodStart.AsTime().Equal(c1) || d < 0 || d > time.Second {
		t.Fatalf("final statement: %+v (now %v)", st, now)
	}
	if !r.DueDate.AsTime().Equal(engine.DueDate(st.PeriodEnd.AsTime())) || st.GeneratedAt.AsTime().IsZero() {
		t.Fatalf("due=%v want %v; generated_at=%v", r.DueDate.AsTime(), engine.DueDate(st.PeriodEnd.AsTime()), st.GeneratedAt.AsTime())
	}
	if st.InterestMinor <= 0 {
		t.Fatalf("interest up to the close date (grace lost): %+v", st)
	}
	again, err := f.svc.FinalizeStatement(ctx, &billingv1.FinalizeStatementRequest{AccountId: acc.Id})
	if err != nil || again.Statement.Id != st.Id {
		t.Fatalf("second call must return the same statement: %v %+v", err, again)
	}
	charges := len(f.charges(t, acc.Id, ""))
	f.advanceTo(t, acc.Id, now.Add(90*dayDur))
	if n := len(f.statements(t, acc.Id)); n != 2 {
		t.Fatalf("statements after the final one: %d", n)
	}
	if n := len(f.charges(t, acc.Id, "")); n != charges {
		t.Fatalf("charges after the final statement: %d → %d", charges, n)
	}
	f.assertReconciles(t, acc.Id)
}

func TestFinalizeStatementRequiresClosing(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 1000)
	if _, err := f.svc.FinalizeStatement(context.Background(), &billingv1.FinalizeStatementRequest{AccountId: acc.Id}); err == nil {
		t.Fatal("an active account has no final statement")
	}
}

func TestCatchUpContinuesWhileClosing(t *testing.T) {
	f := setup(t)
	acc := f.activeAccount(t, 200000)
	f.purchase(t, acc.Id, 100000)
	f.requestClose(t, acc.Id)
	f.advanceTo(t, acc.Id, engine.OnTimeCutoff(engine.DueDate(f.closeN(t, acc, 1))))
	if n := len(f.statements(t, acc.Id)); n != 1 {
		t.Fatalf("regular statements continue until the final one: %d", n)
	}
	if n := len(f.charges(t, acc.Id, "fee")); n != 1 {
		t.Fatalf("late fee before the final statement: %d", n)
	}
}
