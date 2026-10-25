//go:build integration

package audit

import (
	"context"
	"testing"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

func (f *fixture) verify(t *testing.T) *auditv1.VerifyReport {
	t.Helper()
	r, err := f.svc.Verify(context.Background(), &auditv1.VerifyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func seqs(n int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i) + 1
	}
	return out
}

func TestVerifyCleanChain(t *testing.T) {
	f := setup(t)
	f.record(t, "ledger", seqs(50)...)
	r := f.verify(t)
	if !r.ChainValid || r.Checked != 50 || len(r.Gaps) != 0 || r.FirstBadChainSeq != 0 {
		t.Fatalf("clean: %+v", r)
	}
}

func TestVerifyDetectsTamper(t *testing.T) {
	f := setup(t)
	f.record(t, "ledger", seqs(50)...)
	f.tamper(t, `UPDATE audit_events SET after='{"n":999}' WHERE chain_seq=37`)
	if r := f.verify(t); r.ChainValid || r.FirstBadChainSeq != 37 {
		t.Fatalf("tamper: %+v", r)
	}
}

func TestVerifyDetectsDeletion(t *testing.T) {
	f := setup(t)
	f.record(t, "ledger", seqs(50)...)
	f.tamper(t, `DELETE FROM audit_events WHERE chain_seq=20`)
	if r := f.verify(t); r.ChainValid || r.FirstBadChainSeq != 20 {
		t.Fatalf("deletion: %+v", r)
	}
}

func TestVerifyDetectsReorder(t *testing.T) {
	f := setup(t)
	f.record(t, "ledger", seqs(10)...)
	f.tamper(t, `UPDATE audit_events SET chain_seq = CASE chain_seq WHEN 4 THEN -5 WHEN 5 THEN 4 ELSE chain_seq END WHERE chain_seq IN (4,5)`)
	f.tamper(t, `UPDATE audit_events SET chain_seq = 5 WHERE chain_seq = -5`)
	if r := f.verify(t); r.ChainValid || r.FirstBadChainSeq != 4 {
		t.Fatalf("reorder: %+v", r)
	}
}

func TestGapDetection(t *testing.T) {
	f := setup(t)
	f.record(t, "ledger", 1, 2, 4, 5)
	r := f.verify(t)
	if len(r.Gaps) != 1 || r.Gaps[0].Source != "ledger" || r.Gaps[0].FromSeq != 3 || r.Gaps[0].ToSeq != 3 {
		t.Fatalf("gaps: %+v", r.Gaps)
	}
	f.record(t, "ledger", 3) // late delivery
	if r := f.verify(t); len(r.Gaps) != 0 || !r.ChainValid {
		t.Fatalf("after late delivery: %+v", r)
	}
	f.record(t, "billing", 2) // first event of a source missing
	if r := f.verify(t); len(r.Gaps) != 1 || r.Gaps[0].Source != "billing" || r.Gaps[0].FromSeq != 1 {
		t.Fatalf("missing first: %+v", r.Gaps)
	}
}

func TestStuckOutbox(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO accounts.outbox (event_id, payload, created_at) VALUES (gen_random_uuid(), '{}', now() - interval '10 minutes')`); err != nil {
		t.Fatal(err)
	}
	r := f.verify(t)
	if len(r.StuckOutbox) != 1 || r.StuckOutbox[0].Source != "accounts" || r.StuckOutbox[0].Count != 1 {
		t.Fatalf("stuck: %+v", r.StuckOutbox)
	}
}

func TestCheckpointSignAndLink(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "ledger", seqs(10)...)
	c1, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{})
	if err != nil || c1.ChainSeq != 10 || c1.PrevCheckpointSha256 != "" {
		t.Fatalf("checkpoint 1: %v %+v", err, c1)
	}
	f.record(t, "ledger", 11, 12)
	c2, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{})
	if err != nil || c2.ChainSeq != 12 || c2.PrevCheckpointSha256 == "" {
		t.Fatalf("checkpoint 2: %v %+v", err, c2)
	}
	if r := f.verify(t); !r.CheckpointsValid || r.Checkpoints != 2 {
		t.Fatalf("checkpoints: %+v", r)
	}
	if !f.objects.Locked(c1.S3Key) {
		t.Fatal("checkpoints must be written with an object lock retention")
	}
	// Rewriting history before checkpoint 1 (and re-hashing the chain to hide it) is caught by the anchor.
	f.tamper(t, `UPDATE audit_events SET after='{"n":0}' WHERE chain_seq=3`)
	if err := f.rehash(t); err != nil {
		t.Fatal(err)
	}
	r := f.verify(t)
	if !r.ChainValid || r.CheckpointsValid {
		t.Fatalf("a re-hashed chain must still fail the signed checkpoints: %+v", r)
	}
	// A forged checkpoint signature fails.
	body, _ := f.objects.Get(ctx, c2.S3Key)
	_ = f.objects.Overwrite(c2.S3Key+".sig", []byte("AAAA"))
	_ = body
	if r := f.verify(t); r.CheckpointsValid {
		t.Fatal("forged signature accepted")
	}
}

func TestQueryEventsFilters(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "ledger", seqs(30)...)
	r, err := f.svc.QueryEvents(ctx, &auditv1.QueryEventsRequest{Filter: &auditv1.EventFilter{AccountId: "ledger-7"}})
	if err != nil || len(r.Events) != 1 || r.Events[0].AggregateId != "ledger-7" || r.Events[0].Hash == "" {
		t.Fatalf("by account: %v %+v", err, r)
	}
	var all []*auditv1.AuditEvent
	req := &auditv1.QueryEventsRequest{Filter: &auditv1.EventFilter{EventType: "x.happened"}, PageSize: 7}
	for {
		page, err := f.svc.QueryEvents(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Events...)
		if page.NextPageToken == "" {
			break
		}
		req.PageToken = page.NextPageToken
	}
	if len(all) != 30 || all[0].ChainSeq != 1 || all[29].ChainSeq != 30 {
		t.Fatalf("paged: %d", len(all))
	}
	if r, _ := f.svc.QueryEvents(ctx, &auditv1.QueryEventsRequest{Filter: &auditv1.EventFilter{Actor: "nobody"}}); len(r.Events) != 0 {
		t.Fatal("actor filter")
	}
}
