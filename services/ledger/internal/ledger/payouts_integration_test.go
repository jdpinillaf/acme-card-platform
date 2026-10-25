//go:build integration

package ledger

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func payout(s *Service, acc, ref string, coll, credit int64) (*ledgerv1.PostJournalResponse, error) {
	return s.PostPayout(context.Background(), &ledgerv1.PostPayoutRequest{AccountId: acc, Reference: ref, CollateralMinor: coll, CreditMinor: credit})
}

func TestPostPayoutReleasesCollateralAndCredit(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	refundFixture(t, s, acc, "p", 700) // leaves a credit balance of 700
	if r := rawBalance(t, s, acc, "receivable"); r != -700 {
		t.Fatalf("fixture receivable=%d", r)
	}
	r1, err := payout(s, acc, "payout:a1", 100000, 700)
	if err != nil {
		t.Fatal(err)
	}
	if r := rawBalance(t, s, acc, "collateral_held"); r != 0 {
		t.Fatalf("collateral_held=%d", r)
	}
	if r := rawBalance(t, s, acc, "receivable"); r != 0 {
		t.Fatalf("receivable=%d", r)
	}
	r2, err := payout(s, acc, "payout:a1", 100000, 700)
	if err != nil || !r2.Replayed || r2.JournalEntryId != r1.JournalEntryId {
		t.Fatalf("replay: %v %+v", err, r2)
	}
	assertLedgerBalanced(t, s)
}

func TestPostPayoutRejectsMoreThanHeld(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 100000)
	for _, c := range [][2]int64{{100001, 0}, {0, 1}, {0, 0}} {
		_, err := payout(s, acc, "payout:x", c[0], c[1])
		if code := status.Code(err); code != codes.FailedPrecondition && code != codes.InvalidArgument {
			t.Fatalf("payout %v: %v", c, err)
		}
	}
}

func TestPostPayoutConcurrent50(t *testing.T) {
	s := newService(t)
	acc := funded(t, s, 5000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := payout(s, acc, "payout:c", 5000, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM journal_entries WHERE reference='payout:c'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("entries=%d %v", n, err)
	}
}
