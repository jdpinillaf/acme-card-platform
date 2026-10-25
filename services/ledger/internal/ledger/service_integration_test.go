//go:build integration

package ledger

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/ledger/migrations"
)

func newService(t *testing.T) *Service {
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "ledger", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), dsn, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewService(pool)
}

func open(t *testing.T, s *Service) string {
	id := uuid.NewString()
	if _, err := s.OpenLedger(context.Background(), &ledgerv1.OpenLedgerRequest{AccountId: id, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func settle(acc, ref string, amt int64) *ledgerv1.PostJournalRequest {
	return &ledgerv1.PostJournalRequest{AccountId: acc, Reference: ref, EntryType: "deposit_settled",
		Postings: []*ledgerv1.Posting{p(cash, dr, amt), p(coll, cr, amt)}}
}

func balances(t *testing.T, s *Service, acc string) map[ledgerv1.SubAccount]int64 {
	res, err := s.GetBalances(context.Background(), &ledgerv1.GetBalancesRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	m := map[ledgerv1.SubAccount]int64{}
	for _, b := range res.Balances {
		m[b.SubAccount] = b.AmountMinor
	}
	return m
}

func assertLedgerBalanced(t *testing.T, s *Service) {
	t.Helper()
	var unbalanced int
	err := s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM (
		  SELECT journal_entry_id FROM postings GROUP BY journal_entry_id
		  HAVING sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) <> 0) x`).Scan(&unbalanced)
	if err != nil || unbalanced != 0 {
		t.Fatalf("unbalanced entries=%d err=%v", unbalanced, err)
	}
	var drift int
	err = s.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM balances b
		LEFT JOIN (SELECT account_id, sub_account, sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) s
		           FROM postings GROUP BY 1,2) p USING (account_id, sub_account)
		WHERE b.raw_balance_minor <> coalesce(p.s, 0)`).Scan(&drift)
	if err != nil || drift != 0 {
		t.Fatalf("snapshot drift rows=%d err=%v", drift, err)
	}
}

func TestOpenLedgerIsIdempotentAndRejectsCurrencyChange(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	id := open(t, s)
	if _, err := s.OpenLedger(ctx, &ledgerv1.OpenLedgerRequest{AccountId: id, Currency: "USD"}); err != nil {
		t.Fatalf("reopen same currency must be ok: %v", err)
	}
	_, err := s.OpenLedger(ctx, &ledgerv1.OpenLedgerRequest{AccountId: id, Currency: "EUR"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("currency change: want FailedPrecondition got %v", err)
	}
}

func TestPostJournalUpdatesBalancesAndReplays(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := open(t, s)

	r1, err := s.PostJournal(ctx, settle(acc, "dep-1", 10_000))
	if err != nil || r1.Replayed {
		t.Fatalf("first post: %v %+v", err, r1)
	}
	r2, err := s.PostJournal(ctx, settle(acc, "dep-1", 10_000))
	if err != nil || !r2.Replayed || r2.JournalEntryId != r1.JournalEntryId {
		t.Fatalf("replay: %v %+v", err, r2)
	}
	_, err = s.PostJournal(ctx, settle(acc, "dep-1", 999))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("same reference, different postings: want AlreadyExists, got %v", err)
	}
	b := balances(t, s, acc)
	if b[coll] != 10_000 || b[cash] != 10_000 {
		t.Fatalf("balances %+v", b)
	}
	assertLedgerBalanced(t, s)
}

func TestPostJournalRejectsUnknownAccountAndInvalid(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	if _, err := s.PostJournal(ctx, settle(uuid.NewString(), "x", 1)); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown account: %v", err)
	}
	acc := open(t, s)
	bad := settle(acc, "y", 1)
	bad.Postings[1].AmountMinor = 2
	if _, err := s.PostJournal(ctx, bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unbalanced: %v", err)
	}
	if _, err := s.PostJournal(ctx, settle(acc, "", 1)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing reference: %v", err)
	}
}

func TestPostingsAreAppendOnly(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := open(t, s)
	if _, err := s.PostJournal(ctx, settle(acc, "d", 5)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE postings SET amount_minor = 1`, `DELETE FROM postings`, `UPDATE journal_entries SET entry_type='x'`, `DELETE FROM journal_entries`} {
		if _, err := s.pool.Exec(ctx, q); err == nil {
			t.Fatalf("%s must be rejected", q)
		}
	}
}

func TestConcurrentSameReferencePostsOnce(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := open(t, s)
	const n = 50
	var fresh, replayed atomic.Int32
	ids := sync.Map{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := s.PostJournal(ctx, settle(acc, "dep-race", 2_500))
			if err != nil {
				t.Error(err)
				return
			}
			ids.Store(r.JournalEntryId, true)
			if r.Replayed {
				replayed.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	distinct := 0
	ids.Range(func(any, any) bool { distinct++; return true })
	if fresh.Load() != 1 || replayed.Load() != n-1 || distinct != 1 {
		t.Fatalf("fresh=%d replayed=%d distinct=%d", fresh.Load(), replayed.Load(), distinct)
	}
	if b := balances(t, s, acc); b[coll] != 2_500 {
		t.Fatalf("collateral=%d want 2500", b[coll])
	}
	assertLedgerBalanced(t, s)
}

func TestConcurrentDistinctReferencesAllApply(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := open(t, s)
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.PostJournal(ctx, settle(acc, fmt.Sprintf("dep-%d", i), 100)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if b := balances(t, s, acc); b[coll] != n*100 {
		t.Fatalf("collateral=%d want %d", b[coll], n*100)
	}
	assertLedgerBalanced(t, s)
}

func TestPostJournalEnqueuesOutboxEvent(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	acc := open(t, s)
	_, _ = s.PostJournal(ctx, settle(acc, "dep-ev", 7))
	_, _ = s.PostJournal(ctx, settle(acc, "dep-ev", 7)) // replay must not emit again
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE payload->>'type'='ledger.journal_posted'`).Scan(&n)
	if n != 1 {
		t.Fatalf("journal_posted events=%d want 1", n)
	}
}
