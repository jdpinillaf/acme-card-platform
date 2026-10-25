//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/testutil"
)

func TestSNSPublishIsConsumedFromSQSIntoAudit(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	aws := testutil.StartLocalStack(t)
	ctx := context.Background()
	if err := db.Migrate(dsn, "audit", os.DirFS("../../migrations"), "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, dsn, "audit")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	pub := events.NewSNSPublisher(aws.SNS, aws.TopicARN)
	ev := events.New("collateral.confirmed", "deposit", "dep-1")
	ev.Actor = "bank"
	if err := pub.Publish(ctx, []events.Event{ev}); err != nil {
		t.Fatal(err)
	}

	c := NewConsumer(aws.SQS, aws.QueueURL, New(pool), nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		n, err := c.PollOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	var typ, actor string
	if err := pool.QueryRow(ctx, `SELECT event_type, actor FROM audit_events WHERE id=$1`, ev.ID).Scan(&typ, &actor); err != nil {
		t.Fatalf("event not recorded: %v", err)
	}
	if typ != "collateral.confirmed" || actor != "bank" {
		t.Fatalf("%s %s", typ, actor)
	}
	// Message must have been deleted from the queue.
	if n, _ := c.PollOnce(ctx); n != 0 {
		t.Fatalf("queue should be empty, got %d", n)
	}
}
