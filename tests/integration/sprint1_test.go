//go:build integration

// Package integration runs Sprint 1 end to end: real gRPC between services, the real HTTP
// gateway, Postgres (schema per service) and LocalStack SNS→SQS feeding the audit log.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/idempotency"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/testutil"
	"github.com/acme/acme-card/pkg/webhook"
	"github.com/acme/acme-card/services/accounts/accountstest"
	accountsmig "github.com/acme/acme-card/services/accounts/migrations"
	"github.com/acme/acme-card/services/audit/audittest"
	auditmig "github.com/acme/acme-card/services/audit/migrations"
	"github.com/acme/acme-card/services/billing/billingtest"
	billingmig "github.com/acme/acme-card/services/billing/migrations"
	"github.com/acme/acme-card/services/gateway/gatewaytest"
	gatewaymig "github.com/acme/acme-card/services/gateway/migrations"
	"github.com/acme/acme-card/services/ledger/ledgertest"
	ledgermig "github.com/acme/acme-card/services/ledger/migrations"
)

var (
	jwtSecret     = []byte("integration-secret-integration-secret")
	bankSecret    = []byte("bank-integration-secret")
	networkSecret = []byte("network-integration-secret")
)

type stack struct {
	url                                       string
	accounts, ledger, gateway, audit, billing *pgxpool.Pool
	statements                                billingtest.Store
}

// destinationKey is the integration stack's payout-destination key (dev only).
var destinationKey = bytes.Repeat([]byte{42}, 32)

// stackConfig holds the test-only knobs of the stack. Zero values mean the defaults used by the
// sprint suites.
type stackConfig struct {
	RateLimitPerMinute int                   // gateway rate limit per subject; 0 = 100,000
	Accounts           []accountstest.Option // accounts service options (e.g. velocity)
}

func startStack(t *testing.T) *stack {
	t.Helper()
	return startStackWith(t, stackConfig{})
}

func startStackWith(t *testing.T, cfg stackConfig) *stack {
	t.Helper()
	if cfg.RateLimitPerMinute == 0 {
		cfg.RateLimitPerMinute = 100_000
	}
	dsn := testutil.StartPostgres(t)
	aws := testutil.StartLocalStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	connect := func(schema string, files fs.FS) *pgxpool.Pool {
		if err := db.Migrate(dsn, schema, files, "."); err != nil {
			t.Fatalf("migrate %s: %v", schema, err)
		}
		p, err := db.Connect(ctx, dsn, schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	s := &stack{
		accounts: connect("accounts", accountsmig.FS),
		ledger:   connect("ledger", ledgermig.FS),
		gateway:  connect("gateway", gatewaymig.FS),
		audit:    connect("audit", auditmig.FS),
		billing:  connect("billing", billingmig.FS),
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub := events.NewSNSPublisher(aws.SNS, aws.TopicARN)
	for source, p := range map[string]*pgxpool.Pool{"accounts": s.accounts, "ledger": s.ledger, "gateway": s.gateway, "billing": s.billing} {
		go outbox.NewRelay(p, pub, 100, source).Run(ctx, 100*time.Millisecond, quiet)
	}
	go audittest.NewConsumer(aws.SQS, aws.QueueURL, s.audit).Run(ctx)

	serve := func(register func(*grpc.Server)) *grpc.ClientConn {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = grpcx.ServeListener(ctx, lis, register) }()
		conn, err := grpcx.Dial(lis.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	ledgerConn := serve(func(g *grpc.Server) { ledgerv1.RegisterLedgerServiceServer(g, ledgertest.NewServer(s.ledger)) })
	// accounts ↔ billing call each other: reserve billing's address first (gRPC dials lazily).
	billingLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	billingConn, err := grpcx.Dial(billingLis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { billingConn.Close() })
	accOpts := append([]accountstest.Option{accountstest.WithDestinationKey(destinationKey),
		accountstest.WithBilling(billingv1.NewBillingServiceClient(billingConn))}, cfg.Accounts...)
	accountsConn := serve(func(g *grpc.Server) {
		accountsv1.RegisterAccountsServiceServer(g, accountstest.NewServer(s.accounts, ledgerv1.NewLedgerServiceClient(ledgerConn), accOpts...))
	})

	billingSrv, store := billingtest.NewServer(s.billing, ledgerv1.NewLedgerServiceClient(ledgerConn), accountsv1.NewAccountsServiceClient(accountsConn))
	s.statements = store
	go func() {
		_ = grpcx.ServeListener(ctx, billingLis, func(g *grpc.Server) { billingv1.RegisterBillingServiceServer(g, billingSrv) })
	}()

	srv := httptest.NewServer(gatewaytest.NewHandler(gatewaytest.Deps{
		Accounts:         accountsv1.NewAccountsServiceClient(accountsConn),
		Billing:          billingv1.NewBillingServiceClient(billingConn),
		DevClock:         true,
		JWTSecret:        jwtSecret,
		BankSecret:       bankSecret,
		NetworkSecret:    networkSecret,
		WebhookTolerance: 5 * time.Minute,
		Rejections:       gatewaytest.NewRejectionLog(s.gateway),
		RequestTimeout:   10 * time.Second,
		Idempotency:      idempotency.NewStore(s.gateway, 24*time.Hour),
		Limiter:          gatewaytest.NewLimiter(s.gateway, cfg.RateLimitPerMinute),
		AuditSink:        s.gateway,
		Ready:            []func(context.Context) error{s.gateway.Ping},
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

type resp struct {
	status   int
	replayed bool
	body     map[string]any
	raw      string
}

func (s *stack) call(t *testing.T, method, path, token, key string, body any) resp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set(idempotency.HeaderKey, key)
	}
	return do(t, req)
}

func (s *stack) bank(t *testing.T, payload map[string]any, secret []byte) resp {
	t.Helper()
	b, _ := json.Marshal(payload)
	ts, sig := webhook.Sign(secret, b, time.Now())
	req, _ := http.NewRequest("POST", s.url+"/v1/webhooks/bank/deposits", bytes.NewReader(b))
	req.Header.Set(webhook.HeaderTimestamp, ts)
	req.Header.Set(webhook.HeaderSignature, sig)
	return do(t, req)
}

func do(t *testing.T, req *http.Request) resp {
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, replayed: res.Header.Get(idempotency.HeaderReplayed) == "true", raw: string(raw)}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func token(t *testing.T, sub string, accounts ...string) string {
	tok, err := auth.Sign(jwtSecret, sub, accounts, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func count(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// openAndDeposit opens an account and creates one deposit intent, returning their ids and an authorized token.
func (s *stack) openAndDeposit(t *testing.T, amount int64) (accID, depID, tok string) {
	sub := "cust-" + uuid.NewString()[:8]
	r := s.call(t, "POST", "/v1/accounts", token(t, sub), uuid.NewString(), map[string]any{"currency": "USD"})
	if r.status != 201 || r.body["state"] != "pending_collateral" {
		t.Fatalf("open: %d %s", r.status, r.raw)
	}
	accID = r.body["id"].(string)
	tok = token(t, sub, accID)
	r = s.call(t, "POST", "/v1/accounts/"+accID+"/collateral/deposits", tok, uuid.NewString(), map[string]any{"amount_minor": amount})
	if r.status != 201 {
		t.Fatalf("deposit: %d %s", r.status, r.raw)
	}
	return accID, r.body["id"].(string), tok
}

func TestSprint1EndToEnd(t *testing.T) {
	s := startStack(t)

	t.Run("happy path activates card with limit = collateral", func(t *testing.T) {
		acc, dep, tok := s.openAndDeposit(t, 75_000)
		r := s.bank(t, map[string]any{"deposit_id": dep, "external_ref": "bank-" + dep, "amount_minor": 75_000, "currency": "USD", "status": "settled"}, bankSecret)
		if r.status != 200 {
			t.Fatalf("webhook: %d %s", r.status, r.raw)
		}
		g := s.call(t, "GET", "/v1/accounts/"+acc, tok, "", nil)
		if g.body["state"] != "active" || g.body["credit_limit_minor"] != 75000.0 || g.body["available_credit_minor"] != 75000.0 {
			t.Fatalf("account: %s", g.raw)
		}
	})

	t.Run("50 concurrent creates with the same Idempotency-Key produce one deposit", func(t *testing.T) {
		acc, _, tok := s.openAndDeposit(t, 1_000)
		key := uuid.NewString()
		const n = 50
		out := make([]resp, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i] = s.call(t, "POST", "/v1/accounts/"+acc+"/collateral/deposits", tok, key, map[string]any{"amount_minor": 2_500})
			}(i)
		}
		close(start)
		wg.Wait()
		replays := 0
		for _, r := range out {
			if r.status != 201 || r.body["id"] != out[0].body["id"] {
				t.Fatalf("divergent response: %d %s vs %s", r.status, r.raw, out[0].raw)
			}
			if r.replayed {
				replays++
			}
		}
		if replays != n-1 {
			t.Fatalf("replays=%d want %d", replays, n-1)
		}
		if c := count(t, s.accounts, `SELECT count(*) FROM deposits WHERE account_id=$1 AND amount_minor=2500`, acc); c != 1 {
			t.Fatalf("deposits=%d want 1", c)
		}
		r := s.call(t, "POST", "/v1/accounts/"+acc+"/collateral/deposits", tok, key, map[string]any{"amount_minor": 9})
		if r.status != 422 {
			t.Fatalf("same key, different body: %d %s", r.status, r.raw)
		}
	})

	t.Run("50 concurrent bank webhooks settle once", func(t *testing.T) {
		acc, dep, _ := s.openAndDeposit(t, 40_000)
		payload := map[string]any{"deposit_id": dep, "external_ref": "bank-race-" + dep, "amount_minor": 40_000, "currency": "USD", "status": "settled"}
		const n = 50
		var wg sync.WaitGroup
		out := make([]resp, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) { defer wg.Done(); out[i] = s.bank(t, payload, bankSecret) }(i)
		}
		wg.Wait()
		fresh := 0
		for _, r := range out {
			if r.status != 200 {
				t.Fatalf("webhook: %d %s", r.status, r.raw)
			}
			if !r.replayed {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("fresh settlements=%d want 1", fresh)
		}
		if c := count(t, s.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1`, acc); c != 1 {
			t.Fatalf("journal entries=%d want 1", c)
		}
		if c := count(t, s.accounts, `SELECT count(*) FROM accounts WHERE id=$1 AND credit_limit_minor=40000 AND state='active'`, acc); c != 1 {
			t.Fatal("account not active with limit 40000")
		}
	})

	t.Run("security: foreign account, bad signature, unknown fields", func(t *testing.T) {
		acc, dep, _ := s.openAndDeposit(t, 500)
		if r := s.call(t, "GET", "/v1/accounts/"+acc, token(t, "intruder"), "", nil); r.status != 403 {
			t.Fatalf("foreign token: %d", r.status)
		}
		if r := s.bank(t, map[string]any{"deposit_id": dep, "external_ref": "x", "amount_minor": 500, "currency": "USD", "status": "settled"}, []byte("forged")); r.status != 401 {
			t.Fatalf("forged webhook: %d", r.status)
		}
		if r := s.call(t, "POST", "/v1/accounts", token(t, "c"), uuid.NewString(), map[string]any{"currency": "USD", "customer_id": "other"}); r.status != 400 {
			t.Fatalf("unknown field: %d", r.status)
		}
		if r := s.call(t, "POST", "/v1/accounts", token(t, "c2"), "", map[string]any{"currency": "USD"}); r.status != 400 {
			t.Fatalf("missing key: %d", r.status)
		}
		if c := count(t, s.gateway, `SELECT count(*) FROM webhook_rejections`); c != 1 {
			t.Fatalf("rejection rows=%d", c)
		}
	})

	t.Run("ledger is balanced and audit log receives every event append-only", func(t *testing.T) {
		if c := count(t, s.ledger, `SELECT count(*) FROM (SELECT journal_entry_id FROM postings GROUP BY 1
			HAVING sum(CASE WHEN side='debit' THEN amount_minor ELSE -amount_minor END) <> 0) x`); c != 0 {
			t.Fatalf("unbalanced entries=%d", c)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			pending := count(t, s.accounts, `SELECT count(*) FROM outbox WHERE published_at IS NULL`) +
				count(t, s.ledger, `SELECT count(*) FROM outbox WHERE published_at IS NULL`) +
				count(t, s.gateway, `SELECT count(*) FROM outbox WHERE published_at IS NULL`)
			total := count(t, s.accounts, `SELECT count(*) FROM outbox`) + count(t, s.ledger, `SELECT count(*) FROM outbox`) + count(t, s.gateway, `SELECT count(*) FROM outbox`)
			audited := count(t, s.audit, `SELECT count(*) FROM audit_events`)
			if pending == 0 && audited == total {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("audit lag: pending=%d audited=%d total=%d", pending, audited, total)
			}
			time.Sleep(250 * time.Millisecond)
		}
		for _, typ := range []string{"account.opened", "collateral.deposit_created", "collateral.confirmed", "card.activated", "ledger.journal_posted", "http.request"} {
			if count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1`, typ) == 0 {
				t.Errorf("no %s in audit", typ)
			}
		}
		if count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type='http.request' AND http_status IN (401,403,422)`) == 0 {
			t.Error("rejected requests must be audited with their status")
		}
		if _, err := s.audit.Exec(context.Background(), `DELETE FROM audit_events`); err == nil {
			t.Fatal("audit DELETE must fail")
		}
	})
}
