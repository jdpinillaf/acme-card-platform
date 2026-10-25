//go:build integration

package accounts

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/clock"
	"github.com/acme/acme-card/pkg/grpcx"
)

// flakyLedger performs the real ledger call, then reports failure once per armed op — the shape of
// a timeout after the ledger committed.
type flakyLedger struct {
	ledgerv1.LedgerServiceClient
	failPlace, failCapture, failRefund, failJournal atomic.Bool
}

func (l *flakyLedger) PostJournal(ctx context.Context, r *ledgerv1.PostJournalRequest, o ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	res, err := l.LedgerServiceClient.PostJournal(ctx, r, o...)
	if err == nil && l.failJournal.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

var errLost = status.Error(codes.DeadlineExceeded, "simulated: response lost after ledger commit")

func (l *flakyLedger) PlaceHold(ctx context.Context, r *ledgerv1.PlaceHoldRequest, o ...grpc.CallOption) (*ledgerv1.PlaceHoldResponse, error) {
	res, err := l.LedgerServiceClient.PlaceHold(ctx, r, o...)
	if err == nil && l.failPlace.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

func (l *flakyLedger) CaptureHold(ctx context.Context, r *ledgerv1.CaptureHoldRequest, o ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	res, err := l.LedgerServiceClient.CaptureHold(ctx, r, o...)
	if err == nil && l.failCapture.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

func (l *flakyLedger) PostRefund(ctx context.Context, r *ledgerv1.PostRefundRequest, o ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	res, err := l.LedgerServiceClient.PostRefund(ctx, r, o...)
	if err == nil && l.failRefund.CompareAndSwap(true, false) {
		return nil, errLost
	}
	return res, err
}

func flakyFixture(t *testing.T) (*fixture, *clock.Fake, *flakyLedger) {
	f := setup(t)
	c := clock.NewFake(t0)
	fl := &flakyLedger{LedgerServiceClient: f.ledgerClient}
	f.svc = NewService(f.pool, fl, WithClock(c))
	return f, c, fl
}

func (f *fixture) holdStatus(t *testing.T, ref string) string {
	var s string
	_ = f.ledger.QueryRow(context.Background(), `SELECT status FROM holds WHERE reference=$1`, "auth:"+ref).Scan(&s)
	return s
}

func TestCaptureRefUsedByAnotherAuthTouchesNothing(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-r1", 1000)
	a := f.authorize(t, acc.Id, "net-r1a", 100).Authorization
	b := f.authorize(t, acc.Id, "net-r1b", 200).Authorization
	if _, err := f.capture(a.Id, "C1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := f.capture(b.Id, "C1", 200); grpcx.Reason(err) != "network_ref_reuse" {
		t.Fatalf("reused capture ref: %v", err)
	}
	if st := f.holdStatus(t, "net-r1b"); st != "active" {
		t.Fatalf("B's hold must be untouched, got %s", st)
	}
	if _, recv := f.balances(t, acc.Id); recv != 100 {
		t.Fatalf("receivable=%d want 100 (no charge for the refused capture)", recv)
	}
	if _, err := f.capture(b.Id, "C2", 200); err != nil {
		t.Fatalf("B must still be capturable: %v", err)
	}
}

func TestReversalRefUsedByAnotherAuthTouchesNothing(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-r2", 1000)
	a := f.authorize(t, acc.Id, "net-r2a", 100).Authorization
	b := f.authorize(t, acc.Id, "net-r2b", 200).Authorization
	_, _ = f.reverse(a.Id, "V1")
	if _, err := f.reverse(b.Id, "V1"); grpcx.Reason(err) != "network_ref_reuse" {
		t.Fatalf("reused reversal ref: %v", err)
	}
	if st := f.holdStatus(t, "net-r2b"); st != "active" {
		t.Fatalf("B's hold must stay active, got %s", st)
	}
	if _, err := f.capture(b.Id, "C-r2b", 200); err != nil {
		t.Fatalf("B must still be capturable: %v", err)
	}
}

func TestExpiryPoisonRowDoesNotBlockOthers(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-r3", 1000)
	poison := f.authorize(t, acc.Id, "net-r3-poison", 100).Authorization
	healthy := f.authorize(t, acc.Id, "net-r3-ok", 100).Authorization
	// Corrupt: the ledger hold is already captured behind accounts' back.
	if _, err := f.ledgerClient.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{HoldReference: "auth:net-r3-poison", JournalReference: "rogue", AmountMinor: 100}); err != nil {
		t.Fatal(err)
	}
	c.Advance(HoldTTL + time.Second)
	if _, err := f.svc.ExpireDue(ctx, 100); err != nil {
		t.Fatalf("one bad row must not fail the batch: %v", err)
	}
	var st string
	_ = f.pool.QueryRow(ctx, `SELECT status FROM authorizations WHERE id=$1`, healthy.Id).Scan(&st)
	if st != "expired" {
		t.Fatalf("healthy authorization status=%s want expired", st)
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM authorizations WHERE id=$1 AND expiry_failed_at IS NOT NULL`, poison.Id); n != 1 {
		t.Fatal("poison row must be marked and skipped")
	}
	if n, err := f.svc.ExpireDue(ctx, 100); err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v", n, err)
	}
}

func TestAuthorizeLostResponseIsFailedAndHoldReleased(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-r4", 1000)
	fl.failPlace.Store(true)
	if _, err := f.svc.Authorize(ctx, authReq(acc.Id, "net-r4", 300)); err == nil {
		t.Fatal("expected the simulated lost response")
	}
	if st := f.holdStatus(t, "net-r4"); st != "active" {
		t.Fatalf("precondition: ledger hold placed, got %q", st)
	}
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.holdStatus(t, "net-r4"); st != "active" {
		t.Fatal("fresh pending authorizations must not be resolved yet")
	}
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.holdStatus(t, "net-r4"); st != "released" {
		t.Fatalf("orphan hold must be released, got %s", st)
	}
	r, err := f.svc.Authorize(ctx, authReq(acc.Id, "net-r4", 300))
	if err != nil || r.Authorization.Status != accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED || !r.Replayed {
		t.Fatalf("late retry must replay failed: %v %+v", err, r)
	}
	if av, _ := f.balances(t, acc.Id); av != 1000 {
		t.Fatalf("available=%d want 1000", av)
	}
}

func TestAuthorizeRetryResumesPending(t *testing.T) {
	f, _, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-r5", 1000)
	fl.failPlace.Store(true)
	_, _ = f.svc.Authorize(ctx, authReq(acc.Id, "net-r5", 300))
	r, err := f.svc.Authorize(ctx, authReq(acc.Id, "net-r5", 300))
	if err != nil || r.Authorization.Status != approved {
		t.Fatalf("retry must resume and approve: %v %+v", err, r)
	}
	if h := f.ledgerHolds(t, acc.Id); h != 300 {
		t.Fatalf("holds=%d want exactly one hold of 300", h)
	}
}

func TestCaptureLostResponseIsResumed(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-r6", 1000)
	a := f.authorize(t, acc.Id, "net-r6", 300).Authorization
	fl.failCapture.Store(true)
	if _, err := f.capture(a.Id, "C-r6", 250); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := f.capture(a.Id, "C-other", 250); grpcx.Reason(err) != "operation_in_progress" {
		t.Fatalf("competing capture while pending: %v", err)
	}
	if _, err := f.reverse(a.Id, "V-r6"); grpcx.Reason(err) != "operation_in_progress" {
		t.Fatalf("reversal while capture pending: %v", err)
	}
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	var captured int64
	_ = f.pool.QueryRow(ctx, `SELECT status, captured_minor FROM authorizations WHERE id=$1`, a.Id).Scan(&st, &captured)
	if st != "captured" || captured != 250 {
		t.Fatalf("reconciled capture: status=%s captured=%d", st, captured)
	}
	if r, err := f.capture(a.Id, "C-r6", 250); err != nil || !r.Replayed {
		t.Fatalf("retry after reconcile must replay: %v", err)
	}
}

func TestRefundLostResponseCannotDoubleCredit(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-r7", 1000)
	a := f.authorize(t, acc.Id, "net-r7", 200).Authorization
	_, _ = f.capture(a.Id, "C-r7", 200)
	fl.failRefund.Store(true)
	if _, err := f.refund(a.Id, "R-1", 150); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := f.refund(a.Id, "R-2", 150); grpcx.Reason(err) != "refund_exceeds_captured" {
		t.Fatalf("pending refund must count toward the cap: %v", err)
	}
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	var refunded int64
	_ = f.pool.QueryRow(ctx, `SELECT refunded_minor FROM authorizations WHERE id=$1`, a.Id).Scan(&refunded)
	var ledgerRefunds int64
	_ = f.ledger.QueryRow(ctx, `SELECT coalesce(sum(amount_minor),0) FROM postings p JOIN journal_entries j ON j.id=p.journal_entry_id
		WHERE j.account_id=$1 AND j.entry_type='purchase_refunded' AND p.side='debit'`, acc.Id).Scan(&ledgerRefunds)
	if refunded != 150 || ledgerRefunds != 150 {
		t.Fatalf("accounts refunded=%d ledger refunds=%d, both must be 150", refunded, ledgerRefunds)
	}
}

var _ = errors.New

func TestSettlingDepositIsResumedByReconciler(t *testing.T) {
	f, c, fl := flakyFixture(t)
	ctx := context.Background()
	acc := f.open(t, "cust-r8")
	dep := f.deposit(t, acc.Id, 1000)
	fl.failJournal.Store(true)
	if _, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-r8")); err == nil {
		t.Fatal("expected lost response")
	}
	f.ledgerMatchesAccountsOrSettling(t, dep.Id)
	c.Advance(PendingTimeout + time.Second)
	if err := f.svc.ResolvePending(ctx); err != nil {
		t.Fatal(err)
	}
	g, _ := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if g.Account.State != accountsv1.AccountState_ACCOUNT_STATE_ACTIVE || g.Account.CreditLimitMinor != 1000 {
		t.Fatalf("reconciled account: %+v", g.Account)
	}
	f.ledgerMatchesAccounts(t, acc.Id, dep.Id)
}

func TestSagaSurvivesClientCancellationAfterReservation(t *testing.T) {
	f, _, _ := flakyFixture(t)
	acc := f.activeAccount(t, "cust-r9", 1000)
	// Cancel the caller's context as soon as the ledger has the hold: the finalize step must
	// still run, detached from the caller.
	cl := &cancelAfterPlace{LedgerServiceClient: f.ledgerClient}
	f.svc = NewService(f.pool, cl, WithClock(f.svc.clock))
	ctx, cancel := context.WithCancel(context.Background())
	cl.cancel = cancel
	_, _ = f.svc.Authorize(ctx, authReq(acc.Id, "net-r9", 300))
	var st string
	_ = f.pool.QueryRow(context.Background(), `SELECT status FROM authorizations WHERE network_ref='net-r9'`).Scan(&st)
	if st != "approved" {
		t.Fatalf("status=%s want approved (finalize must not depend on the caller staying connected)", st)
	}
}

type cancelAfterPlace struct {
	ledgerv1.LedgerServiceClient
	cancel context.CancelFunc
}

func (c *cancelAfterPlace) PlaceHold(ctx context.Context, r *ledgerv1.PlaceHoldRequest, o ...grpc.CallOption) (*ledgerv1.PlaceHoldResponse, error) {
	res, err := c.LedgerServiceClient.PlaceHold(ctx, r, o...)
	c.cancel()
	return res, err
}
