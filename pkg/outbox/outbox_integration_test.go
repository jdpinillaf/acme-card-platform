//go:build integration

package outbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/testutil"
)

type fakePublisher struct{ got []events.Event }

func (f *fakePublisher) Publish(_ context.Context, evs []events.Event) error {
	f.got = append(f.got, evs...)
	return nil
}

func TestEnqueueFollowsTransactionAndRelayPublishesOnce(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(dsn, "accounts", os.DirFS("../../services/accounts/migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, dsn, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pub := &fakePublisher{}
	relay := NewRelay(pool, pub, 100, "accounts")

	// Rolled-back transaction leaves nothing behind.
	tx, _ := pool.Begin(ctx)
	if err := Enqueue(ctx, tx, events.New("account.opened", "account", "a-rollback")); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if n, err := relay.RelayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("after rollback: n=%d err=%v", n, err)
	}

	// Committed transaction is relayed exactly once.
	tx, _ = pool.Begin(ctx)
	ev := events.New("account.opened", "account", "a-1")
	if err := Enqueue(ctx, tx, ev); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RelayOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first relay: n=%d err=%v", n, err)
	}
	if n, err := relay.RelayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second relay must be a no-op: n=%d err=%v", n, err)
	}
	if len(pub.got) != 1 || pub.got[0].ID != ev.ID || pub.got[0].AggregateID != "a-1" {
		t.Fatalf("published %+v", pub.got)
	}
}

type syncPublisher struct {
	mu   sync.Mutex
	got  []events.Event
	fail atomic.Int32 // fail the next N publishes
}

func (p *syncPublisher) Publish(_ context.Context, evs []events.Event) error {
	if p.fail.Load() > 0 {
		p.fail.Add(-1)
		return errors.New("bus down")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, evs...)
	return nil
}

func sequencePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "accounts", os.DirFS("../../services/accounts/migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), dsn, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func assertContiguous(t *testing.T, evs []events.Event, source string, n int) {
	t.Helper()
	seen := map[int64]bool{}
	for _, e := range evs {
		if e.Source != source || seen[e.SourceSeq] {
			t.Fatalf("event %s source=%q seq=%d (dup=%v)", e.ID, e.Source, e.SourceSeq, seen[e.SourceSeq])
		}
		seen[e.SourceSeq] = true
	}
	for i := int64(1); i <= int64(n); i++ {
		if !seen[i] {
			t.Fatalf("source_seq %d missing (got %d events)", i, len(evs))
		}
	}
}

func TestRelayNumbersContiguously(t *testing.T) {
	pool := sequencePool(t)
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		if err := Enqueue(ctx, pool, events.New("x.happened", "x", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	pub := &syncPublisher{}
	var wg sync.WaitGroup
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relay := NewRelay(pool, pub, 25, "accounts")
			for {
				n, err := relay.RelayOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(pub.got) != 300 {
		t.Fatalf("published %d", len(pub.got))
	}
	assertContiguous(t, pub.got, "accounts", 300)
}

func TestRelayRollbackLeavesNoGap(t *testing.T) {
	pool := sequencePool(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := Enqueue(ctx, pool, events.New("x.happened", "x", fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	pub := &syncPublisher{}
	pub.fail.Store(1)
	relay := NewRelay(pool, pub, 4, "accounts")
	if _, err := relay.RelayOnce(ctx); err == nil {
		t.Fatal("first publish must fail")
	}
	for {
		n, err := relay.RelayOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	assertContiguous(t, pub.got, "accounts", 10)
}
