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
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

const (
	capturedSt = accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED
	reversedSt = accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_REVERSED
)

func (f *fixture) capture(id, ref string, amt int64) (*accountsv1.AuthorizationResponse, error) {
	return f.svc.Capture(context.Background(), &accountsv1.CaptureRequest{AuthorizationId: id, NetworkRef: ref, AmountMinor: amt})
}

func (f *fixture) reverse(id, ref string) (*accountsv1.AuthorizationResponse, error) {
	return f.svc.Reverse(context.Background(), &accountsv1.ReverseRequest{AuthorizationId: id, NetworkRef: ref})
}

func (f *fixture) refund(id, ref string, amt int64) (*accountsv1.RefundResponse, error) {
	return f.svc.Refund(context.Background(), &accountsv1.RefundRequest{AuthorizationId: id, NetworkRef: ref, AmountMinor: amt})
}

func (f *fixture) balances(t *testing.T, acc string) (available, receivable int64) {
	b, err := f.ledgerClient.GetBalances(context.Background(), &ledgerv1.GetBalancesRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range b.Balances {
		if x.SubAccount == ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE {
			receivable = x.AmountMinor
		}
	}
	return b.AvailableMinor, receivable
}

func TestCaptureLessThanAuthorizedReleasesRest(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p1", 1000)
	a := f.authorize(t, acc.Id, "net-p1", 300).Authorization
	r, err := f.capture(a.Id, "cap-p1", 200)
	if err != nil || r.Authorization.Status != capturedSt || r.Authorization.CapturedAmountMinor != 200 {
		t.Fatalf("%v %+v", err, r)
	}
	if av, recv := f.balances(t, acc.Id); av != 800 || recv != 200 {
		t.Fatalf("available=%d receivable=%d", av, recv)
	}
	if _, err := f.capture(a.Id, "cap-p1-x", 1); grpcx.Reason(err) != "already_captured" {
		t.Fatalf("second capture: %v", err)
	}
	if r, err := f.capture(a.Id, "cap-p1", 200); err != nil || !r.Replayed {
		t.Fatalf("replay: %v %+v", err, r)
	}
	if _, err := f.capture(a.Id, "cap-p1", 199); grpcx.Reason(err) != "network_ref_reuse" {
		t.Fatalf("same ref different amount: %v", err)
	}
	b := f.authorize(t, acc.Id, "net-p1b", 100).Authorization
	if _, err := f.capture(b.Id, "cap-p1b", 101); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("capture above authorized: %v", err)
	}
	if f.events(t, "authorization.captured") != 1 {
		t.Fatal("captured event")
	}
}

func TestCaptureAfterExpiryRefused(t *testing.T) {
	f, c := txFixture(t)
	acc := f.activeAccount(t, "cust-p2", 1000)
	a := f.authorize(t, acc.Id, "net-p2", 100).Authorization
	c.Advance(HoldTTL + time.Second)
	if _, err := f.capture(a.Id, "cap-p2", 100); grpcx.Reason(err) != "authorization_expired" {
		t.Fatalf("capture after expiry: %v", err)
	}
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE reference=$1`, "capture:"+a.Id); n != 0 {
		t.Fatal("expired capture must post nothing")
	}
}

func TestCaptureVsReverseExactlyOne(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p3", 100_000)
	f.svc = NewService(f.pool, f.ledgerClient, WithClock(f.svc.clock), WithVelocity(1000, time.Minute))
	for i := 0; i < 25; i++ {
		a := f.authorize(t, acc.Id, fmt.Sprintf("net-p3-%d", i), 100).Authorization
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.capture(a.Id, "cap-"+a.Id, 100); err == nil {
				wins.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := f.reverse(a.Id, "rev-"+a.Id); err == nil {
				wins.Add(1)
			}
		}()
		close(start)
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("round %d: winners=%d", i, wins.Load())
		}
		var authStatus, holdStatus string
		_ = f.pool.QueryRow(context.Background(), `SELECT status FROM authorizations WHERE id=$1`, a.Id).Scan(&authStatus)
		_ = f.ledger.QueryRow(context.Background(), `SELECT status FROM holds WHERE reference=$1`, "auth:"+a.NetworkRef).Scan(&holdStatus)
		if !(authStatus == "captured" && holdStatus == "captured") && !(authStatus == "reversed" && holdStatus == "released") {
			t.Fatalf("round %d: authorization=%s hold=%s", i, authStatus, holdStatus)
		}
	}
}

func TestReversalWindow(t *testing.T) {
	f, c := txFixture(t)
	acc := f.activeAccount(t, "cust-p4", 1000)
	a := f.authorize(t, acc.Id, "net-p4", 400).Authorization
	r, err := f.reverse(a.Id, "rev-p4")
	if err != nil || r.Authorization.Status != reversedSt {
		t.Fatalf("%v %+v", err, r)
	}
	if av, _ := f.balances(t, acc.Id); av != 1000 {
		t.Fatalf("available=%d", av)
	}
	if r, err := f.reverse(a.Id, "rev-p4"); err != nil || !r.Replayed {
		t.Fatalf("replay: %v", err)
	}
	b := f.authorize(t, acc.Id, "net-p4b", 100).Authorization
	c.Advance(ReversalWindow + time.Second)
	if _, err := f.reverse(b.Id, "rev-p4b"); grpcx.Reason(err) != "reversal_window_expired" {
		t.Fatalf("late reversal: %v", err)
	}
	cap := f.authorize(t, acc.Id, "net-p4c", 100).Authorization
	_, _ = f.capture(cap.Id, "cap-p4c", 100)
	if _, err := f.reverse(cap.Id, "rev-p4c"); grpcx.Reason(err) != "already_captured" {
		t.Fatalf("reverse captured: %v", err)
	}
}

func TestPartialRefundsUpToCaptured(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p5", 1000)
	a := f.authorize(t, acc.Id, "net-p5", 200).Authorization
	_, _ = f.capture(a.Id, "cap-p5", 200)
	if _, err := f.refund(a.Id, "rf-1", 120); err != nil {
		t.Fatal(err)
	}
	r, err := f.refund(a.Id, "rf-2", 80)
	if err != nil || r.Authorization.RefundedAmountMinor != 200 || r.Refund.Amount.AmountMinor != 80 {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := f.refund(a.Id, "rf-3", 1); grpcx.Reason(err) != "refund_exceeds_captured" {
		t.Fatalf("over-refund: %v", err)
	}
	if r, err := f.refund(a.Id, "rf-1", 120); err != nil || !r.Replayed {
		t.Fatalf("replay: %v", err)
	}
	if _, err := f.refund(a.Id, "rf-1", 121); grpcx.Reason(err) != "network_ref_reuse" {
		t.Fatalf("ref reuse: %v", err)
	}
	if av, recv := f.balances(t, acc.Id); av != 1000 || recv != 0 {
		t.Fatalf("available=%d receivable=%d", av, recv)
	}

	b := f.authorize(t, acc.Id, "net-p5b", 200).Authorization
	_, _ = f.capture(b.Id, "cap-p5b", 200)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.refund(b.Id, fmt.Sprintf("rf-c-%d", i), 150); err == nil {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("concurrent refunds over captured: successes=%d", ok.Load())
	}
	if f.events(t, "refund.posted") != 3 {
		t.Fatalf("refund.posted=%d", f.events(t, "refund.posted"))
	}
}

func TestRefundBeforeCaptureRefused(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p6", 1000)
	a := f.authorize(t, acc.Id, "net-p6", 100).Authorization
	if _, err := f.refund(a.Id, "rf-p6", 10); grpcx.Reason(err) != "not_captured" {
		t.Fatalf("%v", err)
	}
	if _, err := f.capture("01a17300-0000-7000-8000-000000000000", "x", 1); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown authorization: %v", err)
	}
}

func TestClosedAccountFollowUps(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p7", 1000)
	toCapture := f.authorize(t, acc.Id, "net-p7a", 100).Authorization
	captured := f.authorize(t, acc.Id, "net-p7b", 100).Authorization
	toReverse := f.authorize(t, acc.Id, "net-p7c", 100).Authorization
	_, _ = f.capture(captured.Id, "cap-p7b", 100)
	if _, err := f.pool.Exec(context.Background(), `UPDATE accounts SET state='closed' WHERE id=$1`, acc.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.capture(toCapture.Id, "cap-p7a", 100); grpcx.Reason(err) != "account_closed" {
		t.Fatalf("capture on closed: %v", err)
	}
	if _, err := f.refund(captured.Id, "rf-p7b", 50); err != nil {
		t.Fatalf("refund on closed must be accepted: %v", err)
	}
	if _, err := f.reverse(toReverse.Id, "rev-p7c"); err != nil {
		t.Fatalf("reversal on closed must be accepted: %v", err)
	}
}

func TestFrozenCardStillCapturesExistingAuthorization(t *testing.T) {
	f, _ := txFixture(t)
	acc := f.activeAccount(t, "cust-p8", 1000)
	a := f.authorize(t, acc.Id, "net-p8", 100).Authorization
	_, _ = f.svc.SetFrozen(context.Background(), &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true})
	if _, err := f.capture(a.Id, "cap-p8", 100); err != nil {
		t.Fatalf("frozen blocks only new authorizations: %v", err)
	}
}
