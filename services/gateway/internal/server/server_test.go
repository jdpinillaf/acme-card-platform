package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/webhook"
)

var jwtSecret = []byte("0123456789abcdef0123456789abcdef")

const accID = "0192f000-0000-7000-8000-000000000001"

type fakeAccounts struct {
	accountsv1.AccountsServiceClient
	lastCreate *accountsv1.CreateAccountRequest
	lastSettle *accountsv1.SettleDepositRequest
	err        error
}

func (f *fakeAccounts) CreateAccount(_ context.Context, r *accountsv1.CreateAccountRequest, _ ...grpc.CallOption) (*accountsv1.CreateAccountResponse, error) {
	f.lastCreate = r
	if f.err != nil {
		return nil, f.err
	}
	return &accountsv1.CreateAccountResponse{Account: &accountsv1.Account{Id: accID, CustomerId: r.CustomerId, Currency: r.Currency, State: accountsv1.AccountState_ACCOUNT_STATE_PENDING_COLLATERAL}}, nil
}

func (f *fakeAccounts) GetAccount(_ context.Context, r *accountsv1.GetAccountRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, State: accountsv1.AccountState_ACCOUNT_STATE_ACTIVE, CreditLimitMinor: 500, AvailableCreditMinor: 500, Currency: "USD"}}, nil
}

func (f *fakeAccounts) SettleDeposit(_ context.Context, r *accountsv1.SettleDepositRequest, _ ...grpc.CallOption) (*accountsv1.SettleDepositResponse, error) {
	f.lastSettle = r
	if f.err != nil {
		return nil, f.err
	}
	return &accountsv1.SettleDepositResponse{
		Deposit: &accountsv1.Deposit{Id: r.DepositId, AccountId: accID, Amount: &commonv1.Money{AmountMinor: r.AmountMinor, Currency: r.Currency}, Status: r.Status, ExternalRef: r.ExternalRef},
		Account: &accountsv1.Account{Id: accID, State: accountsv1.AccountState_ACCOUNT_STATE_ACTIVE},
	}, nil
}

type nopRejections struct{ n int }

func (r *nopRejections) Record(context.Context, string, string, time.Time) error { r.n++; return nil }

func newTestServer(acc *fakeAccounts, rej *nopRejections) http.Handler {
	return New(Deps{
		Accounts:         acc,
		JWTSecret:        jwtSecret,
		BankSecret:       []byte("bank"),
		WebhookTolerance: 5 * time.Minute,
		Rejections:       rej,
		RequestTimeout:   time.Second,
		DisableStateful:  true, // no idempotency/rate-limit/audit stores in unit tests
	})
}

func token(t *testing.T, accounts ...string) string {
	tok, err := auth.Sign(jwtSecret, "cust-1", accounts, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Error.Code
}

func TestHealthz(t *testing.T) {
	w := do(newTestServer(&fakeAccounts{}, &nopRejections{}), "GET", "/healthz", "", nil)
	if w.Code != 200 {
		t.Fatalf("%d", w.Code)
	}
}

func TestCreateAccountUsesTokenSubjectAndStrictJSON(t *testing.T) {
	acc := &fakeAccounts{}
	h := newTestServer(acc, &nopRejections{})
	hdr := map[string]string{"Authorization": token(t)}

	w := do(h, "POST", "/v1/accounts", `{"currency":"USD"}`, hdr)
	if w.Code != 201 || acc.lastCreate.CustomerId != "cust-1" {
		t.Fatalf("code=%d body=%s req=%+v", w.Code, w.Body.String(), acc.lastCreate)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["state"] != "pending_collateral" || body["id"] != accID {
		t.Fatalf("body=%v", body)
	}

	w = do(h, "POST", "/v1/accounts", `{"currency":"USD","customer_id":"someone-else"}`, hdr)
	if w.Code != 400 || errCode(t, w) != "invalid_body" {
		t.Fatalf("unknown field must be rejected: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/v1/accounts", `{"currency":"USD"}`, nil); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
}

func TestAccountRoutesEnforceAuthorizedAccounts(t *testing.T) {
	h := newTestServer(&fakeAccounts{}, &nopRejections{})
	if w := do(h, "GET", "/v1/accounts/"+accID, "", map[string]string{"Authorization": token(t, accID)}); w.Code != 200 {
		t.Fatalf("authorized: %d %s", w.Code, w.Body.String())
	}
	w := do(h, "GET", "/v1/accounts/"+accID, "", map[string]string{"Authorization": token(t, "other")})
	if w.Code != 403 || errCode(t, w) != "account_not_authorized" {
		t.Fatalf("foreign: %d %s", w.Code, w.Body.String())
	}
}

func TestGRPCErrorsMapToHTTP(t *testing.T) {
	cases := []struct {
		err  error
		code int
		name string
	}{
		{grpcx.Error(codes.AlreadyExists, "account_exists", "x"), 409, "account_exists"},
		{grpcx.Error(codes.FailedPrecondition, "account_closed", "x"), 409, "account_closed"},
		{grpcx.Error(codes.InvalidArgument, "invalid_currency", "x"), 400, "invalid_currency"},
		{grpcx.Error(codes.NotFound, "account_not_found", "x"), 404, "account_not_found"},
		{grpcx.Error(codes.DeadlineExceeded, "", "x"), 504, "timeout"},
		{grpcx.Error(codes.Unavailable, "", "x"), 503, "unavailable"},
		{grpcx.Error(codes.Internal, "", "secret detail"), 500, "internal"},
	}
	for _, c := range cases {
		h := newTestServer(&fakeAccounts{err: c.err}, &nopRejections{})
		w := do(h, "POST", "/v1/accounts", `{"currency":"USD"}`, map[string]string{"Authorization": token(t)})
		if w.Code != c.code || errCode(t, w) != c.name {
			t.Errorf("%v: got %d %s", c.err, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret detail") {
			t.Error("internal error details must not leak")
		}
	}
}

func signedWebhook(t *testing.T, body string, secret []byte, at time.Time) map[string]string {
	ts, sig := webhook.Sign(secret, []byte(body), at)
	return map[string]string{webhook.HeaderTimestamp: ts, webhook.HeaderSignature: sig}
}

func TestBankWebhookVerifiesSignatureAndRecordsRejections(t *testing.T) {
	acc := &fakeAccounts{}
	rej := &nopRejections{}
	h := newTestServer(acc, rej)
	body := `{"deposit_id":"0192f000-0000-7000-8000-0000000000aa","external_ref":"bank-1","amount_minor":500,"currency":"USD","status":"settled"}`

	w := do(h, "POST", "/v1/webhooks/bank/deposits", body, signedWebhook(t, body, []byte("bank"), time.Now()))
	if w.Code != 200 || acc.lastSettle == nil || acc.lastSettle.Status != accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED || acc.lastSettle.Meta.Actor != "bank" {
		t.Fatalf("valid webhook: %d %s %+v", w.Code, w.Body.String(), acc.lastSettle)
	}

	w = do(h, "POST", "/v1/webhooks/bank/deposits", body, signedWebhook(t, body, []byte("wrong"), time.Now()))
	if w.Code != 401 || errCode(t, w) != "bad_signature" {
		t.Fatalf("bad sig: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "POST", "/v1/webhooks/bank/deposits", body, signedWebhook(t, body, []byte("bank"), time.Now().Add(-10*time.Minute)))
	if w.Code != 401 || errCode(t, w) != "stale_timestamp" {
		t.Fatalf("stale: %d %s", w.Code, w.Body.String())
	}
	if rej.n != 2 {
		t.Fatalf("rejections recorded=%d want 2", rej.n)
	}

	bad := `{"deposit_id":"x","external_ref":"r","amount_minor":1,"currency":"USD","status":"maybe"}`
	if w := do(h, "POST", "/v1/webhooks/bank/deposits", bad, signedWebhook(t, bad, []byte("bank"), time.Now())); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
}

type countingSink struct{ n int }

func (c *countingSink) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	c.n++
	return pgconn.CommandTag{}, nil
}

type allowAll struct{}

func (allowAll) Allow(context.Context, string, time.Time) (bool, time.Duration, error) {
	return true, 0, nil
}

func TestRejectedWebhooksAreNotAuditedPerRequest(t *testing.T) {
	sink := &countingSink{}
	rej := &nopRejections{}
	h := New(Deps{Accounts: &fakeAccounts{}, JWTSecret: jwtSecret, BankSecret: []byte("bank"), WebhookTolerance: 5 * time.Minute,
		Rejections: rej, Limiter: allowAll{}, AuditSink: sink})
	body := `{"deposit_id":"d","external_ref":"r","amount_minor":1,"currency":"USD","status":"settled"}`
	for i := 0; i < 20; i++ {
		do(h, "POST", "/v1/webhooks/bank/deposits", body, signedWebhook(t, body, []byte("forged"), time.Now()))
	}
	if rej.n != 20 || sink.n != 0 {
		t.Fatalf("rejections=%d audit writes=%d; rejected webhooks must only hit the deduped rejection log", rej.n, sink.n)
	}
	do(h, "POST", "/v1/webhooks/bank/deposits", body, signedWebhook(t, body, []byte("bank"), time.Now()))
	if sink.n != 1 {
		t.Fatalf("accepted webhook must be audited, writes=%d", sink.n)
	}
}

type ipRecorder struct{ sources []string }

func (r *ipRecorder) Record(_ context.Context, source, _ string, _ time.Time) error {
	r.sources = append(r.sources, source)
	return nil
}

func TestClientIPIgnoresSpoofedHeaders(t *testing.T) {
	rec := &ipRecorder{}
	h := New(Deps{Accounts: &fakeAccounts{}, JWTSecret: jwtSecret, BankSecret: []byte("bank"), WebhookTolerance: time.Minute,
		Rejections: rec, DisableStateful: true, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	send := func(remote string, hdr map[string]string) {
		r := httptest.NewRequest("POST", "/v1/webhooks/bank/deposits", strings.NewReader("{}"))
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	// Direct client (not a trusted proxy): headers are ignored entirely.
	send("203.0.113.7:5555", map[string]string{"True-Client-IP": "1.1.1.1", "X-Forwarded-For": "2.2.2.2", "X-Real-IP": "3.3.3.3"})
	// Via trusted proxy: the right-most untrusted XFF hop is the client; client-supplied prefix is ignored.
	send("10.0.0.5:5555", map[string]string{"True-Client-IP": "1.1.1.1", "X-Forwarded-For": "9.9.9.9, 198.51.100.4"})
	want := []string{"203.0.113.7", "198.51.100.4"}
	if len(rec.sources) != 2 || rec.sources[0] != want[0] || rec.sources[1] != want[1] {
		t.Fatalf("sources=%v want %v", rec.sources, want)
	}
}
