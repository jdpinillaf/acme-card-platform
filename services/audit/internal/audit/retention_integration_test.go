//go:build integration

package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/events"
)

var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.`)

func (f *fixture) recordEvent(t *testing.T, typ, aggID, after string, occurred time.Time) {
	t.Helper()
	ev := events.New(typ, "account", aggID)
	ev.After, ev.OccurredAt = []byte(after), occurred
	if _, err := f.store.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func TestExportHasNoSecrets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// A row written before Sprint 6 was never redacted: the export must scrub it too.
	f.tamper(t, `INSERT INTO audit_events (id, event_type, aggregate_type, aggregate_id, after, occurred_at)
		VALUES (gen_random_uuid(), 'payout.instruction_created', 'account', 'acc-1',
		'{"account_id":"acc-1","account_number":"999988887777","note":"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJsZWdhY3kifQ.c2ln"}', now())`)
	if _, err := f.store.BackfillChain(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.recordEvent(t, "account.close_requested", "acc-1", `{"account_id":"acc-1","state":"closing"}`, now)
	f.recordEvent(t, "payout.instruction_created", "pay-1",
		`{"account_id":"acc-1","account_number":"000123456789","holder_name":"Jane Roe","destination_ciphertext":"c2VjcmV0Q2lwaGVy","last4":"6789"}`, now)
	f.recordEvent(t, "http.request", "acc-1", `{"path":"/v1/accounts/acc-1/close","token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjdXN0LTEifQ.c2ln"}`, now)
	f.recordEvent(t, "account.closed", "acc-1", `{"account_id":"acc-1","state":"closed"}`, now)
	f.recordEvent(t, "account.opened", "acc-2", `{"account_id":"acc-2"}`, now) // other account: not exported

	ex, err := f.svc.CreateExport(ctx, &auditv1.CreateExportRequest{Filter: &auditv1.EventFilter{AccountId: "acc-1"}, Meta: &commonv1.RequestMeta{Actor: "auditor-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Count != 5 || ex.S3Key != "exports/"+ex.Id+".ndjson" {
		t.Fatalf("export: %+v", ex)
	}
	got, err := f.svc.GetExport(ctx, &auditv1.GetExportRequest{Id: ex.Id})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(got.Body)
	if hex.EncodeToString(sum[:]) != ex.Sha256 || got.Export.GetSha256() != ex.Sha256 {
		t.Fatal("export sha256 does not match its body")
	}
	body := string(got.Body)
	if n := strings.Count(body, "\n"); n != 5 {
		t.Fatalf("lines=%d", n)
	}
	for _, secret := range []string{"000123456789", "999988887777", "Jane Roe", "c2VjcmV0Q2lwaGVy", "acc-2"} {
		if strings.Contains(body, secret) {
			t.Fatalf("export leaks %q:\n%s", secret, body)
		}
	}
	if jwtPattern.MatchString(body) {
		t.Fatalf("export contains a JWT:\n%s", body)
	}
	var actor string
	if err := f.pool.QueryRow(ctx, `SELECT actor FROM audit_exports WHERE id=$1`, ex.Id).Scan(&actor); err != nil || actor != "auditor-1" {
		t.Fatalf("audit_exports row: %q %v", actor, err)
	}
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// age makes the first n chain rows look 14 months old (recorded_at is not part of the hash).
func (f *fixture) age(t *testing.T, through int64) {
	f.tamper(t, `UPDATE audit_events SET recorded_at = now() - interval '14 months' WHERE chain_seq <= $1`, through)
}

func TestRetentionArchiveVerifyPurge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, -14, 0)
	for i := 0; i < 6; i++ {
		f.recordEvent(t, "x.old", fmt.Sprintf("old-%d", i), fmt.Sprintf(`{"n":%d}`, i), old)
	}
	if _, err := f.svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{}); err != nil { // anchors seq 6, purged below
		t.Fatal(err)
	}
	f.record(t, "accounts", 1, 2, 3, 4)
	f.age(t, 6)

	rep, err := f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Archived != 6 || rep.Purged != 6 || rep.ManifestKey == "" {
		t.Fatalf("retention: %+v", rep)
	}
	if !f.objects.Locked(rep.ManifestKey) {
		t.Fatal("manifest not object-locked")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events`); n != 4 {
		t.Fatalf("rows left=%d", n)
	}
	if n := f.count(t, `SELECT min(chain_seq) FROM audit_events`); n != 7 {
		t.Fatalf("first remaining chain_seq=%d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_archives WHERE verified_at IS NOT NULL AND purged_at IS NOT NULL AND last_chain_seq=6`); n != 1 {
		t.Fatal("archive row not verified+purged")
	}
	v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.ChainValid || !v.CheckpointsValid || v.Checked != 4 {
		t.Fatalf("verify after purge: %+v", v)
	}
	// a new event still links to the chain
	f.record(t, "accounts", 5)
	if v, _ = f.svc.Verify(ctx, &auditv1.VerifyRequest{}); !v.ChainValid {
		t.Fatalf("verify after insert: %+v", v)
	}
	// nothing more is old enough
	if rep, err = f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{}); err != nil || rep.Archived != 0 || rep.Purged != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
	// a forged archive breaks the purged checkpoint's check
	var key string
	_ = f.pool.QueryRow(ctx, `SELECT s3_key FROM audit_archives`).Scan(&key)
	_ = f.objects.Overwrite(key, []byte("{}\n"))
	if v, _ = f.svc.Verify(ctx, &auditv1.VerifyRequest{}); v.CheckpointsValid {
		t.Fatal("forged archive not detected")
	}
}

func TestRetentionNowOverride(t *testing.T) {
	f := setup(t)
	f.record(t, "accounts", 1, 2, 3)
	rep, err := f.svc.RunRetention(context.Background(), &auditv1.RunRetentionRequest{Now: timestamppb.New(time.Now().AddDate(1, 2, 0))})
	if err != nil || rep.Archived != 3 || rep.Purged != 3 {
		t.Fatalf("retention with now+14m: %+v %v", rep, err)
	}
}

func TestPurgeRefusedWithoutArchive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2, 3)
	if _, err := f.pool.Exec(ctx, `SELECT audit_purge_archived(3)`); err == nil {
		t.Fatal("purge without archive accepted")
	}
	// an archive that was never verified does not count
	if _, err := f.pool.Exec(ctx, `INSERT INTO audit_archives (first_chain_seq, last_chain_seq, first_prev_hash, last_hash, count, sha256, s3_key, manifest_key)
		VALUES (1, 3, repeat('0',64), repeat('a',64), 3, repeat('b',64), 'k', 'm')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `SELECT audit_purge_archived(3)`); err == nil {
		t.Fatal("purge with unverified archive accepted")
	}
	// setting the flag by hand does not open a back door
	err := func() error {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL audit.purge = 'on'`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM audit_events WHERE chain_seq = 1`)
		return err
	}()
	if err == nil {
		t.Fatal("manual audit.purge delete accepted")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events`); n != 3 {
		t.Fatalf("rows=%d", n)
	}
}

func TestPlainDeleteStillRefused(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.record(t, "accounts", 1, 2)
	f.age(t, 2)
	if _, _, err := f.svc.Archive(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.VerifyArchives(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`DELETE FROM audit_events WHERE chain_seq = 1`, `TRUNCATE audit_events`, `UPDATE audit_events SET actor='x' WHERE chain_seq=2`} {
		if _, err := f.pool.Exec(ctx, sql); err == nil {
			t.Fatalf("%s accepted", sql)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events`); n != 2 {
		t.Fatalf("rows=%d", n)
	}
}

func TestPurgeRacesInserts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		f.recordEvent(t, "x.old", fmt.Sprintf("old-%d", i), `{}`, time.Now().AddDate(0, -14, 0))
	}
	f.age(t, 50)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				ev := events.New("x.new", "account", "new")
				if _, err := f.store.Record(ctx, ev); err != nil {
					errs <- err
				}
			}
		}()
	}
	rep, err := f.svc.RunRetention(ctx, &auditv1.RunRetentionRequest{})
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if err != nil {
		t.Fatal(err)
	}
	if rep.Archived != 50 || rep.Purged != 50 {
		t.Fatalf("retention: %+v", rep)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE event_type='x.new'`); n != 200 {
		t.Fatalf("new rows=%d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events`); n != 200 {
		t.Fatalf("rows=%d (unarchived old rows removed or kept wrongly)", n)
	}
	if v, err := f.svc.Verify(ctx, &auditv1.VerifyRequest{}); err != nil || !v.ChainValid || v.Checked != 200 {
		t.Fatalf("verify: %+v %v", v, err)
	}
}
