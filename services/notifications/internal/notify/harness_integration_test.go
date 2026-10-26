//go:build integration

package notify

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/services/notifications/migrations"
)

// fakeAccounts answers GetAccount from a map (customer "cust-<account>", USD) or fails.
type fakeAccounts struct {
	accountsv1.AccountsServiceClient
	mu    sync.Mutex
	fail  bool
	calls int
}

func (f *fakeAccounts) GetAccount(_ context.Context, r *accountsv1.GetAccountRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, status.Error(codes.Unavailable, "accounts down")
	}
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, CustomerId: "cust-" + r.AccountId, Currency: "USD"}}, nil
}

// recordingProviders records sends; fail makes a channel fail.
type recordingProviders struct {
	mu   sync.Mutex
	sent []string
	fail map[string]int // channel → failures left
}

func (p *recordingProviders) Send(_ context.Context, channel, recipient, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[channel] > 0 {
		p.fail[channel]--
		return status.Error(codes.Unavailable, channel+" provider down")
	}
	p.sent = append(p.sent, channel+"|"+recipient+"|"+text)
	return nil
}

type fixture struct {
	svc       *Service
	pool      *pgxpool.Pool
	accounts  *fakeAccounts
	providers *recordingProviders
	now       time.Time
}

var testSecretKey = []byte("0123456789abcdef0123456789abcdef")

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := testutil.StartPostgres(t)
	if err := db.Migrate(dsn, "notifications", migrations.FS, "."); err != nil {
		t.Fatal(err)
	}
	pool, err := db.ConnectN(context.Background(), dsn, "notifications", 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	box, err := secretbox.New(testSecretKey)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, accounts: &fakeAccounts{}, providers: &recordingProviders{fail: map[string]int{}}, now: time.Now().UTC()}
	f.svc = NewService(pool, f.accounts, f.providers, box, Config{AllowedHTTPHosts: []string{"127.0.0.1"}})
	return f
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func event(typ, aggType, aggID string, after any) events.Event {
	e := events.New(typ, aggType, aggID)
	e.After, _ = json.Marshal(after)
	return e
}

func approved(acc string) events.Event {
	return event("authorization.approved", "authorization", "au-"+acc, map[string]any{"account_id": acc, "amount_minor": 1234, "currency": "USD",
		"merchant_name": "Corner Bookstore", "status": "approved"})
}

func declined(acc, reason string, at time.Time) events.Event {
	e := event("authorization.declined", "authorization", "au-"+acc, map[string]any{"account_id": acc, "amount_minor": 99999, "currency": "USD",
		"merchant_name": "Corner Bookstore", "status": "declined", "decline_reason": reason})
	e.OccurredAt = at
	return e
}
