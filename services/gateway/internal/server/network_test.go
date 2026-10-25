package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/webhook"
)

var networkSecret = []byte("network-secret")

const authID = "0192f000-0000-7000-8000-0000000000bb"

type fakeTx struct {
	fakeAccounts
	lastAuth     *accountsv1.AuthorizeRequest
	lastLimit    *accountsv1.SetPurchaseLimitRequest
	frozen       *bool
	authStatus   accountsv1.AuthorizationStatus
	reason       commonv1.DeclineReason
	replayed     bool
	transactions int
}

func (f *fakeTx) auth() *accountsv1.Authorization {
	return &accountsv1.Authorization{Id: authID, AccountId: accID, NetworkRef: "n-1", Amount: &commonv1.Money{AmountMinor: 300, Currency: "USD"},
		Merchant: &accountsv1.Merchant{Id: "m", Name: "Shop", Mcc: "5814"}, Status: f.authStatus, DeclineReason: f.reason,
		CreatedAt: timestamppb.New(time.Unix(1700000000, 0))}
}

func (f *fakeTx) Authorize(_ context.Context, r *accountsv1.AuthorizeRequest, _ ...grpc.CallOption) (*accountsv1.AuthorizationResponse, error) {
	f.lastAuth = r
	if f.err != nil {
		return nil, f.err
	}
	return &accountsv1.AuthorizationResponse{Authorization: f.auth(), Replayed: f.replayed}, nil
}

func (f *fakeTx) Capture(context.Context, *accountsv1.CaptureRequest, ...grpc.CallOption) (*accountsv1.AuthorizationResponse, error) {
	return &accountsv1.AuthorizationResponse{Authorization: f.auth(), Replayed: f.replayed}, nil
}

func (f *fakeTx) Reverse(context.Context, *accountsv1.ReverseRequest, ...grpc.CallOption) (*accountsv1.AuthorizationResponse, error) {
	return &accountsv1.AuthorizationResponse{Authorization: f.auth(), Replayed: f.replayed}, nil
}

func (f *fakeTx) Refund(_ context.Context, r *accountsv1.RefundRequest, _ ...grpc.CallOption) (*accountsv1.RefundResponse, error) {
	return &accountsv1.RefundResponse{Refund: &accountsv1.Refund{Id: "rf", AuthorizationId: authID, NetworkRef: r.NetworkRef,
		Amount: &commonv1.Money{AmountMinor: r.AmountMinor, Currency: "USD"}}, Authorization: f.auth()}, nil
}

func (f *fakeTx) SetFrozen(_ context.Context, r *accountsv1.SetFrozenRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.frozen = &r.Frozen
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, Frozen: r.Frozen, State: accountsv1.AccountState_ACCOUNT_STATE_ACTIVE}}, nil
}

func (f *fakeTx) SetPurchaseLimit(_ context.Context, r *accountsv1.SetPurchaseLimitRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.lastLimit = r
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, PurchaseLimitMinor: r.PurchaseLimitMinor}}, nil
}

func (f *fakeTx) ListTransactions(context.Context, *accountsv1.ListTransactionsRequest, ...grpc.CallOption) (*accountsv1.ListTransactionsResponse, error) {
	f.transactions++
	return &accountsv1.ListTransactionsResponse{Authorizations: []*accountsv1.Authorization{f.auth()}, NextPageToken: "next"}, nil
}

func newTxServer(f *fakeTx, rej *nopRejections) http.Handler {
	return New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), NetworkSecret: networkSecret,
		WebhookTolerance: 5 * time.Minute, Rejections: rej, RequestTimeout: time.Second, DisableStateful: true})
}

const authBody = `{"network_ref":"n-1","card_id":"` + accID + `","amount_minor":300,"currency":"USD","merchant":{"id":"m","name":"Shop","mcc":"5814"}}`

// netSigned signs a network request (method + path bound into the MAC).
func netSigned(path, body string, secret []byte) map[string]string {
	ts, sig := webhook.SignRequest(secret, "POST", path, []byte(body), time.Now())
	return map[string]string{webhook.HeaderTimestamp: ts, webhook.HeaderSignature: sig}
}

func TestNetworkAuthorizeDeclineIs201(t *testing.T) {
	f := &fakeTx{authStatus: accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED, reason: commonv1.DeclineReason_DECLINE_REASON_CARD_FROZEN}
	w := do(newTxServer(f, &nopRejections{}), "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, networkSecret))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 201 || body["status"] != "declined" || body["decline_reason"] != "card_frozen" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if f.lastAuth.Merchant.Mcc != "5814" || f.lastAuth.Meta.Actor != "network" {
		t.Fatalf("%+v", f.lastAuth)
	}
	m := body["merchant"].(map[string]any)
	if m["name"] != "Shop" || body["amount_minor"] != 300.0 {
		t.Fatalf("shape: %s", w.Body.String())
	}
}

func TestNetworkApprovedOmitsDeclineReason(t *testing.T) {
	f := &fakeTx{authStatus: accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED}
	w := do(newTxServer(f, &nopRejections{}), "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, networkSecret))
	if w.Code != 201 || strings.Contains(w.Body.String(), "decline_reason") || !strings.Contains(w.Body.String(), `"status":"approved"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestNetworkRoutesRequireNetworkSignature(t *testing.T) {
	rej := &nopRejections{}
	h := newTxServer(&fakeTx{}, rej)
	w := do(h, "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, []byte("bank")))
	if w.Code != 401 || errCode(t, w) != "bad_signature" || rej.n != 1 {
		t.Fatalf("%d %s rej=%d", w.Code, w.Body.String(), rej.n)
	}
	for _, p := range []string{"/capture", "/reversal", "/refunds"} {
		body := `{"network_ref":"x","amount_minor":1}`
		if p == "/reversal" {
			body = `{"network_ref":"x"}`
		}
		if w := do(h, "POST", "/v1/network/authorizations/"+authID+p, body, nil); w.Code != 401 {
			t.Errorf("%s unsigned: %d", p, w.Code)
		}
	}
}

func TestNetworkStrictJSON(t *testing.T) {
	body := strings.Replace(authBody, `"currency":"USD"`, `"currency":"USD","extra":1`, 1)
	w := do(newTxServer(&fakeTx{}, &nopRejections{}), "POST", "/v1/network/authorizations", body, netSigned("/v1/network/authorizations", body, networkSecret))
	if w.Code != 400 {
		t.Fatalf("%d", w.Code)
	}
}

func TestNetworkFollowUpStatusCodesAndReplayHeader(t *testing.T) {
	f := &fakeTx{authStatus: accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_REVERSED, replayed: true}
	h := newTxServer(f, &nopRejections{})
	cases := []struct {
		path, body string
		code       int
	}{
		{"/capture", `{"network_ref":"c-1","amount_minor":200}`, 201},
		{"/reversal", `{"network_ref":"r-1"}`, 200},
		{"/refunds", `{"network_ref":"rf-1","amount_minor":50}`, 201},
	}
	for _, c := range cases {
		w := do(h, "POST", "/v1/network/authorizations/"+authID+c.path, c.body, netSigned("/v1/network/authorizations/"+authID+c.path, c.body, networkSecret))
		if w.Code != c.code {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body.String())
		}
		if c.path != "/refunds" && w.Header().Get("Idempotency-Replayed") != "true" {
			t.Errorf("%s: missing replay header", c.path)
		}
	}
	w := do(h, "POST", "/v1/network/authorizations/"+authID+"/refunds", `{"network_ref":"rf-1","amount_minor":50}`, netSigned("/v1/network/authorizations/"+authID+"/refunds", `{"network_ref":"rf-1","amount_minor":50}`, networkSecret))
	if !strings.Contains(w.Body.String(), `"refund"`) || !strings.Contains(w.Body.String(), `"authorization"`) {
		t.Fatalf("refund shape: %s", w.Body.String())
	}
}

func TestFreezeRequiresAuthorizedAccount(t *testing.T) {
	f := &fakeTx{}
	h := newTxServer(f, &nopRejections{})
	if w := do(h, "POST", "/v1/accounts/"+accID+"/freeze", "", map[string]string{"Authorization": token(t, "other")}); w.Code != 403 {
		t.Fatalf("foreign: %d", w.Code)
	}
	w := do(h, "POST", "/v1/accounts/"+accID+"/freeze", "", map[string]string{"Authorization": token(t, accID)})
	if w.Code != 200 || f.frozen == nil || !*f.frozen || !strings.Contains(w.Body.String(), `"frozen":true`) {
		t.Fatalf("freeze: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "POST", "/v1/accounts/"+accID+"/unfreeze", "", map[string]string{"Authorization": token(t, accID)})
	if w.Code != 200 || *f.frozen {
		t.Fatalf("unfreeze: %d", w.Code)
	}
}

func TestLimitsBody(t *testing.T) {
	f := &fakeTx{}
	h := newTxServer(f, &nopRejections{})
	hdr := map[string]string{"Authorization": token(t, accID)}
	if w := do(h, "PUT", "/v1/accounts/"+accID+"/limits", `{"purchase_limit_minor":500,"x":1}`, hdr); w.Code != 400 {
		t.Fatalf("extra field: %d", w.Code)
	}
	w := do(h, "PUT", "/v1/accounts/"+accID+"/limits", `{"purchase_limit_minor":500}`, hdr)
	if w.Code != 200 || f.lastLimit.PurchaseLimitMinor != 500 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestTransactionsListShape(t *testing.T) {
	f := &fakeTx{authStatus: accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED}
	w := do(newTxServer(f, &nopRejections{}), "GET", "/v1/accounts/"+accID+"/transactions?page_size=5", "", map[string]string{"Authorization": token(t, accID)})
	var body struct {
		Transactions []map[string]any `json:"transactions"`
		Next         string           `json:"next_page_token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || len(body.Transactions) != 1 || body.Transactions[0]["status"] != "captured" || body.Next != "next" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

var _ = httptest.NewRecorder

func TestNetworkSignatureIsBoundToPath(t *testing.T) {
	rej := &nopRejections{}
	h := newTxServer(&fakeTx{}, rej)
	body := `{"network_ref":"x","amount_minor":1}`
	sig := netSigned("/v1/network/authorizations/"+authID+"/capture", body, networkSecret)
	if w := do(h, "POST", "/v1/network/authorizations/"+authID+"/capture", body, sig); w.Code != 201 {
		t.Fatalf("signature must verify on its own path, got %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/v1/network/authorizations/"+authID+"/refunds", body, sig); w.Code != 401 {
		t.Fatalf("capture signature replayed on refunds must be 401, got %d", w.Code)
	}
	other := "0192f000-0000-7000-8000-0000000000cc"
	if w := do(h, "POST", "/v1/network/authorizations/"+other+"/capture", body, sig); w.Code != 401 {
		t.Fatalf("signature replayed on another authorization must be 401, got %d", w.Code)
	}
}

func TestNetworkRefReuseIs422(t *testing.T) {
	f := &fakeTx{}
	f.err = grpcx.Error(codes.AlreadyExists, "network_ref_reuse", "x")
	w := do(newTxServer(f, &nopRejections{}), "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, networkSecret))
	if w.Code != 422 || errCode(t, w) != "network_ref_reuse" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestNetworkJSONMapsPendingAndFailed(t *testing.T) {
	for st, want := range map[accountsv1.AuthorizationStatus]string{
		accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_PENDING: "pending",
		accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED:  "failed",
	} {
		f := &fakeTx{authStatus: st}
		w := do(newTxServer(f, &nopRejections{}), "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, networkSecret))
		if !strings.Contains(w.Body.String(), `"status":"`+want+`"`) {
			t.Errorf("%s: %s", want, w.Body.String())
		}
	}
}
