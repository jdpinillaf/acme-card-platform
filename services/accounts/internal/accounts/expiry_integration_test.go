//go:build integration

package accounts

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
)

func TestExpireDueReleasesHolds(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-e1", 1000)
	f.authorize(t, acc.Id, "net-e1", 100)
	f.authorize(t, acc.Id, "net-e2", 200)
	if n, err := f.svc.ExpireDue(ctx, 100); err != nil || n != 0 {
		t.Fatalf("nothing due yet: %d %v", n, err)
	}
	c.Advance(HoldTTL + time.Second)
	n, err := f.svc.ExpireDue(ctx, 100)
	if err != nil || n != 2 {
		t.Fatalf("expired=%d err=%v want 2", n, err)
	}
	if av, _ := f.balances(t, acc.Id); av != 1000 {
		t.Fatalf("available=%d", av)
	}
	if n, _ := f.svc.ExpireDue(ctx, 100); n != 0 {
		t.Fatalf("second run expired %d", n)
	}
	if f.events(t, "authorization.expired") != 2 {
		t.Fatal("expired events")
	}
}

func TestExpiryVsCaptureRace(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-e2", 100_000)
	var ids []*accountsv1.Authorization
	for i := 0; i < 10; i++ {
		ids = append(ids, f.authorize(t, acc.Id, fmt.Sprintf("net-er-%d", i), 100).Authorization)
	}
	c.Advance(HoldTTL + time.Second)
	var wg sync.WaitGroup
	for _, a := range ids {
		wg.Add(1)
		go func(a *accountsv1.Authorization) { defer wg.Done(); _, _ = f.capture(a.Id, "cap-"+a.Id, 100) }(a)
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = f.svc.ExpireDue(ctx, 100) }()
	wg.Wait()
	_, _ = f.svc.ExpireDue(ctx, 100)
	if n := f.count(t, f.pool, `SELECT count(*) FROM authorizations WHERE account_id=$1 AND status='expired'`, acc.Id); n != 10 {
		t.Fatalf("expired=%d want 10", n)
	}
	if n := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='purchase_captured'`, acc.Id); n != 0 {
		t.Fatalf("captures posted after expiry: %d", n)
	}
}

func TestListTransactionsPaginates(t *testing.T) {
	f, c := txFixture(t)
	ctx := context.Background()
	acc := f.activeAccount(t, "cust-e3", 100_000)
	f.svc = NewService(f.pool, f.ledgerClient, WithClock(c), WithVelocity(1000, time.Minute))
	for i := 0; i < 25; i++ {
		f.authorize(t, acc.Id, fmt.Sprintf("net-l-%02d", i), 10)
		c.Advance(time.Second)
	}
	p1, err := f.svc.ListTransactions(ctx, &accountsv1.ListTransactionsRequest{AccountId: acc.Id})
	if err != nil || len(p1.Authorizations) != 20 || p1.NextPageToken == "" {
		t.Fatalf("page1: %v len=%d token=%q", err, len(p1.Authorizations), p1.NextPageToken)
	}
	if p1.Authorizations[0].NetworkRef != "net-l-24" {
		t.Fatalf("newest first: %s", p1.Authorizations[0].NetworkRef)
	}
	p2, err := f.svc.ListTransactions(ctx, &accountsv1.ListTransactionsRequest{AccountId: acc.Id, PageToken: p1.NextPageToken})
	if err != nil || len(p2.Authorizations) != 5 || p2.NextPageToken != "" {
		t.Fatalf("page2: %v len=%d token=%q", err, len(p2.Authorizations), p2.NextPageToken)
	}
	seen := map[string]bool{}
	for _, a := range append(p1.Authorizations, p2.Authorizations...) {
		if seen[a.Id] {
			t.Fatalf("duplicate %s", a.Id)
		}
		seen[a.Id] = true
	}
	if p, _ := f.svc.ListTransactions(ctx, &accountsv1.ListTransactionsRequest{AccountId: acc.Id, PageSize: 500}); len(p.Authorizations) != 25 {
		t.Fatalf("page size capped at 100, got %d", len(p.Authorizations))
	}
	if _, err := f.svc.ListTransactions(ctx, &accountsv1.ListTransactionsRequest{AccountId: acc.Id, PageToken: "garbage"}); err == nil {
		t.Fatal("bad token must fail")
	}
}
