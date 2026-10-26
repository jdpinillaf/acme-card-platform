//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/notifications/migrations"
)

func TestMigrationsApply(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "notifications", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), dsn, "notifications")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, table := range []string{"processed_events", "preferences", "subscriptions", "deliveries", "inbox", "declines", "fraud_alerts", "outbox", "outbox_sequence"} {
		var ok bool
		if err := pool.QueryRow(context.Background(), `SELECT to_regclass('notifications.'||$1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}
