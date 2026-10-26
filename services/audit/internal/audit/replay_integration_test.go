//go:build integration

package audit

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/events"
)

// relayAccounts copies the accounts outbox into the audit store (what SNS→SQS→consumer does).
func (f *fixture) relayAccounts(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rows, err := f.pool.Query(ctx, `SELECT payload FROM accounts.outbox WHERE published_at IS NULL ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var evs []events.Event
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var ev events.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	rows.Close()
	for _, ev := range evs {
		if _, err := f.store.Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE accounts.outbox SET published_at=now() WHERE published_at IS NULL`); err != nil {
		t.Fatal(err)
	}
}

// liveAccount creates an account row (its trigger emits account.changed) plus ledger balances and
// the matching ledger.journal_posted event.
func (f *fixture) liveAccount(t *testing.T, collateral, receivable int64) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := f.pool.Exec(ctx, `INSERT INTO accounts.accounts (id, customer_id, currency, state, collateral_minor, credit_limit_minor, activated_at)
		VALUES ($1::uuid, 'cust-'||$1::text, 'USD', 'active', $2, $2, now())`, id, collateral); err != nil {
		t.Fatal(err)
	}
	for _, sa := range []struct {
		name string
		raw  int64
	}{{"cash_clearing", collateral - receivable}, {"collateral_held", -collateral}, {"credit_available", 0}, {"receivable", receivable}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO ledger.ledger_accounts (account_id, sub_account, currency) VALUES ($1,$2,'USD')`, id, sa.name); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO ledger.balances (account_id, sub_account, raw_balance_minor) VALUES ($1,$2,$3)`, id, sa.name, sa.raw); err != nil {
			t.Fatal(err)
		}
	}
	f.relayAccounts(t)
	ev := events.New("ledger.journal_posted", "journal_entry", uuid.NewString())
	ev.After, _ = json.Marshal(map[string]any{"account_id": id, "balances": map[string]int64{"cash_clearing": collateral - receivable, "collateral_held": collateral, "receivable": receivable}})
	if _, err := f.store.Record(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) replay(t *testing.T, acc string) *auditv1.ReplayReport {
	t.Helper()
	r, err := f.svc.Replay(context.Background(), &auditv1.ReplayRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReplayCleanAccount(t *testing.T) {
	f := setup(t)
	acc := f.liveAccount(t, 100000, 30000)
	if _, err := f.pool.Exec(context.Background(), `UPDATE accounts.accounts SET frozen=true WHERE id=$1`, acc); err != nil { // a normal change: evented
		t.Fatal(err)
	}
	f.relayAccounts(t)
	if r := f.replay(t, acc); len(r.Differences) != 0 || r.BasedOnEvent == "" {
		t.Fatalf("clean replay: %+v", r)
	}
}

func TestReplayDetectsLiveCorruption(t *testing.T) {
	f := setup(t)
	acc := f.liveAccount(t, 100000, 30000)
	f.tamper(t, `UPDATE accounts.accounts SET credit_limit_minor=50000 WHERE id=$1`, acc) // bypasses the trigger
	r := f.replay(t, acc)
	if len(r.Differences) != 1 || r.Differences[0].Field != "credit_limit_minor" || r.Differences[0].Audit != "100000" || r.Differences[0].Live != "50000" {
		t.Fatalf("corruption: %+v", r.Differences)
	}
}

func TestReplayLedgerBalance(t *testing.T) {
	f := setup(t)
	acc := f.liveAccount(t, 100000, 30000)
	f.tamper(t, `UPDATE ledger.balances SET raw_balance_minor=1 WHERE account_id=$1 AND sub_account='receivable'`, acc)
	r := f.replay(t, acc)
	if len(r.Differences) != 1 || r.Differences[0].Field != "balance.receivable" {
		t.Fatalf("ledger corruption: %+v", r.Differences)
	}
}

func TestReplayUsesSnapshot(t *testing.T) {
	f := setup(t)
	acc := f.liveAccount(t, 5000, 0)
	ev := events.New("snapshot.account", "account", acc)
	ev.After, _ = json.Marshal(map[string]any{"account": map[string]any{"state": "active", "frozen": false, "collateral_minor": 5000, "credit_limit_minor": 5000,
		"purchase_limit_minor": 0, "restricted_reason": nil, "delinquency": "none", "close_stage": nil, "clock_offset_seconds": 0},
		"balances": map[string]int64{"cash_clearing": 5000, "collateral_held": 5000, "credit_available": 0, "receivable": 0}, "snapshot_hash": "x"})
	if _, err := f.store.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if r := f.replay(t, acc); len(r.Differences) != 0 || r.BasedOnEvent != ev.ID {
		t.Fatalf("snapshot replay: %+v", r)
	}
}

func TestReconciliationCleanAndViolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r, err := f.svc.RunReconciliation(ctx, &auditv1.RunReconciliationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalViolations != 0 || len(r.Checks) < 30 || r.S3Key == "" {
		t.Fatalf("clean run: %+v", r)
	}
	if body, err := f.objects.Get(ctx, r.S3Key); err != nil || len(body) == 0 || !f.objects.Locked(r.S3Key) {
		t.Fatalf("report object: %v locked=%v", err, f.objects.Locked(r.S3Key))
	}
	acc := f.liveAccount(t, 1000, 0)
	f.tamper(t, `UPDATE ledger.balances SET raw_balance_minor=raw_balance_minor+1 WHERE account_id=$1 AND sub_account='cash_clearing'`, acc)
	r, err = f.svc.RunReconciliation(ctx, &auditv1.RunReconciliationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalViolations == 0 {
		t.Fatalf("corruption not reported: %+v", r)
	}
	var violations int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE payload->>'type'='reconciliation.violation'`).Scan(&violations); err != nil || violations == 0 {
		t.Fatalf("violation events=%d %v", violations, err)
	}
	last, err := f.svc.GetReconciliation(ctx, &auditv1.GetReconciliationRequest{})
	if err != nil || last.RunId != r.RunId {
		t.Fatalf("latest: %v %+v", err, last)
	}
}

func TestEmbeddedVerifySQLMatchesLoadFile(t *testing.T) {
	load, err := os.ReadFile("../../../../tests/load/verify.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(load) != verifySQL {
		t.Fatal("services/audit/internal/audit/verify.sql must equal tests/load/verify.sql (copy it)")
	}
}
