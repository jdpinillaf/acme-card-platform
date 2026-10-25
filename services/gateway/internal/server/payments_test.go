package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/webhook"
)

const payID = "0192f000-0000-7000-8000-0000000000dd"

type fakePay struct {
	fakeTx
	lastCreate *accountsv1.CreatePaymentRequest
	lastSettle *accountsv1.SettlePaymentRequest
	lastReturn *accountsv1.ReturnPaymentRequest
}

func (f *fakePay) payResp(st accountsv1.PaymentStatus) *accountsv1.PaymentResponse {
	return &accountsv1.PaymentResponse{
		Payment: &accountsv1.Payment{Id: payID, AccountId: accID, Amount: &commonv1.Money{AmountMinor: 500, Currency: "USD"}, Status: st, ExternalRef: "b-1"},
		Account: &accountsv1.Account{Id: accID, State: accountsv1.AccountState_ACCOUNT_STATE_ACTIVE, OutstandingMinor: 100, CreditBalanceMinor: 0, RestrictedReason: "payment_returned"},
	}
}

func (f *fakePay) CreatePayment(_ context.Context, r *accountsv1.CreatePaymentRequest, _ ...grpc.CallOption) (*accountsv1.PaymentResponse, error) {
	f.lastCreate = r
	return f.payResp(accountsv1.PaymentStatus_PAYMENT_STATUS_PENDING), nil
}

func (f *fakePay) SettlePayment(_ context.Context, r *accountsv1.SettlePaymentRequest, _ ...grpc.CallOption) (*accountsv1.PaymentResponse, error) {
	f.lastSettle = r
	return f.payResp(r.Status), nil
}

func (f *fakePay) ReturnPayment(_ context.Context, r *accountsv1.ReturnPaymentRequest, _ ...grpc.CallOption) (*accountsv1.PaymentResponse, error) {
	f.lastReturn = r
	return f.payResp(accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNED), nil
}

func (f *fakePay) ListPayments(context.Context, *accountsv1.ListPaymentsRequest, ...grpc.CallOption) (*accountsv1.ListPaymentsResponse, error) {
	return &accountsv1.ListPaymentsResponse{Payments: []*accountsv1.Payment{f.payResp(accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED).Payment}}, nil
}

func bankSigned(path, body string) map[string]string {
	ts, sig := webhook.SignRequest([]byte("bank"), "POST", path, []byte(body), time.Now())
	return map[string]string{webhook.HeaderTimestamp: ts, webhook.HeaderSignature: sig}
}

func TestCreatePaymentRoute(t *testing.T) {
	f := &fakePay{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), NetworkSecret: networkSecret, WebhookTolerance: 300e9, Rejections: &nopRejections{}, RequestTimeout: 1e9, DisableStateful: true})
	hdr := map[string]string{"Authorization": token(t, accID)}
	if w := do(h, "POST", "/v1/accounts/"+accID+"/payments", `{"amount_minor":500,"extra":1}`, hdr); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	w := do(h, "POST", "/v1/accounts/"+accID+"/payments", `{"amount_minor":500}`, hdr)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 201 || f.lastCreate.AmountMinor != 500 || body["payment"].(map[string]any)["status"] != "pending" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/v1/accounts/"+accID+"/payments", `{"amount_minor":500}`, map[string]string{"Authorization": token(t, "other")}); w.Code != 403 {
		t.Fatalf("foreign: %d", w.Code)
	}
	w = do(h, "GET", "/v1/accounts/"+accID+"/payments", "", hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"payments":[`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
}

func TestPaymentWebhooksRequireBoundSignature(t *testing.T) {
	f := &fakePay{}
	rej := &nopRejections{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), NetworkSecret: networkSecret, WebhookTolerance: 300e9, Rejections: rej, RequestTimeout: 1e9, DisableStateful: true})
	body := `{"payment_id":"` + payID + `","external_ref":"b-1","amount_minor":500,"currency":"USD","status":"settled"}`
	if w := do(h, "POST", "/v1/webhooks/bank/payments", body, signedWebhookHeaders(body, []byte("bank"))); w.Code != 401 {
		t.Fatalf("deposit-style (unbound) signature must be rejected: %d", w.Code)
	}
	w := do(h, "POST", "/v1/webhooks/bank/payments", body, bankSigned("/v1/webhooks/bank/payments", body))
	if w.Code != 200 || f.lastSettle.Status != accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED || f.lastSettle.Meta.Actor != "bank" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	fail := strings.Replace(body, `"settled"`, `"failed"`, 1)
	if w := do(h, "POST", "/v1/webhooks/bank/payments", fail, bankSigned("/v1/webhooks/bank/payments", fail)); w.Code != 200 || f.lastSettle.Status != accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED {
		t.Fatalf("failed: %d", w.Code)
	}
	bad := strings.Replace(body, `"settled"`, `"maybe"`, 1)
	if w := do(h, "POST", "/v1/webhooks/bank/payments", bad, bankSigned("/v1/webhooks/bank/payments", bad)); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
}

func TestReturnRouteShape(t *testing.T) {
	f := &fakePay{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), NetworkSecret: networkSecret, WebhookTolerance: 300e9, Rejections: &nopRejections{}, RequestTimeout: 1e9, DisableStateful: true})
	path := "/v1/webhooks/bank/payments/" + payID + "/return"
	body := `{"return_ref":"r-1","amount_minor":500}`
	w := do(h, "POST", path, body, bankSigned(path, body))
	if w.Code != 200 || f.lastReturn.PaymentId != payID || f.lastReturn.ReturnRef != "r-1" || !strings.Contains(w.Body.String(), `"status":"returned"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAccountJSONHasBalances(t *testing.T) {
	f := &fakePay{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), NetworkSecret: networkSecret, WebhookTolerance: 300e9, Rejections: &nopRejections{}, RequestTimeout: 1e9, DisableStateful: true})
	w := do(h, "POST", "/v1/accounts/"+accID+"/payments", `{"amount_minor":500}`, map[string]string{"Authorization": token(t, accID)})
	for _, want := range []string{`"outstanding_minor":100`, `"credit_balance_minor":0`, `"restricted_reason":"payment_returned"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s in %s", want, w.Body.String())
		}
	}
}

func TestDeclinePaymentReturnedJSON(t *testing.T) {
	f := &fakeTx{authStatus: accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED, reason: commonv1.DeclineReason_DECLINE_REASON_PAYMENT_RETURNED}
	w := do(newTxServer(f, &nopRejections{}), "POST", "/v1/network/authorizations", authBody, netSigned("/v1/network/authorizations", authBody, networkSecret))
	if !strings.Contains(w.Body.String(), `"decline_reason":"payment_returned"`) {
		t.Fatalf("%s", w.Body.String())
	}
}
