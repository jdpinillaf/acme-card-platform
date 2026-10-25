//go:build integration

package billing

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/accounts/accountstest"
	accmig "github.com/acme/acme-card/services/accounts/migrations"
	"github.com/acme/acme-card/services/billing/internal/engine"
	"github.com/acme/acme-card/services/billing/migrations"
	"github.com/acme/acme-card/services/ledger/ledgertest"
	ledmig "github.com/acme/acme-card/services/ledger/migrations"
)

const dayDur = 24 * time.Hour

type fixture struct {
	svc      *Service
	pool     *pgxpool.Pool
	ledgerDB *pgxpool.Pool
	ledger   ledgerv1.LedgerServiceClient
	accounts accountsv1.AccountsServiceClient
	store    *MemStore
	seq      atomic.Int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	ctx := context.Background()
	if err := db.Migrate(dsn, "ledger", ledmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "accounts", accmig.FS, "."); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(dsn, "billing", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	connect := func(schema string) *pgxpool.Pool {
		p, err := db.ConnectN(ctx, dsn, schema, 60)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	f := &fixture{pool: connect("billing"), ledgerDB: connect("ledger"), store: NewMemStore()}
	f.ledger = ledgertest.NewClient(f.ledgerDB)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	accountsv1.RegisterAccountsServiceServer(srv, accountstest.NewServer(connect("accounts"), f.ledger))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	f.accounts = accountsv1.NewAccountsServiceClient(conn)
	f.svc = NewService(f.pool, f.ledger, f.accounts, f.store, Config{APRBps: 2280, Fixed: map[string]int64{"USD": 2500}, Bucket: "test-bucket"})
	return f
}

func (f *fixture) ref(prefix string) string { return fmt.Sprintf("%s-%d", prefix, f.seq.Add(1)) }

// activeAccount opens and funds an account with collateral `amt`.
func (f *fixture) activeAccount(t *testing.T, amt int64) *accountsv1.Account {
	t.Helper()
	ctx := context.Background()
	acc, err := f.accounts.CreateAccount(ctx, &accountsv1.CreateAccountRequest{CustomerId: f.ref("cust"), Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := f.accounts.CreateDeposit(ctx, &accountsv1.CreateDepositRequest{AccountId: acc.Account.Id, AmountMinor: amt})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.accounts.SettleDeposit(ctx, &accountsv1.SettleDepositRequest{DepositId: dep.Deposit.Id, ExternalRef: f.ref("bank"), AmountMinor: amt,
		Currency: "USD", Status: accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

func (f *fixture) purchase(t *testing.T, acc string, amt int64) {
	t.Helper()
	ctx := context.Background()
	a, err := f.accounts.Authorize(ctx, &accountsv1.AuthorizeRequest{NetworkRef: f.ref("net"), CardId: acc, AmountMinor: amt, Currency: "USD",
		Merchant: &accountsv1.Merchant{Id: "m", Name: "Shop", Mcc: "5411"}})
	if err != nil || a.Authorization.Status != accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED {
		t.Fatalf("authorize: %v %+v", err, a)
	}
	if _, err := f.accounts.Capture(ctx, &accountsv1.CaptureRequest{AuthorizationId: a.Authorization.Id, NetworkRef: f.ref("cap"), AmountMinor: amt}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) authorize(t *testing.T, acc string, amt int64) *accountsv1.Authorization {
	t.Helper()
	a, err := f.accounts.Authorize(context.Background(), &accountsv1.AuthorizeRequest{NetworkRef: f.ref("net"), CardId: acc, AmountMinor: amt, Currency: "USD",
		Merchant: &accountsv1.Merchant{Id: "m", Name: "Shop", Mcc: "5411"}})
	if err != nil {
		t.Fatal(err)
	}
	return a.Authorization
}

func (f *fixture) createPayment(t *testing.T, acc string, amt int64) *accountsv1.Payment {
	t.Helper()
	p, err := f.accounts.CreatePayment(context.Background(), &accountsv1.CreatePaymentRequest{AccountId: acc, AmountMinor: amt})
	if err != nil {
		t.Fatal(err)
	}
	return p.Payment
}

func (f *fixture) bank(t *testing.T, p *accountsv1.Payment, st accountsv1.PaymentStatus) {
	t.Helper()
	if _, err := f.accounts.SettlePayment(context.Background(), &accountsv1.SettlePaymentRequest{PaymentId: p.Id, ExternalRef: f.ref("bankp"),
		AmountMinor: p.Amount.AmountMinor, Currency: p.Amount.Currency, Status: st}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) pay(t *testing.T, acc string, amt int64) {
	f.bank(t, f.createPayment(t, acc, amt), accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED)
}

func (f *fixture) account(t *testing.T, acc string) *accountsv1.Account {
	t.Helper()
	r, err := f.accounts.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: acc})
	if err != nil {
		t.Fatal(err)
	}
	return r.Account
}

// advanceTo moves the account clock to the first whole day at or after target, then catches up.
func (f *fixture) advanceTo(t *testing.T, acc string, target time.Time) *accountsv1.Account {
	t.Helper()
	now := f.account(t, acc).AccountTime.AsTime()
	if !target.After(now) {
		f.catchUp(t, acc)
		return f.account(t, acc)
	}
	days := int32((target.Sub(now) + dayDur - 1) / dayDur)
	for days > 0 {
		step := min(days, 400)
		if _, err := f.accounts.AdvanceClock(context.Background(), &accountsv1.AdvanceClockRequest{AccountId: acc, Days: step}); err != nil {
			t.Fatal(err)
		}
		days -= step
	}
	f.catchUp(t, acc)
	return f.account(t, acc)
}

func (f *fixture) catchUp(t *testing.T, acc string) {
	t.Helper()
	if _, err := f.svc.CatchUpAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) closeN(t *testing.T, acc *accountsv1.Account, n int) time.Time {
	return engine.CycleClose(acc.ActivatedAt.AsTime(), n)
}

func (f *fixture) count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ledgerReceivableBefore sums receivable movements with effective_at < at.
func (f *fixture) ledgerReceivableBefore(t *testing.T, acc string, at time.Time) int64 {
	t.Helper()
	var s int64
	if err := f.ledgerDB.QueryRow(context.Background(), `SELECT coalesce(sum(CASE WHEN p.side='debit' THEN p.amount_minor ELSE -p.amount_minor END),0)
		FROM journal_entries j JOIN postings p ON p.journal_entry_id=j.id AND p.sub_account='receivable'
		WHERE j.account_id=$1 AND j.effective_at < $2`, acc, at).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
