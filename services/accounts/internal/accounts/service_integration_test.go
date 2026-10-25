//go:build integration

package accounts

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/accounts/migrations"
	"github.com/acme/acme-card/services/ledger/ledgertest"
	ledgermig "github.com/acme/acme-card/services/ledger/migrations"
)

type fixture struct {
	svc    *Service
	pool   *pgxpool.Pool
	ledger *pgxpool.Pool
}

func setup(t *testing.T) *fixture {
	dsn := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(dsn, "accounts", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "ledger", ledgermig.FS, "."); err != nil {
		t.Fatal(err)
	}
	ap, err := db.Connect(ctx, dsn, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	lp, err := db.Connect(ctx, dsn, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close(); lp.Close() })
	return &fixture{svc: NewService(ap, ledgertest.NewClient(lp)), pool: ap, ledger: lp}
}

func (f *fixture) open(t *testing.T, customer string) *accountsv1.Account {
	r, err := f.svc.CreateAccount(context.Background(), &accountsv1.CreateAccountRequest{CustomerId: customer, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func (f *fixture) deposit(t *testing.T, acc string, amt int64) *accountsv1.Deposit {
	r, err := f.svc.CreateDeposit(context.Background(), &accountsv1.CreateDepositRequest{AccountId: acc, AmountMinor: amt})
	if err != nil {
		t.Fatal(err)
	}
	return r.Deposit
}

func settled(dep *accountsv1.Deposit, ref string) *accountsv1.SettleDepositRequest {
	return &accountsv1.SettleDepositRequest{DepositId: dep.Id, ExternalRef: ref, AmountMinor: dep.Amount.AmountMinor, Currency: dep.Amount.Currency, Status: accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED}
}

func (f *fixture) count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateAccountStartsPendingAndIsUniquePerCurrency(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.open(t, "cust-1")
	if acc.State != accountsv1.AccountState_ACCOUNT_STATE_PENDING_COLLATERAL || acc.CreditLimitMinor != 0 || acc.Currency != "USD" {
		t.Fatalf("%+v", acc)
	}
	_, err := f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: "cust-1", Currency: "USD"})
	if status.Code(err) != codes.AlreadyExists || grpcx.Reason(err) != "account_exists" {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: "cust-1", Currency: "COP"}); err != nil {
		t.Fatalf("second currency must be allowed: %v", err)
	}
	if _, err := f.svc.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: "cust-1", Currency: "usd"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad currency: %v", err)
	}
}

func TestDepositSettlementActivatesWithLimitEqualToCollateral(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.open(t, "cust-1")
	dep := f.deposit(t, acc.Id, 50_000)
	if dep.Status != accountsv1.DepositStatus_DEPOSIT_STATUS_PENDING || dep.Amount.Currency != "USD" {
		t.Fatalf("%+v", dep)
	}
	r, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-1"))
	if err != nil || r.Replayed {
		t.Fatalf("settle: %v %+v", err, r)
	}
	a := r.Account
	if a.State != accountsv1.AccountState_ACCOUNT_STATE_ACTIVE || a.CreditLimitMinor != 50_000 || a.CollateralMinor != 50_000 || a.AvailableCreditMinor != 50_000 {
		t.Fatalf("account after settle: %+v", a)
	}
	// Top-up after activation grows the limit.
	dep2 := f.deposit(t, acc.Id, 25_000)
	r, err = f.svc.SettleDeposit(ctx, settled(dep2, "bank-2"))
	if err != nil || r.Account.CreditLimitMinor != 75_000 {
		t.Fatalf("top-up: %v %+v", err, r)
	}
	g, err := f.svc.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: acc.Id})
	if err != nil || g.Account.AvailableCreditMinor != 75_000 {
		t.Fatalf("get: %v %+v", err, g)
	}
	// Events: opened, 2x deposit_created, 2x confirmed, exactly 1 activation.
	for typ, want := range map[string]int{"account.opened": 1, "collateral.deposit_created": 2, "collateral.confirmed": 2, "card.activated": 1} {
		if n := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'=$1`, typ); n != want {
			t.Errorf("%s events=%d want %d", typ, n, want)
		}
	}
	var before, after string
	_ = f.pool.QueryRow(ctx, `SELECT payload->'before'->>'state', payload->'after'->>'state' FROM outbox WHERE payload->>'type'='card.activated'`).Scan(&before, &after)
	if before != "pending_collateral" || after != "active" {
		t.Fatalf("activation snapshot before=%q after=%q", before, after)
	}
}

func TestSettleReplayFailureAndValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.open(t, "cust-1")
	dep := f.deposit(t, acc.Id, 1_000)

	bad := settled(dep, "bank-x")
	bad.AmountMinor = 999
	if _, err := f.svc.SettleDeposit(ctx, bad); status.Code(err) != codes.InvalidArgument || grpcx.Reason(err) != "amount_mismatch" {
		t.Fatalf("amount mismatch: %v", err)
	}
	bad = settled(dep, "bank-x")
	bad.Currency = "EUR"
	if _, err := f.svc.SettleDeposit(ctx, bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("currency mismatch: %v", err)
	}

	if _, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-1")); err != nil {
		t.Fatal(err)
	}
	r, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-1"))
	if err != nil || !r.Replayed {
		t.Fatalf("replay: %v %+v", err, r)
	}
	if _, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-OTHER")); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("different external_ref on settled deposit: %v", err)
	}

	// external_ref reused across deposits is rejected.
	dep2 := f.deposit(t, acc.Id, 1_000)
	if _, err := f.svc.SettleDeposit(ctx, settled(dep2, "bank-1")); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("external_ref reuse: %v", err)
	}

	// FAILED settlement leaves account untouched.
	dep3 := f.deposit(t, acc.Id, 7_000)
	fr := settled(dep3, "bank-3")
	fr.Status = accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED
	r, err = f.svc.SettleDeposit(ctx, fr)
	if err != nil || r.Deposit.Status != accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED || r.Account.CreditLimitMinor != 1_000 {
		t.Fatalf("failed settle: %v %+v", err, r)
	}
	if _, err := f.svc.SettleDeposit(ctx, settled(dep3, "bank-3")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("settling a failed deposit: %v", err)
	}
	if _, err := f.svc.SettleDeposit(ctx, settled(&accountsv1.Deposit{Id: "00000000-0000-0000-0000-000000000000", Amount: dep.Amount}, "z")); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown deposit: %v", err)
	}
}

func TestClosedAccountRejectsDepositsAndSettlement(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.open(t, "cust-1")
	dep := f.deposit(t, acc.Id, 1_000)
	if _, err := f.pool.Exec(ctx, `UPDATE accounts SET state='closed' WHERE id=$1`, acc.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateDeposit(ctx, &accountsv1.CreateDepositRequest{AccountId: acc.Id, AmountMinor: 5}); grpcx.Reason(err) != "account_closed" {
		t.Fatalf("deposit on closed: %v", err)
	}
	if _, err := f.svc.SettleDeposit(ctx, settled(dep, "b")); grpcx.Reason(err) != "account_closed" {
		t.Fatalf("settle on closed: %v", err)
	}
	if _, err := f.svc.CreateDeposit(ctx, &accountsv1.CreateDepositRequest{AccountId: acc.Id, AmountMinor: 0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("zero deposit: %v", err)
	}
}

func TestConcurrentSettlementSameWebhookAppliesOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := f.open(t, "cust-race")
	dep := f.deposit(t, acc.Id, 10_000)

	const n = 50
	var fresh, replayed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := f.svc.SettleDeposit(ctx, settled(dep, "bank-race"))
			if err != nil {
				t.Error(err)
				return
			}
			if r.Account.CreditLimitMinor != 10_000 {
				t.Errorf("limit=%d", r.Account.CreditLimitMinor)
			}
			if r.Replayed {
				replayed.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if fresh.Load() != 1 || replayed.Load() != n-1 {
		t.Fatalf("fresh=%d replayed=%d", fresh.Load(), replayed.Load())
	}
	if c := f.count(t, f.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1`, acc.Id); c != 1 {
		t.Fatalf("journal entries=%d want 1", c)
	}
	if c := f.count(t, f.pool, `SELECT count(*) FROM outbox WHERE payload->>'type'='card.activated'`); c != 1 {
		t.Fatalf("activations=%d want 1", c)
	}
}
