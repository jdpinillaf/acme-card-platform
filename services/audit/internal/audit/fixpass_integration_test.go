//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/events"
)

// slowStore delays List and PutLocked, widening the read→write windows of concurrent writers.
type slowStore struct{ *MemStore }

func (s slowStore) List(ctx context.Context, prefix string) ([]string, error) {
	time.Sleep(150 * time.Millisecond)
	return s.MemStore.List(ctx, prefix)
}

func (s slowStore) PutLocked(ctx context.Context, key string, body []byte, until time.Time) error {
	time.Sleep(150 * time.Millisecond)
	return s.MemStore.PutLocked(ctx, key, body, until)
}

func TestConcurrentCheckpointsKeepOneChain(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.objects = slowStore{f.objects}
	f.record(t, "accounts", 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{}); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(30 * time.Millisecond)
		f.record(t, "accounts", 2)
		if _, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{}); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{Full: true}); err != nil || !v.CheckpointsValid || v.Checkpoints != 2 {
		t.Fatalf("concurrent checkpoints: %+v %v", v, err)
	}
}

func TestForgedArchiveAndVerificationRows(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	// archive bookkeeping is append-only too
	if _, err := f.pool.Exec(ctx, `INSERT INTO audit_archives (first_chain_seq, last_chain_seq, first_prev_hash, last_hash, count, sha256, s3_key, manifest_key, verified_at)
		VALUES (1, 10, repeat('0',64), repeat('a',64), 10, repeat('b',64), 'k', 'm', now())`); err == nil {
		t.Fatal("archive inserted as already verified")
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO audit_archives (first_chain_seq, last_chain_seq, first_prev_hash, last_hash, count, sha256, s3_key, manifest_key)
		VALUES (1, 10, repeat('0',64), repeat('a',64), 10, repeat('b',64), 'k', 'm')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE audit_archives SET last_chain_seq = 9`); err == nil {
		t.Fatal("archive range rewritten")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM audit_archives`); err == nil {
		t.Fatal("archive row deleted")
	}
	// the service never purges an archive whose signed manifest does not check out
	if _, err := f.pool.Exec(ctx, `UPDATE audit_archives SET verified_at = now()`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.Purge(ctx); err == nil || n != 0 {
		t.Fatalf("purge of a forged archive: %d %v", n, err)
	}
	// someone with database access purges anyway: verify says so
	if _, err := f.pool.Exec(ctx, `SELECT audit_purge_archived(10)`); err != nil {
		t.Fatal(err)
	}
	if v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{}); err != nil || v.ChainValid {
		t.Fatalf("verify after a forged purge: %+v %v", v, err)
	}
}

func TestForgedVerificationAnchorIgnored(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3, 4, 5, 6)
	f.tamper(t, `UPDATE audit_events SET after='{"n":0}' WHERE chain_seq=3`)
	var hash string
	_ = f.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=6`).Scan(&hash)
	if _, err := f.pool.Exec(ctx, `INSERT INTO audit_verifications (through_chain_seq, through_hash, checked, signature) VALUES (6, $1, 6, 'forged')`, hash); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.svc.Verify(ctx, &auditv1.VerifyRequest{}); v.ChainValid || v.FirstBadChainSeq != 3 {
		t.Fatalf("forged anchor trusted: %+v", v)
	}
}

func TestIncrementalVerifyCatchesDeletionBelowAnchor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	if _, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{Full: true}); err != nil {
		t.Fatal(err)
	}
	f.tamper(t, `DELETE FROM audit_events WHERE chain_seq=5`)
	if v, _ := f.svc.Verify(ctx, &auditv1.VerifyRequest{}); v.ChainValid || v.FirstBadChainSeq != 5 {
		t.Fatalf("deletion below the anchor: %+v", v)
	}
}

func TestFullRunNotRecordedWhenCheckpointsFail(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2)
	cp, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = f.objects.Overwrite(cp.S3Key, []byte(`{"chain_seq":2}`))
	if v, _ := f.svc.Verify(ctx, &auditv1.VerifyRequest{Full: true}); v.CheckpointsValid {
		t.Fatal("forged checkpoint accepted")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_verifications`); n != 0 {
		t.Fatalf("anchor recorded despite invalid checkpoints: %d", n)
	}
}

func TestNoFalseGapsAfterPurge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3, 4, 5, 6)
	f.age(t, 6)
	f.record(t, "accounts", 7, 8, 9)
	if rep, err := f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{}); err != nil || rep.Purged != 6 {
		t.Fatalf("retention: %+v %v", rep, err)
	}
	v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{})
	if err != nil || !v.ChainValid || len(v.Gaps) != 0 {
		t.Fatalf("verify after purge: %+v %v", v, err)
	}
	f.record(t, "accounts", 11) // a real gap (10) is still reported
	if v, _ = f.svc.Verify(ctx, &auditv1.VerifyRequest{}); len(v.Gaps) != 1 || v.Gaps[0].FromSeq != 10 {
		t.Fatalf("real gap after purge: %+v", v.Gaps)
	}
}

func TestArchiveRangeDisagreementDoesNotPanic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3)
	if _, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{}); err != nil { // anchors seq 3
		t.Fatal(err)
	}
	f.age(t, 2)
	if rep, err := f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{}); err != nil || rep.Purged != 2 {
		t.Fatalf("retention: %+v %v", rep, err)
	}
	// the DB row is widened past its signed manifest (1..2) and the anchored row removed
	f.tamper(t, `DELETE FROM audit_events WHERE chain_seq = 3`)
	f.tamper(t, `UPDATE audit_archives SET last_chain_seq = 3, count = 3`)
	v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{})
	if err != nil || v.CheckpointsValid || v.ChainValid {
		t.Fatalf("verify with a widened archive row: %+v %v", v, err)
	}
}

func TestArchivesAreChunked(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.cfg.ArchiveChunk = 10
	for i := 0; i < 25; i++ {
		f.recordEvent(t, "x.old", fmt.Sprintf("o-%d", i), `{}`, time.Now().AddDate(0, -14, 0))
	}
	f.age(t, 25)
	rep, err := f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{})
	if err != nil || rep.Archived != 25 || rep.Purged != 25 {
		t.Fatalf("chunked retention: %+v %v", rep, err)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_archives WHERE count <= 10`); n != 3 {
		t.Fatalf("archives=%d", n)
	}
}

func TestConcurrentArchiveCallsDoNotOverlap(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.svc.objects = slowStore{f.objects}
	f.record(t, "accounts", 1, 2, 3, 4, 5, 6)
	f.tamper(t, `UPDATE audit_events SET recorded_at = now() - interval '15 months' WHERE chain_seq <= 3`)
	f.tamper(t, `UPDATE audit_events SET recorded_at = now() - interval '14 months' WHERE chain_seq > 3`)
	var wg sync.WaitGroup
	for _, now := range []time.Time{time.Now(), time.Now().AddDate(0, 0, -45)} { // throughs 6 and 3
		wg.Add(1)
		go func(now time.Time) {
			defer wg.Done()
			if _, _, err := f.svc.Archive(ctx, now); err != nil {
				t.Error(err)
			}
		}(now)
	}
	wg.Wait()
	if err := f.svc.VerifyArchives(ctx); err != nil {
		t.Fatalf("archives overlap: %v", err)
	}
	if n := f.count(t, `SELECT coalesce(sum(count),0) FROM audit_archives`); n != 6 {
		t.Fatalf("archived rows=%d", n)
	}
}

func TestReplayOrdersBySourceSeq(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.liveAccount(t, 100000, 30000)
	rec := func(typ, source string, seq int64, after any) {
		ev := events.New(typ, "journal_entry", uuid.NewString())
		if typ != "ledger.journal_posted" {
			ev.AggregateType, ev.AggregateID = "account", acc
		}
		ev.Source, ev.SourceSeq = source, seq
		ev.After, _ = json.Marshal(after)
		if _, err := f.store.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	// SQS delivers the newer journal first, then a stale one
	rec("ledger.journal_posted", "ledger", 9, map[string]any{"account_id": acc, "balances": map[string]int64{"receivable": 30000}})
	rec("ledger.journal_posted", "ledger", 8, map[string]any{"account_id": acc, "balances": map[string]int64{"receivable": 10000}})
	if r := f.replay(t, acc); len(r.Differences) != 0 {
		t.Fatalf("stale journal won: %+v", r.Differences)
	}
	// same for account state: a stale account.changed after a newer snapshot
	var live []byte
	_ = f.pool.QueryRow(ctx, `SELECT after::text FROM audit_events WHERE aggregate_id=$1 AND event_type='account.changed' ORDER BY chain_seq DESC LIMIT 1`, acc).Scan(&live)
	var state map[string]any
	_ = json.Unmarshal(live, &state)
	rec("snapshot.account", "accounts", 20, map[string]any{"account": state, "balances": map[string]int64{}})
	stale := map[string]any{}
	for k, v := range state {
		stale[k] = v
	}
	stale["frozen"] = true
	rec("account.changed", "accounts", 19, stale)
	if r := f.replay(t, acc); len(r.Differences) != 0 {
		t.Fatalf("stale account.changed won: %+v", r.Differences)
	}
}
