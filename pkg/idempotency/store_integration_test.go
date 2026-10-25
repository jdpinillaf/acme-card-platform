//go:build integration

package idempotency

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
)

func newStore(t *testing.T) *Store {
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "gateway", os.DirFS("../../services/gateway/migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), dsn, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool, 24*time.Hour)
}

func TestAcquireThenReplay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "k1"), RequestHash("POST", "/x", []byte("a"))

	claim, replay, err := s.Acquire(ctx, kh, rh)
	if err != nil || claim == nil || replay != nil {
		t.Fatalf("first acquire: claim=%v replay=%v err=%v", claim, replay, err)
	}
	if err := claim.Complete(ctx, Result{Status: 201, Body: []byte(`{"id":1}`)}); err != nil {
		t.Fatal(err)
	}

	claim2, replay2, err := s.Acquire(ctx, kh, rh)
	if err != nil || claim2 != nil || replay2 == nil {
		t.Fatalf("second acquire: claim=%v replay=%v err=%v", claim2, replay2, err)
	}
	if replay2.Status != 201 || string(replay2.Body) != `{"id":1}` || replay2.Outcome != OutcomeSuccess {
		t.Fatalf("replay = %+v", replay2)
	}
}

func TestAcquireMismatchedBodyIsRejected(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	kh := KeyHash("sub", "k1")
	claim, _, err := s.Acquire(ctx, kh, RequestHash("POST", "/x", []byte("a")))
	if err != nil {
		t.Fatal(err)
	}
	_ = claim.Complete(ctx, Result{Status: 200})
	_, _, err = s.Acquire(ctx, kh, RequestHash("POST", "/x", []byte("DIFFERENT")))
	if !errors.Is(err, ErrKeyReuseMismatch) {
		t.Fatalf("want ErrKeyReuseMismatch, got %v", err)
	}
}

func TestAbortReleasesKeyForRetry(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "k1"), RequestHash("POST", "/x", nil)
	claim, _, err := s.Acquire(ctx, kh, rh)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	claim2, replay, err := s.Acquire(ctx, kh, rh)
	if err != nil || claim2 == nil || replay != nil {
		t.Fatalf("after abort a fresh claim is expected: %v %v %v", claim2, replay, err)
	}
	_ = claim2.Complete(ctx, Result{Status: 500, Outcome: OutcomeRolledBack})
	_, replay, _ = s.Acquire(ctx, kh, rh)
	if replay == nil || replay.Outcome != OutcomeRolledBack || replay.Status != 500 {
		t.Fatalf("rolled_back outcome must be stored and replayed: %+v", replay)
	}
}

func TestConcurrentAcquireSameKeyYieldsExactlyOneClaim(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "k-concurrent"), RequestHash("POST", "/x", []byte("same"))

	const n = 50
	var claims, replays atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim, replay, err := s.Acquire(ctx, kh, rh)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if claim != nil {
				claims.Add(1)
				time.Sleep(200 * time.Millisecond) // simulate work while others block
				if err := claim.Complete(ctx, Result{Status: 201, Body: []byte("done")}); err != nil {
					t.Errorf("complete: %v", err)
				}
				return
			}
			if string(replay.Body) != "done" || replay.Status != 201 {
				t.Errorf("replay mismatch: %+v", replay)
			}
			replays.Add(1)
		}()
	}
	close(start)
	wg.Wait()
	if claims.Load() != 1 || replays.Load() != n-1 {
		t.Fatalf("claims=%d replays=%d, want 1/%d", claims.Load(), replays.Load(), n-1)
	}
}

func TestSweepDeletesExpiredOnly(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	fresh := KeyHash("sub", "fresh")
	c, _, _ := s.Acquire(ctx, fresh, RequestHash("POST", "/x", nil))
	_ = c.Complete(ctx, Result{Status: 200})

	old := NewStore(s.pool, -time.Hour) // already expired on insert
	c2, _, _ := old.Acquire(ctx, KeyHash("sub", "old"), RequestHash("POST", "/x", nil))
	_ = c2.Complete(ctx, Result{Status: 200})

	deleted, err := s.Sweep(ctx, time.Now())
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v, want 1", deleted, err)
	}
	if _, replay, _ := s.Acquire(ctx, fresh, RequestHash("POST", "/x", nil)); replay == nil {
		t.Fatal("fresh key must survive sweep")
	}
}

func TestDuplicateWaiterGivesUpAfterWaitTimeout(t *testing.T) {
	s := newStore(t).WithWaitTimeout(200 * time.Millisecond)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "slow"), RequestHash("POST", "/x", nil)
	claim, _, err := s.Acquire(ctx, kh, rh)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Abort(ctx)

	start := time.Now()
	_, _, err = s.Acquire(ctx, kh, rh)
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("want ErrInProgress, got %v", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("waiter held for %v; must give up near the wait timeout", waited)
	}
}
