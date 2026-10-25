//go:build integration

package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
)

func newLimiter(t *testing.T, limit int) *Limiter {
	dsn := testutil.StartPostgres(t)
	pool, err := db.Connect(context.Background(), dsn, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), Migration); err != nil {
		t.Fatal(err)
	}
	return New(pool, limit, time.Minute)
}

func TestAllowsUpToLimitThenBlocksPerKey(t *testing.T) {
	l := newLimiter(t, 3)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if ok, _, err := l.Allow(ctx, "cust-a", now); err != nil || !ok {
			t.Fatalf("hit %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, retry, err := l.Allow(ctx, "cust-a", now)
	if err != nil || ok || retry <= 0 {
		t.Fatalf("4th hit must be blocked with retry-after: ok=%v retry=%v err=%v", ok, retry, err)
	}
	if ok, _, _ := l.Allow(ctx, "cust-b", now); !ok {
		t.Fatal("other key unaffected")
	}
	// Two full windows later the budget is restored.
	if ok, _, _ := l.Allow(ctx, "cust-a", now.Add(2*time.Minute)); !ok {
		t.Fatal("budget must recover")
	}
}

func TestSlidingWindowWeighsPreviousWindow(t *testing.T) {
	l := newLimiter(t, 4)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		_, _, _ = l.Allow(ctx, "k", start.Add(50*time.Second))
	}
	// 15s into the next window, 75% of the previous 4 hits still count (=3), so only 1 more fits.
	at := start.Add(75 * time.Second)
	if ok, _, _ := l.Allow(ctx, "k", at); !ok {
		t.Fatal("first hit in new window should pass")
	}
	if ok, _, _ := l.Allow(ctx, "k", at); ok {
		t.Fatal("second hit should exceed weighted budget")
	}
}

func TestConcurrentHitsNeverExceedLimit(t *testing.T) {
	l := newLimiter(t, 10)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _, err := l.Allow(ctx, "burst", now); err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("allowed=%d want 10", allowed.Load())
	}
}
