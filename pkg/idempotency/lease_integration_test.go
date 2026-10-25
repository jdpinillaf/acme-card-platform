//go:build integration

package idempotency

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
)

// newStoreWithPool is newStore with an explicit pool size.
func newStoreWithPool(t *testing.T, maxConns int32) *Store {
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "gateway", os.DirFS("../../services/gateway/migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.ConnectN(context.Background(), dsn, "gateway", maxConns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool, 24*time.Hour)
}

// A held claim must not pin a pool connection: with a pool of one connection, unrelated work
// (rate limit, audit, other requests) must still get through while a request is in flight.
func TestClaimDoesNotPinAPoolConnection(t *testing.T) {
	s := newStoreWithPool(t, 1)
	ctx := context.Background()
	claim, _, err := s.Acquire(ctx, KeyHash("sub", "k-pin"), RequestHash("POST", "/x", nil))
	if err != nil || claim == nil {
		t.Fatalf("acquire: %v", err)
	}
	defer claim.Abort(ctx)

	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var one int
	if err := s.pool.QueryRow(qctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("pool starved while a claim is held: %v", err)
	}
}

// A claim whose owner died is taken over once its lease expires, without waiting for the TTL.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	s := newStore(t).WithLease(200 * time.Millisecond).WithWaitTimeout(100 * time.Millisecond)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "k-lease"), RequestHash("POST", "/x", nil)
	if claim, _, err := s.Acquire(ctx, kh, rh); err != nil || claim == nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Owner never completes nor aborts (crash). A duplicate within the lease gets in-progress.
	if _, _, err := s.Acquire(ctx, kh, rh); err != ErrInProgress {
		t.Fatalf("within lease: want ErrInProgress, got %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	claim2, replay, err := s.Acquire(ctx, kh, rh)
	if err != nil || claim2 == nil || replay != nil {
		t.Fatalf("after lease expiry a fresh claim is expected: %v %v %v", claim2, replay, err)
	}
	if err := claim2.Complete(ctx, Result{Status: 201, Body: []byte("ok")}); err != nil {
		t.Fatal(err)
	}
	if _, replay, _ := s.Acquire(ctx, kh, rh); replay == nil || replay.Status != 201 {
		t.Fatalf("takeover result must be replayable: %+v", replay)
	}
}

// Complete by a stale owner (lease expired and taken over) must not overwrite the new owner's row.
func TestStaleOwnerCannotCompleteAfterTakeover(t *testing.T) {
	s := newStore(t).WithLease(200 * time.Millisecond).WithWaitTimeout(50 * time.Millisecond)
	ctx := context.Background()
	kh, rh := KeyHash("sub", "k-stale"), RequestHash("POST", "/x", nil)
	stale, _, err := s.Acquire(ctx, kh, rh)
	if err != nil || stale == nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	fresh, _, err := s.Acquire(ctx, kh, rh)
	if err != nil || fresh == nil {
		t.Fatalf("takeover: %v", err)
	}
	if err := stale.Complete(ctx, Result{Status: 500, Body: []byte("stale")}); err == nil {
		t.Fatal("stale owner must not be able to complete")
	}
	if err := fresh.Complete(ctx, Result{Status: 201, Body: []byte("fresh")}); err != nil {
		t.Fatal(err)
	}
	_, replay, _ := s.Acquire(ctx, kh, rh)
	if replay == nil || string(replay.Body) != "fresh" {
		t.Fatalf("replay = %+v, want the fresh owner's result", replay)
	}
}
