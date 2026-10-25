//go:build integration

package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/testutil"
)

func TestRecordIsIdempotentAndTableIsAppendOnly(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(dsn, "audit", os.DirFS("../../migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, dsn, "audit")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := New(pool)

	ev := events.New("card.activated", "account", "acc-1")
	ev.HTTPStatus = 200
	ins, err := s.Record(ctx, ev)
	if err != nil || !ins {
		t.Fatalf("first record: inserted=%v err=%v", ins, err)
	}
	ins, err = s.Record(ctx, ev)
	if err != nil || ins {
		t.Fatalf("duplicate must be ignored: inserted=%v err=%v", ins, err)
	}

	for _, stmt := range []string{
		`UPDATE audit_events SET actor = 'x' WHERE id = $1`,
		`DELETE FROM audit_events WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, stmt, ev.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s should be blocked, got %v", stmt, err)
		}
	}
	if _, err := pool.Exec(ctx, `TRUNCATE audit_events`); err == nil {
		t.Fatal("TRUNCATE should be blocked")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows=%d want 1", n)
	}
}
