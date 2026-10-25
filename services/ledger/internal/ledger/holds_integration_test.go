//go:build integration

package ledger

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func funded(t *testing.T, s *Service, amt int64) string {
	acc := open(t, s)
	if _, err := s.PostJournal(context.Background(), settle(acc, "dep-"+acc, amt)); err != nil {
		t.Fatal(err)
	}
	return acc
}

func hold(s *Service, acc, ref string, amt int64) (*ledgerv1.PlaceHoldResponse, error) {
	return s.PlaceHold(context.Background(), &ledgerv1.PlaceHoldRequest{AccountId: acc, Reference: ref, AmountMinor: amt, Currency: "USD"})
}

func avail(t *testing.T, s *Service, acc string) (available, holds int64) {
	r, err := s.GetBalances(context.Background(), &ledgerv1.GetBalancesRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r.AvailableMinor, r.HoldsMinor
}

func TestPlaceHoldStrictLimit(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	r, err := hold(s, acc, "h1", 600)
	if err != nil || !r.Approved || r.AvailableMinor != 400 {
		t.Fatalf("first hold: %v %+v", err, r)
	}
	r, err = hold(s, acc, "h2", 500)
	if err != nil || r.Approved {
		t.Fatalf("over-limit hold must be approved=false: %v %+v", err, r)
	}
	if a, h := avail(t, s, acc); a != 400 || h != 600 {
		t.Fatalf("available=%d holds=%d", a, h)
	}
	if _, err := s.PlaceHold(context.Background(), &ledgerv1.PlaceHoldRequest{AccountId: acc, Reference: "h3", AmountMinor: 1, Currency: "EUR"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("wrong currency: %v", err)
	}
	if _, err := hold(s, acc, "h4", 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("zero amount: %v", err)
	}
}

func TestConcurrentHoldsNeverExceedLimit(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	var approved atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := hold(s, acc, "c-"+string(rune('a'+i)), 250)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Approved {
				approved.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if approved.Load() != 4 {
		t.Fatalf("approved=%d want 4", approved.Load())
	}
	if a, h := avail(t, s, acc); a != 0 || h != 1000 {
		t.Fatalf("available=%d holds=%d", a, h)
	}
}

func TestPlaceHoldReplayAndMismatch(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	r1, _ := hold(s, acc, "same", 100)
	r2, err := hold(s, acc, "same", 100)
	if err != nil || !r2.Replayed || r2.HoldId != r1.HoldId {
		t.Fatalf("replay: %v %+v", err, r2)
	}
	if _, err := hold(s, acc, "same", 101); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("mismatch: %v", err)
	}
	if _, h := avail(t, s, acc); h != 100 {
		t.Fatalf("holds=%d want 100", h)
	}
}

func capture(s *Service, ref, journal string, amt int64) (*ledgerv1.PostJournalResponse, error) {
	return s.CaptureHold(context.Background(), &ledgerv1.CaptureHoldRequest{HoldReference: ref, JournalReference: journal, AmountMinor: amt})
}

func release(s *Service, ref string, why ledgerv1.HoldRelease) (*ledgerv1.ReleaseHoldResponse, error) {
	return s.ReleaseHold(context.Background(), &ledgerv1.ReleaseHoldRequest{HoldReference: ref, Reason: why})
}

func TestCaptureHoldPostsReceivableAndReleasesRemainder(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "h", 300)
	if _, err := capture(s, "h", "cap-1", 400); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("capture above hold: %v", err)
	}
	if _, err := capture(s, "h", "cap-1", 200); err != nil {
		t.Fatal(err)
	}
	r, err := capture(s, "h", "cap-1", 200)
	if err != nil || !r.Replayed {
		t.Fatalf("capture replay: %v %+v", err, r)
	}
	if _, err := capture(s, "h", "cap-2", 200); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second capture: %v", err)
	}
	b := balances(t, s, acc)
	if b[recv] != 200 {
		t.Fatalf("receivable=%d", b[recv])
	}
	if a, h := avail(t, s, acc); a != 800 || h != 0 {
		t.Fatalf("available=%d holds=%d", a, h)
	}
	assertLedgerBalanced(t, s)
}

func TestCaptureVsReleaseExactlyOneWins(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100_000)
	for i := 0; i < 25; i++ {
		ref := "race-" + string(rune('A'+i))
		if r, err := hold(s, acc, ref, 100); err != nil || !r.Approved {
			t.Fatal(err)
		}
		var ok atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := capture(s, ref, "j-"+ref, 100); err == nil {
				ok.Add(1)
			} else if status.Code(err) != codes.FailedPrecondition {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := release(s, ref, ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED); err == nil {
				ok.Add(1)
			} else if status.Code(err) != codes.FailedPrecondition {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		if ok.Load() != 1 {
			t.Fatalf("round %d: %d winners", i, ok.Load())
		}
	}
	if _, h := avail(t, s, acc); h != 0 {
		t.Fatalf("holds left=%d", h)
	}
	assertLedgerBalanced(t, s)
}

func TestReleaseHoldReplayAndConflict(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "r", 100)
	if _, err := release(s, "r", ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED); err != nil {
		t.Fatal(err)
	}
	if r, err := release(s, "r", ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED); err != nil || !r.Replayed {
		t.Fatalf("replay: %v %+v", err, r)
	}
	if _, err := release(s, "r", ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("different terminal: %v", err)
	}
	if _, err := release(s, "missing", ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED); status.Code(err) != codes.NotFound {
		t.Fatalf("missing: %v", err)
	}
}

func refundFixture(t *testing.T, s *Service, acc, ref string, amt int64) {
	t.Helper()
	if _, err := s.PostRefund(context.Background(), &ledgerv1.PostRefundRequest{AccountId: acc, Reference: ref, AmountMinor: amt}); err != nil {
		t.Fatal(err)
	}
}

// Sprint 3 (ADR-017) supersedes the Sprint 2 strict-limit rule: a credit balance is spendable.
func TestCreditBalanceIsSpendable(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "h", 200)
	_, _ = capture(s, "h", "cap", 200)
	refundFixture(t, s, acc, "rf-1", 200)
	refundFixture(t, s, acc, "rf-2", 700) // ledger-level fixture: credit balance 700
	if b := balances(t, s, acc); b[recv] != -700 {
		t.Fatalf("receivable=%d want -700", b[recv])
	}
	if a, _ := avail(t, s, acc); a != 1700 {
		t.Fatalf("available=%d want 1700 (credit balance is spendable)", a)
	}
	if r, _ := hold(s, acc, "big", 1700); !r.Approved {
		t.Fatal("hold of 1700 must approve")
	}
	if r, _ := hold(s, acc, "one-more", 1); r.Approved {
		t.Fatal("exposure would exceed the limit")
	}
	_, _ = release(s, "big", ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED)
	_, _ = hold(s, acc, "buy", 500)
	if _, err := capture(s, "buy", "cap-buy", 500); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s, acc); b[recv] != -200 {
		t.Fatalf("receivable=%d want -200 (500 paid from the credit balance, nothing owed)", b[recv])
	}
	assertLedgerBalanced(t, s)
}

func TestHoldExpiryWithCreditBalance(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	refundFixture(t, s, acc, "rf", 300)
	_, _ = hold(s, acc, "h", 500)
	if a, _ := avail(t, s, acc); a != 800 {
		t.Fatalf("available=%d want 800", a)
	}
	_, _ = release(s, "h", ledgerv1.HoldRelease_HOLD_RELEASE_EXPIRED)
	if a, _ := avail(t, s, acc); a != 1300 {
		t.Fatalf("available=%d want 1300", a)
	}
}

func TestHoldOperationsAreEvented(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "e1", 100)
	_, _ = hold(s, acc, "e1", 100)
	_, _ = capture(s, "e1", "ej", 100)
	_, _ = capture(s, "e1", "ej", 100)
	_, _ = hold(s, acc, "e2", 100)
	_, _ = release(s, "e2", ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED)
	_, _ = release(s, "e2", ledgerv1.HoldRelease_HOLD_RELEASE_RELEASED)
	_, _ = s.PostRefund(context.Background(), &ledgerv1.PostRefundRequest{AccountId: acc, Reference: "er", AmountMinor: 50})
	_, _ = s.PostRefund(context.Background(), &ledgerv1.PostRefundRequest{AccountId: acc, Reference: "er", AmountMinor: 50})
	for typ, want := range map[string]int{"ledger.hold_placed": 2, "ledger.hold_captured": 1, "ledger.hold_released": 1, "ledger.refund_posted": 1} {
		var n int
		_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE payload->>'type'=$1`, typ).Scan(&n)
		if n != want {
			t.Errorf("%s=%d want %d", typ, n, want)
		}
	}
}

func TestCaptureReplayRequiresSameAmount(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "ha", 300)
	if _, err := capture(s, "ha", "cap-a", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(s, "ha", "cap-a", 300); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("replay with different amount must be AlreadyExists, got %v", err)
	}
}

func TestExpireStaleHoldsReleasesOrphans(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := funded(t, s, 1000)
	_, _ = hold(s, acc, "old", 300)
	_, _ = hold(s, acc, "new", 200)
	if _, err := s.pool.Exec(ctx, `UPDATE holds SET created_at = now() - interval '9 days' WHERE reference='old'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.ExpireStaleHolds(ctx, 8*24*time.Hour, 100)
	if err != nil || n != 1 {
		t.Fatalf("expired=%d err=%v want 1", n, err)
	}
	if a, h := avail(t, s, acc); a != 800 || h != 200 {
		t.Fatalf("available=%d holds=%d", a, h)
	}
}
