//go:build integration

package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func (f *fixture) changedEvents(t *testing.T, acc string) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT payload FROM outbox WHERE payload->>'type'='account.changed' AND payload->>'aggregate_id'=$1 ORDER BY seq`, acc)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

func TestAccountChangedOnEveryMutation(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.open(t, "cust-au1")
	if n := len(f.changedEvents(t, acc.Id)); n != 1 {
		t.Fatalf("after open: %d", n)
	}
	dep := f.deposit(t, acc.Id, 1000)
	if _, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-au1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: true}); err != nil {
		t.Fatal(err)
	}
	evs := f.changedEvents(t, acc.Id)
	last := evs[len(evs)-1]["after"].(map[string]any)
	if len(evs) < 3 || last["frozen"] != true || last["collateral_minor"] != 1000.0 || last["state"] != "active" {
		t.Fatalf("events=%d last=%v", len(evs), last)
	}
	if _, err := f.svc.SetFrozen(ctx, &accountsv1.SetFrozenRequest{AccountId: acc.Id, Frozen: false}); err != nil {
		t.Fatal(err)
	}
	f.close(t, acc.Id)
	for _, e := range f.changedEvents(t, acc.Id) {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "destination_ciphertext") || strings.Contains(string(raw), dest.AccountNumber) {
			t.Fatalf("account.changed leaks the destination: %s", raw)
		}
		if e["occurred_at"] == nil || e["id"] == nil {
			t.Fatalf("envelope: %v", e)
		}
	}
}

func TestLeaseOnlyChangeEmitsNothing(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-au2", 1000)
	before := len(f.changedEvents(t, acc.Id))
	if _, err := f.pool.Exec(context.Background(), `UPDATE accounts SET close_checked_at=now(), close_lease_until=now(), close_lease_owner=gen_random_uuid() WHERE id=$1`, acc.Id); err != nil {
		t.Fatal(err)
	}
	if after := len(f.changedEvents(t, acc.Id)); after != before {
		t.Fatalf("lease-only change emitted %d events", after-before)
	}
}

func TestSnapshotAccounts(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	acc := f.withDebt(t, "cust-au3", 1000, 300)
	r, err := f.svc.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{AccountIds: []string{acc.Id}})
	if err != nil || r.Count != 1 {
		t.Fatalf("snapshot: %v %+v", err, r)
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT payload->'after' FROM outbox WHERE payload->>'type'='snapshot.account' AND payload->>'aggregate_id'=$1`, acc.Id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Account  json.RawMessage  `json:"account"`
		Balances map[string]int64 `json:"balances"`
		Hash     string           `json:"snapshot_hash"`
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if after.Balances["receivable"] != 300 || after.Balances["collateral_held"] != 1000 {
		t.Fatalf("balances: %v", after.Balances)
	}
	want, _ := SnapshotHash(after.Account, after.Balances)
	if after.Hash != want || len(after.Hash) != 64 {
		t.Fatalf("hash %s want %s", after.Hash, want)
	}
}

func TestSnapshotPendingAccountHasNoBalances(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	pending, err := f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: "cust-snp", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	// CreateAccount commits before opening the ledger (settlement re-opens it): simulate that
	// opening having failed, as it did for some accounts of the dev load runs.
	for _, q := range []string{`DELETE FROM ledger.balances WHERE account_id=$1`, `DELETE FROM ledger.ledger_accounts WHERE account_id=$1`} {
		if _, err := f.pool.Exec(ctx, q, pending.Account.Id); err != nil {
			t.Fatal(err)
		}
	}
	active := f.withDebt(t, "cust-sna", 1000, 100)
	// the all-accounts run covers the account with no ledger yet and keeps going
	r, err := f.svc.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{})
	if err != nil || r.Count < 2 {
		t.Fatalf("snapshot: %v %+v", err, r)
	}
	for _, id := range []string{pending.Account.Id, active.Id} {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE payload->>'type'='snapshot.account' AND payload->>'aggregate_id'=$1`, id).Scan(&n); err != nil || n != 1 {
			t.Fatalf("snapshot of %s: %d %v", id, n, err)
		}
	}
}

// snapFlakyLedger fails GetBalances for one account.
type snapFlakyLedger struct {
	ledgerv1.LedgerServiceClient
	fail string
}

func (l snapFlakyLedger) GetBalances(ctx context.Context, r *ledgerv1.GetBalancesRequest, o ...grpc.CallOption) (*ledgerv1.GetBalancesResponse, error) {
	if r.AccountId == l.fail {
		return nil, status.Error(codes.Unavailable, "ledger hiccup")
	}
	return l.LedgerServiceClient.GetBalances(ctx, r, o...)
}

func TestSnapshotRunSurvivesOneFailure(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.withDebt(t, fmt.Sprintf("cust-fl%d", i), 1000, 100).Id)
	}
	f.svc.ledger = snapFlakyLedger{f.svc.ledger, ids[0]}
	if _, err := f.svc.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{AccountIds: ids}); err == nil {
		t.Fatal("a failed account must be reported")
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(DISTINCT payload->>'aggregate_id') FROM outbox WHERE payload->>'type'='snapshot.account'
		AND payload->>'aggregate_id' = ANY($1)`, ids[1:]).Scan(&n); err != nil || n != 2 {
		t.Fatalf("other accounts snapshotted: %d %v", n, err)
	}
}

func TestSnapshotAccountsBackground(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.withDebt(t, fmt.Sprintf("cust-bg%d", i), 1000, 100).Id)
	}
	r, err := f.svc.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{Background: true})
	if err != nil || !r.Background || r.Count != 3 {
		t.Fatalf("background snapshot: %v %+v", err, r)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(DISTINCT payload->>'aggregate_id') FROM outbox
			WHERE payload->>'type'='snapshot.account' AND payload->>'aggregate_id' = ANY($1)`, ids).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background snapshot emitted %d/3", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := f.svc.SnapshotAccounts(ctx, &accountsv1.SnapshotAccountsRequest{Background: true, AccountIds: ids}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("background with ids: %v", err)
	}
}

func TestRunSnapshotsOncePerDay(t *testing.T) {
	f, _, _ := closeFixture(t)
	ctx := context.Background()
	f.activeAccount(t, "cust-au4", 1000)
	f.activeAccount(t, "cust-au5", 1000)
	n1, err := f.svc.SnapshotDaily(ctx)
	if err != nil || n1 != 2 {
		t.Fatalf("first run: %d %v", n1, err)
	}
	if n2, err := f.svc.SnapshotDaily(ctx); err != nil || n2 != 0 {
		t.Fatalf("second run same day: %d %v", n2, err)
	}
}

func TestInstructionReadAudited(t *testing.T) {
	f, _, _ := closeFixture(t)
	acc := f.activeAccount(t, "cust-au6", 1000)
	p := f.closeToPayout(t, acc.Id)
	if _, err := f.svc.GetPayoutInstruction(context.Background(), &accountsv1.GetPayoutInstructionRequest{AttemptId: p.AttemptId}); err != nil {
		t.Fatal(err)
	}
	if f.events(t, "payout.instruction_read") != 1 {
		t.Fatal("instruction read event")
	}
	if n := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'='payout.instruction_read' AND payload::text LIKE '%'||$1||'%'`, dest.AccountNumber); n != 0 {
		t.Fatal("instruction_read must not carry the number")
	}
}
