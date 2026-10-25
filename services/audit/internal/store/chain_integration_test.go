//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/testutil"
)

func chainPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "audit", os.DirFS("../../migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.ConnectN(context.Background(), dsn, "audit", 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// recomputeChain walks the stored chain and returns the first chain_seq whose hash does not
// match, or 0 when the chain is intact and contiguous.
func recomputeChain(t *testing.T, s *Store) (int64, int64) {
	t.Helper()
	bad, n, err := s.VerifyRange(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	return bad, n
}

func TestChainContiguousUnderConcurrency(t *testing.T) {
	pool := chainPool(t)
	s := New(pool)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				ev := events.New("x.happened", "x", fmt.Sprint(i))
				ev.Source, ev.SourceSeq = fmt.Sprintf("src%d", g), int64(i+1)
				if _, err := s.Record(ctx, ev); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	var min, max, count int64
	if err := pool.QueryRow(ctx, `SELECT min(chain_seq), max(chain_seq), count(*) FROM audit_events`).Scan(&min, &max, &count); err != nil {
		t.Fatal(err)
	}
	if min != 1 || max != 1000 || count != 1000 {
		t.Fatalf("chain_seq %d..%d count %d", min, max, count)
	}
	if bad, n := recomputeChain(t, s); bad != 0 || n != 1000 {
		t.Fatalf("chain broken at %d (checked %d)", bad, n)
	}
}

func TestDuplicateDeliveryDoesNotFork(t *testing.T) {
	pool := chainPool(t)
	s := New(pool)
	ctx := context.Background()
	ev := events.New("x.happened", "x", "1")
	if ins, err := s.Record(ctx, ev); err != nil || !ins {
		t.Fatalf("first: %v %v", ins, err)
	}
	if ins, err := s.Record(ctx, ev); err != nil || ins {
		t.Fatalf("redelivery: %v %v", ins, err)
	}
	var head int64
	if err := pool.QueryRow(ctx, `SELECT chain_seq FROM chain_head`).Scan(&head); err != nil || head != 1 {
		t.Fatalf("head=%d %v", head, err)
	}
}

func TestRecordRedactsBeforeHashing(t *testing.T) {
	pool := chainPool(t)
	s := New(pool)
	ctx := context.Background()
	ev := events.New("x.happened", "x", "1")
	ev.After = []byte(`{"destination":{"account_number":"000123456789"}}`)
	if _, err := s.Record(ctx, ev); err != nil {
		t.Fatal(err)
	}
	var after string
	var redacted bool
	if err := pool.QueryRow(ctx, `SELECT after::text, redacted FROM audit_events WHERE id=$1`, ev.ID).Scan(&after, &redacted); err != nil {
		t.Fatal(err)
	}
	if !redacted || after != `{"destination": {"account_number": "[redacted]"}}` {
		t.Fatalf("stored %s redacted=%v", after, redacted)
	}
	if bad, _ := recomputeChain(t, s); bad != 0 {
		t.Fatalf("chain must verify over the redacted value, broken at %d", bad)
	}
}

func TestBackfillChainsExistingRows(t *testing.T) {
	pool := chainPool(t)
	s := New(pool)
	ctx := context.Background()
	// Rows written before chaining existed (no chain columns).
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, event_type, aggregate_type, aggregate_id, occurred_at, recorded_at)
			VALUES (gen_random_uuid(), 'old.event', 'x', $1, now(), now() - make_interval(mins => 10 - $2::int))`, fmt.Sprint(i), i); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.BackfillChain(ctx)
	if err != nil || n != 5 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	if _, err := s.Record(ctx, events.New("new.event", "x", "n")); err != nil {
		t.Fatal(err)
	}
	if bad, checked := recomputeChain(t, s); bad != 0 || checked != 6 {
		t.Fatalf("after backfill: bad=%d checked=%d", bad, checked)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET actor='x'`); err == nil {
		t.Fatal("ordinary UPDATE must still be refused")
	}
}
