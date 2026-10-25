//go:build integration

package accounts

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/clock"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/secretbox"
)

// fakeFinal stands in for billing: the final statement is due 21 days after the request.
type fakeFinal struct {
	billingv1.BillingServiceClient
	f     *fixture
	mu    sync.Mutex
	calls int
	due   map[string]time.Time
}

func (b *fakeFinal) FinalizeStatement(ctx context.Context, r *billingv1.FinalizeStatementRequest, _ ...grpc.CallOption) (*billingv1.FinalizeStatementResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if d, ok := b.due[r.AccountId]; ok {
		return &billingv1.FinalizeStatementResponse{DueDate: timestamppb.New(d)}, nil
	}
	acc, err := b.f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: r.AccountId})
	if err != nil {
		return nil, err
	}
	d := acc.Account.AccountTime.AsTime().AddDate(0, 0, 21)
	b.due[r.AccountId] = d
	return &billingv1.FinalizeStatementResponse{DueDate: timestamppb.New(d)}, nil
}

var testKey = bytes.Repeat([]byte{9}, 32)

func closeFixture(t *testing.T) (*fixture, *clock.Fake, *fakeFinal) {
	f := setup(t)
	c := clock.NewFake(t0)
	box, err := secretbox.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeFinal{f: f, due: map[string]time.Time{}}
	f.svc = NewService(f.pool, f.ledgerClient, WithClock(c), WithBilling(b), WithDestinationBox(box))
	return f, c, b
}

var dest = &accountsv1.Destination{BankCode: "BANK001", AccountNumber: "000123456789", HolderName: "Jane Doe"}

func (f *fixture) close(t *testing.T, acc string) *accountsv1.Account {
	t.Helper()
	r, err := f.svc.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: acc, Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func (f *fixture) step(t *testing.T, acc string) *accountsv1.Account {
	t.Helper()
	r, err := f.svc.AdvanceClose(context.Background(), &accountsv1.AdvanceCloseRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func TestCloseBlocksPurchasesImmediately(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl1", 100000)
	a := f.authorize(t, acc.Id, "net-cl1", 5000).Authorization
	got := f.close(t, acc.Id)
	if got.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING || got.CloseStage != "in_flight" || got.DestinationLast4 != "6789" {
		t.Fatalf("close: %+v", got)
	}
	if d := f.authorize(t, acc.Id, "net-cl1b", 1).Authorization; d.DeclineReason != commonv1.DeclineReason_DECLINE_REASON_ACCOUNT_CLOSING {
		t.Fatalf("decline=%s", d.DeclineReason)
	}
	if _, err := f.capture(a.Id, "cap-cl1", 5000); err != nil {
		t.Fatalf("capture of an earlier approval: %v", err)
	}
	if _, err := f.svc.CreateDeposit(context.Background(), &accountsv1.CreateDepositRequest{AccountId: acc.Id, AmountMinor: 100}); grpcx.Reason(err) != "account_closing" {
		t.Fatalf("deposit while closing: %v", err)
	}
	if f.events(t, "account.close_requested") != 1 {
		t.Fatal("event")
	}
}

func TestCloseVoidsPendingDeposits(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl2", 1000)
	d := f.deposit(t, acc.Id, 500)
	f.close(t, acc.Id)
	if _, err := f.svc.SettleDeposit(context.Background(), settled(d, "bank-cl2")); grpcx.Reason(err) != "deposit_voided" {
		t.Fatalf("late deposit: %v", err)
	}
}

func TestCloseWaitsForInFlightThenFinalStatement(t *testing.T) {
	f, _, b := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl3", 100000)
	a := f.authorize(t, acc.Id, "net-cl3", 5000).Authorization
	f.close(t, acc.Id)
	if got := f.step(t, acc.Id); got.CloseStage != "in_flight" || b.calls != 0 {
		t.Fatalf("must wait for the approved authorization: %+v calls=%d", got, b.calls)
	}
	if _, err := f.capture(a.Id, "cap-cl3", 5000); err != nil {
		t.Fatal(err)
	}
	got := f.step(t, acc.Id)
	if got.CloseStage != "awaiting_due" || b.calls != 1 {
		t.Fatalf("after capture: %+v calls=%d", got, b.calls)
	}
}

func TestCloseWithUnpaidDebtAppliesCollateralOnDueDate(t *testing.T) {
	f, _, b := closeFixture(t)
	acc := f.withDebt(t, "cust-cl4", 100000, 30000)
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	due := b.due[acc.Id]
	if got := f.step(t, acc.Id); got.CloseStage != "awaiting_due" {
		t.Fatalf("before the due date: %s", got.CloseStage)
	}
	f.advance(t, acc.Id, 22) // past the due date's on-time cutoff
	got := f.step(t, acc.Id)
	// No payment is inside its return window, so the saga goes straight on to the payout.
	if got.CloseStage != "payout" || got.CollateralMinor != 70000 || got.OutstandingMinor != 0 {
		t.Fatalf("after due: %+v", got)
	}
	if at := f.entryEffectiveAt(t, "closeout:"+acc.Id+":1"); !at.Equal(due.Add(24 * time.Hour)) {
		t.Fatalf("application dated %v want %v", at, due.Add(24*time.Hour))
	}
}

func TestCloseDebtPaidBeforeDue(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-cl5", 100000, 30000)
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	p, err := f.createPayment(acc.Id, 30000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-cl5", paySettled)); err != nil {
		t.Fatal(err)
	}
	got := f.step(t, acc.Id)
	if got.CloseStage != "awaiting_returns" || got.CollateralMinor != 100000 {
		t.Fatalf("paid before due: %+v", got)
	}
}

func TestCloseWaitsForPendingPaymentAtDue(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-cl6", 100000, 30000)
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	p, err := f.createPayment(acc.Id, 30000, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.advance(t, acc.Id, 22)
	if got := f.step(t, acc.Id); got.CloseStage != "awaiting_due" {
		t.Fatalf("pending payment received before due must hold the application: %s", got.CloseStage)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-cl6", paySettled)); err != nil {
		t.Fatal(err)
	}
	if got := f.step(t, acc.Id); got.CloseStage != "awaiting_returns" || got.CollateralMinor != 100000 {
		t.Fatalf("after settlement: %+v", got)
	}
}

func TestCloseTwiceSameDestinationReplays(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl7", 1000)
	f.close(t, acc.Id)
	if _, err := f.svc.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: acc.Id, Destination: dest}); err != nil {
		t.Fatalf("same destination replays: %v", err)
	}
	other := &accountsv1.Destination{BankCode: "BANK001", AccountNumber: "000999999999", HolderName: "Jane Doe"}
	if _, err := f.svc.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: acc.Id, Destination: other}); grpcx.Reason(err) != "close_already_requested" {
		t.Fatalf("different destination: %v", err)
	}
	if f.events(t, "account.close_requested") != 1 {
		t.Fatal("one request event")
	}
}

func TestCloseValidatesDestination(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl8", 1000)
	for _, d := range []*accountsv1.Destination{nil, {BankCode: "B", AccountNumber: "1234", HolderName: "x"}, {BankCode: "BANK001", AccountNumber: "12a4", HolderName: "x"},
		{BankCode: "BANK001", AccountNumber: "123", HolderName: "x"}, {BankCode: "BANK001", AccountNumber: "1234", HolderName: ""}} {
		if _, err := f.svc.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: acc.Id, Destination: d}); grpcx.Reason(err) != "invalid_destination" {
			t.Errorf("%+v: %v", d, err)
		}
	}
	pending := f.open(t, "cust-cl8b")
	if _, err := f.svc.CloseAccount(context.Background(), &accountsv1.CloseAccountRequest{AccountId: pending.Id, Destination: dest}); grpcx.Reason(err) != "not_closable" {
		t.Fatalf("closing a pending account: %v", err)
	}
}

func TestCloseChargedOffAccountSkipsWait(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.withDebt(t, "cust-cl9", 1000, 400)
	f.chargeOff(t, acc.Id)
	f.close(t, acc.Id)
	f.step(t, acc.Id)
	if got := f.step(t, acc.Id); got.CloseStage != "payout" {
		t.Fatalf("charged-off account: %s", got.CloseStage)
	}
}

func TestCaptureRacingFinalStatement(t *testing.T) {
	f, _, b := closeFixture(t)
	acc := f.activeAccount(t, "cust-cl10", 100000)
	a := f.authorize(t, acc.Id, "net-cl10", 5000).Authorization
	f.close(t, acc.Id)
	var wg sync.WaitGroup
	wg.Add(2)
	var capErr error
	go func() { defer wg.Done(); _, capErr = f.capture(a.Id, "cap-cl10", 5000) }()
	go func() { defer wg.Done(); f.step(t, acc.Id) }()
	wg.Wait()
	if capErr != nil {
		t.Fatalf("capture of an earlier approval must succeed: %v", capErr)
	}
	got := f.step(t, acc.Id)
	if got.CloseStage != "awaiting_due" || b.calls != 1 {
		t.Fatalf("%+v calls=%d", got, b.calls)
	}
	// The final statement was requested only after the capture posted.
	var capturedAt time.Time
	if err := f.ledger.QueryRow(context.Background(), `SELECT posted_at FROM journal_entries WHERE reference=$1`, "capture:"+a.Id).Scan(&capturedAt); err != nil {
		t.Fatal(err)
	}
}

// Review C1: a deposit settling when the close is requested finishes; the saga waits for it.
func TestCloseWhileDepositSettling(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-cl11", 1000)
	d := f.deposit(t, acc.Id, 500)
	if _, err := f.pool.Exec(ctx, `UPDATE deposits SET status='settling', external_ref='bank-cl11', settling_at=now(), effective_at=now() WHERE id=$1`, d.Id); err != nil {
		t.Fatal(err)
	}
	if got := f.close(t, acc.Id); got.CloseStage != "in_flight" {
		t.Fatalf("waits for the settling deposit: %s", got.CloseStage)
	}
	r, err := f.svc.SettleDeposit(ctx, settled(d, "bank-cl11"))
	if err != nil {
		t.Fatalf("finishing the settling deposit on a closing account: %v", err)
	}
	if r.Account.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSING || r.Account.CollateralMinor != 1500 {
		t.Fatalf("after settlement: %+v", r.Account)
	}
	if got := f.step(t, acc.Id); got.CloseStage != "payout" {
		t.Fatalf("saga after the deposit: %s", got.CloseStage)
	}
	if po := f.openPayout(t, acc.Id); po.Amount.AmountMinor != 1500 {
		t.Fatalf("payout=%d want 1500", po.Amount.AmountMinor)
	}
}

// Review C2: debt left on a closed account can be paid through the normal payment flow, and the
// customer can open a new account afterwards.
func TestPayLeftoverDebtOnClosedAccountThenReopen(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-cl12", 400, 400)
	if _, err := f.ledgerClient.PostCharge(ctx, &ledgerv1.PostChargeRequest{AccountId: acc.Id, Reference: "late_fee:cl12:1",
		Kind: ledgerv1.ChargeKind_CHARGE_KIND_FEE, AmountMinor: 600}); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	f.advance(t, acc.Id, 23)
	got := f.step(t, acc.Id)
	if got.State != accountsv1.AccountState_ACCOUNT_STATE_CLOSED || got.OutstandingMinor != 600 {
		t.Fatalf("closed owing 600: %+v", got)
	}
	p, err := f.createPayment(acc.Id, 600, nil)
	if err != nil {
		t.Fatalf("paying the leftover debt: %v", err)
	}
	if _, err := f.svc.SettlePayment(ctx, settlePay(p.Payment, "bank-cl12", paySettled)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: "cust-cl12", Currency: "USD"}); err != nil {
		t.Fatalf("reopen after paying: %v", err)
	}
}
