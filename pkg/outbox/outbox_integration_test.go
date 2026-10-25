//go:build integration

package outbox

import (
	"context"
	"os"
	"testing"

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
	relay := NewRelay(pool, pub, 100)

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
