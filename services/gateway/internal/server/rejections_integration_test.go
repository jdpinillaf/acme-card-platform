//go:build integration

package server

import (
	"context"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/gateway/migrations"
)

func TestRejectionsCollapsePerSourceAndWindow(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "gateway", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), dsn, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := NewRejectionLog(pool, 5*time.Minute)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 25; i++ {
		if err := r.Record(ctx, "10.0.0.1", "bad_signature", t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Record(ctx, "10.0.0.2", "stale_timestamp", t0)
	_ = r.Record(ctx, "10.0.0.1", "bad_signature", t0.Add(6*time.Minute)) // next window

	var rows, hits int
	_ = pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(attempts),0) FROM webhook_rejections`).Scan(&rows, &hits)
	if rows != 3 || hits != 27 {
		t.Fatalf("rows=%d hits=%d, want 3 rows / 27 attempts", rows, hits)
	}
}
