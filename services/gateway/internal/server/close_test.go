package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/webhook"
)

const (
	payoutID  = "0192f000-0000-7000-8000-0000000000cc"
	attemptID = "0192f000-0000-7000-8000-0000000000ce"
)

type fakeClose struct {
	fakeAccounts
	lastClose  *accountsv1.CloseAccountRequest
	lastSettle *accountsv1.SettlePayoutRequest
	cancelled  bool
	retried    bool
}

func (f *fakeClose) closingAccount() *accountsv1.Account {
	return &accountsv1.Account{Id: accID, State: accountsv1.AccountState_ACCOUNT_STATE_CLOSING, Currency: "USD", CloseStage: "in_flight", DestinationLast4: "6789"}
}

func (f *fakeClose) payout() *accountsv1.Payout {
	return &accountsv1.Payout{Id: payoutID, AccountId: accID, Amount: &commonv1.Money{AmountMinor: 100000, Currency: "USD"},
		Status: accountsv1.PayoutStatus_PAYOUT_STATUS_PENDING, Attempt: 1, AttemptId: attemptID, DestinationLast4: "6789"}
}

func (f *fakeClose) CloseAccount(_ context.Context, r *accountsv1.CloseAccountRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.lastClose = r
	return &accountsv1.GetAccountResponse{Account: f.closingAccount()}, nil
}

func (f *fakeClose) CancelAccount(context.Context, *accountsv1.CancelAccountRequest, ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.cancelled = true
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: accID, State: accountsv1.AccountState_ACCOUNT_STATE_CANCELLED}}, nil
}

func (f *fakeClose) ListPayouts(context.Context, *accountsv1.ListPayoutsRequest, ...grpc.CallOption) (*accountsv1.ListPayoutsResponse, error) {
	return &accountsv1.ListPayoutsResponse{Payouts: []*accountsv1.Payout{f.payout()}}, nil
}

func (f *fakeClose) RetryPayout(context.Context, *accountsv1.RetryPayoutRequest, ...grpc.CallOption) (*accountsv1.PayoutResponse, error) {
	f.retried = true
	return &accountsv1.PayoutResponse{Payout: f.payout(), Account: f.closingAccount()}, nil
}

func (f *fakeClose) SettlePayout(_ context.Context, r *accountsv1.SettlePayoutRequest, _ ...grpc.CallOption) (*accountsv1.PayoutResponse, error) {
	f.lastSettle = r
	return &accountsv1.PayoutResponse{Payout: f.payout(), Account: f.closingAccount()}, nil
}

func (f *fakeClose) GetPayoutInstruction(_ context.Context, r *accountsv1.GetPayoutInstructionRequest, _ ...grpc.CallOption) (*accountsv1.PayoutInstruction, error) {
	return &accountsv1.PayoutInstruction{AttemptId: r.AttemptId, AmountMinor: 100000, Currency: "USD", BankCode: "BANK001", AccountNumber: "000123456789", HolderName: "Jane Doe"}, nil
}

const destJSON = `{"bank_code":"BANK001","account_number":"000123456789","holder_name":"Jane Doe"}`

func TestCustomerCloseAndCancel(t *testing.T) {
	f := &fakeClose{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), Rejections: &nopRejections{}, RequestTimeout: time.Second, DisableStateful: true})
	hdr := map[string]string{"Authorization": token(t, accID)}
	w := do(h, "POST", "/v1/accounts/"+accID+"/close", `{"destination":`+destJSON+`}`, hdr)
	if w.Code != 202 || f.lastClose.Destination.AccountNumber != "000123456789" || f.lastClose.Reason != "" {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "000123456789") || !strings.Contains(w.Body.String(), `"destination_last4":"6789"`) || !strings.Contains(w.Body.String(), `"state":"closing"`) {
		t.Fatalf("close response must be masked: %s", w.Body.String())
	}
	if w := do(h, "POST", "/v1/accounts/"+accID+"/close", `{"destination":`+destJSON+`,"x":1}`, hdr); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/accounts/"+accID+"/close", `{"destination":`+destJSON+`}`, map[string]string{"Authorization": token(t, "other")}); w.Code != 403 {
		t.Fatalf("foreign: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/accounts/"+accID+"/cancel", ``, hdr); w.Code != 200 || !f.cancelled || !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/payouts", "", hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"attempt_id":"`+attemptID+`"`) || strings.Contains(w.Body.String(), "000123456789") {
		t.Fatalf("payouts: %d %s", w.Code, w.Body.String())
	}
}

func TestSupportCloseAndRetry(t *testing.T) {
	f := &fakeClose{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), Rejections: &nopRejections{}, RequestTimeout: time.Second, DisableStateful: true})
	sup := map[string]string{"Authorization": supportToken(t)}
	if w := do(h, "POST", "/v1/support/accounts/"+accID+"/close", `{"destination":`+destJSON+`}`, sup); w.Code != 400 {
		t.Fatalf("support close without reason: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/support/accounts/"+accID+"/close", `{"destination":`+destJSON+`,"reason":"customer called"}`, sup); w.Code != 202 || f.lastClose.Reason != "customer called" || f.lastClose.Meta.Actor != "agent-1" {
		t.Fatalf("support close: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/v1/support/accounts/"+accID+"/close", `{"destination":`+destJSON+`,"reason":"x"}`, map[string]string{"Authorization": token(t, accID)}); w.Code != 403 {
		t.Fatalf("customer on the support route: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/support/accounts/"+accID+"/payouts/"+payoutID+"/retry", ``, sup); w.Code != 201 || !f.retried {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
}

func signedBank(method, path, body string) map[string]string {
	ts, sig := webhook.SignRequest([]byte("bank"), method, path, []byte(body), time.Now())
	return map[string]string{webhook.HeaderTimestamp: ts, webhook.HeaderSignature: sig}
}

func TestBankPayoutInstructionAndWebhook(t *testing.T) {
	f := &fakeClose{}
	h := New(Deps{Accounts: f, JWTSecret: jwtSecret, BankSecret: []byte("bank"), Rejections: &nopRejections{}, RequestTimeout: time.Second, WebhookTolerance: 5 * time.Minute, DisableStateful: true})
	ipath := "/v1/bank/payout-instructions/" + attemptID
	if w := do(h, "GET", ipath, "", nil); w.Code != 401 {
		t.Fatalf("unsigned instruction: %d", w.Code)
	}
	if w := do(h, "GET", ipath, "", signedBank("GET", "/v1/bank/payout-instructions/other", "")); w.Code != 401 {
		t.Fatalf("signature bound to another path: %d", w.Code)
	}
	w := do(h, "GET", ipath, "", signedBank("GET", ipath, ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"account_number":"000123456789"`) {
		t.Fatalf("instruction: %d %s", w.Code, w.Body.String())
	}
	path := "/v1/webhooks/bank/payouts"
	body := `{"attempt_id":"` + attemptID + `","amount_minor":100000,"currency":"USD","destination_last4":"6789","status":"settled"}`
	if w := do(h, "POST", path, body, nil); w.Code != 401 {
		t.Fatalf("unsigned webhook: %d", w.Code)
	}
	w = do(h, "POST", path, body, signedBank("POST", path, body))
	if w.Code != 200 || f.lastSettle.Status != accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED || f.lastSettle.DestinationLast4 != "6789" {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	bad := strings.Replace(body, `"settled"`, `"bogus"`, 1)
	if w := do(h, "POST", path, bad, signedBank("POST", path, bad)); w.Code != 400 {
		t.Fatalf("bad status: %d", w.Code)
	}
}

func TestPayoutMismatchIs422(t *testing.T) {
	for _, reason := range []string{"destination_mismatch", "amount_mismatch"} {
		if !semantic422[reason] {
			t.Errorf("%s must map to 422", reason)
		}
	}
}
