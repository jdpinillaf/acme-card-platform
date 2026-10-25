package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	"github.com/acme/acme-card/pkg/auth"
)

const (
	stmtID   = "0192f000-0000-7000-8000-0000000000aa"
	chargeID = "0192f000-0000-7000-8000-0000000000bb"
)

type fakeBilling struct {
	billingv1.BillingServiceClient
	catchUps   int
	lastWaiver *billingv1.WaiveChargeRequest
}

func (f *fakeBilling) CatchUp(context.Context, *billingv1.CatchUpRequest, ...grpc.CallOption) (*billingv1.CatchUpResponse, error) {
	f.catchUps++
	return &billingv1.CatchUpResponse{StatementsGenerated: 1}, nil
}

func (f *fakeBilling) ListStatements(context.Context, *billingv1.ListStatementsRequest, ...grpc.CallOption) (*billingv1.ListStatementsResponse, error) {
	return &billingv1.ListStatementsResponse{Statements: []*billingv1.Statement{{Id: stmtID, AccountId: accID, Cycle: 1, NewBalanceMinor: 100000, MinimumDueMinor: 2500,
		PeriodStart: timestamppb.New(time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)), PeriodEnd: timestamppb.New(time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)),
		DueDate: timestamppb.New(time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)), Currency: "USD", Sha256: strings.Repeat("a", 64)}}}, nil
}

func (f *fakeBilling) GetStatement(_ context.Context, r *billingv1.GetStatementRequest, _ ...grpc.CallOption) (*billingv1.GetStatementResponse, error) {
	l, _ := f.ListStatements(context.Background(), nil)
	return &billingv1.GetStatementResponse{Statement: l.Statements[0], BodyJson: []byte(`{"cycle":1,"new_balance_minor":100000}`)}, nil
}

func (f *fakeBilling) GetSummary(context.Context, *billingv1.GetSummaryRequest, ...grpc.CallOption) (*billingv1.BillingSummary, error) {
	return &billingv1.BillingSummary{AccountId: accID, MinimumDueMinor: 2500, PastDueMinor: 2500, Dpd: 15, Stage: "past_due", Currency: "USD",
		AccountTime: timestamppb.New(time.Date(2027, 3, 18, 0, 0, 0, 0, time.UTC))}, nil
}

func (f *fakeBilling) ListCharges(context.Context, *billingv1.ListChargesRequest, ...grpc.CallOption) (*billingv1.ListChargesResponse, error) {
	return &billingv1.ListChargesResponse{Charges: []*billingv1.Charge{{Id: chargeID, Kind: "fee", Cycle: 1, AmountMinor: 2500, Status: "posted"}}}, nil
}

func (f *fakeBilling) WaiveCharge(_ context.Context, r *billingv1.WaiveChargeRequest, _ ...grpc.CallOption) (*billingv1.WaiveChargeResponse, error) {
	f.lastWaiver = r
	return &billingv1.WaiveChargeResponse{Charge: &billingv1.Charge{Id: r.ChargeId, Kind: "fee", AmountMinor: 2500, WaivedMinor: r.AmountMinor},
		Waiver: &billingv1.Waiver{Id: "w-1", ChargeId: r.ChargeId, AmountMinor: r.AmountMinor, Reason: r.Reason}}, nil
}

type fakeClockAccounts struct {
	fakeAccounts
	advanced []int32
}

func (f *fakeClockAccounts) AdvanceClock(_ context.Context, r *accountsv1.AdvanceClockRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	f.advanced = append(f.advanced, r.Days)
	return f.GetAccount(context.Background(), &accountsv1.GetAccountRequest{AccountId: r.AccountId})
}

func (f *fakeClockAccounts) GetAccount(_ context.Context, r *accountsv1.GetAccountRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, State: accountsv1.AccountState_ACCOUNT_STATE_ACTIVE, Currency: "USD",
		AccountTime: timestamppb.New(time.Date(2027, 3, 18, 0, 0, 0, 0, time.UTC)), ActivatedAt: timestamppb.New(time.Date(2027, 1, 10, 15, 0, 0, 0, time.UTC)),
		Delinquency: "past_due"}}, nil
}

func billingServer(acc accountsv1.AccountsServiceClient, b *fakeBilling, devClock bool) http.Handler {
	return New(Deps{Accounts: acc, Billing: b, DevClock: devClock, JWTSecret: jwtSecret, BankSecret: []byte("bank"), Rejections: &nopRejections{},
		RequestTimeout: time.Second, DisableStateful: true})
}

func supportToken(t *testing.T) string {
	tok, err := auth.SignWithRole(jwtSecret, "agent-1", nil, auth.RoleSupport, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok
}

func TestDevClockRouteAbsentWhenDisabled(t *testing.T) {
	h := billingServer(&fakeClockAccounts{}, &fakeBilling{}, false)
	if w := do(h, "POST", "/v1/dev/accounts/"+accID+"/clock/advance", `{"days":1}`, map[string]string{"Authorization": token(t, accID)}); w.Code != 404 {
		t.Fatalf("dev route without DEV_CLOCK_ENABLED: %d", w.Code)
	}
}

func TestDevClockAdvanceCatchesUp(t *testing.T) {
	acc, b := &fakeClockAccounts{}, &fakeBilling{}
	h := billingServer(acc, b, true)
	hdr := map[string]string{"Authorization": token(t, accID)}
	w := do(h, "POST", "/v1/dev/accounts/"+accID+"/clock/advance", `{"days":31}`, hdr)
	if w.Code != 200 || len(acc.advanced) != 1 || acc.advanced[0] != 31 || b.catchUps != 1 {
		t.Fatalf("advance: %d %s advanced=%v catchUps=%d", w.Code, w.Body.String(), acc.advanced, b.catchUps)
	}
	var body struct {
		Account map[string]any `json:"account"`
		Billing map[string]any `json:"billing"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Account["account_time"] == nil || body.Billing["stage"] != "past_due" || body.Billing["dpd"] != float64(15) {
		t.Fatalf("body: %s", w.Body.String())
	}
	if w := do(h, "POST", "/v1/dev/accounts/"+accID+"/clock/advance", `{"days":31}`, map[string]string{"Authorization": token(t, "other")}); w.Code != 403 {
		t.Fatalf("foreign account: %d", w.Code)
	}
}

func TestStrictJSONOnNewRoutes(t *testing.T) {
	h := billingServer(&fakeClockAccounts{}, &fakeBilling{}, true)
	if w := do(h, "POST", "/v1/dev/accounts/"+accID+"/clock/advance", `{"days":1,"x":1}`, map[string]string{"Authorization": token(t, accID)}); w.Code != 400 {
		t.Fatalf("advance: %d", w.Code)
	}
	if w := do(h, "POST", "/v1/support/accounts/"+accID+"/charges/"+chargeID+"/waivers", `{"amount_minor":1,"reason":"x","y":2}`,
		map[string]string{"Authorization": supportToken(t)}); w.Code != 400 {
		t.Fatalf("waiver: %d", w.Code)
	}
}

func TestSupportWaiverRequiresRole(t *testing.T) {
	b := &fakeBilling{}
	h := billingServer(&fakeClockAccounts{}, b, false)
	path := "/v1/support/accounts/" + accID + "/charges/" + chargeID + "/waivers"
	if w := do(h, "POST", path, `{"amount_minor":500,"reason":"goodwill"}`, map[string]string{"Authorization": token(t, accID)}); w.Code != 403 {
		t.Fatalf("customer token: %d", w.Code)
	}
	w := do(h, "POST", path, `{"amount_minor":500,"reason":"goodwill"}`, map[string]string{"Authorization": supportToken(t)})
	if w.Code != 201 || b.lastWaiver == nil || b.lastWaiver.AmountMinor != 500 || b.lastWaiver.Reason != "goodwill" || b.lastWaiver.Meta.Actor != "agent-1" {
		t.Fatalf("support waiver: %d %s %+v", w.Code, w.Body.String(), b.lastWaiver)
	}
}

func TestStatementRoutes(t *testing.T) {
	h := billingServer(&fakeClockAccounts{}, &fakeBilling{}, false)
	hdr := map[string]string{"Authorization": token(t, accID)}
	w := do(h, "GET", "/v1/accounts/"+accID+"/statements", "", hdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"minimum_due_minor":2500`) || !strings.Contains(w.Body.String(), `"due_date":"2027-03-03T00:00:00Z"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "GET", "/v1/accounts/"+accID+"/statements/"+stmtID, "", hdr)
	var detail struct {
		Statement map[string]any `json:"statement"`
		Document  map[string]any `json:"document"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if w.Code != 200 || detail.Document["new_balance_minor"] != float64(100000) || detail.Statement["id"] != stmtID {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/billing", "", hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"past_due_minor":2500`) {
		t.Fatalf("billing: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/charges", "", hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"fee"`) {
		t.Fatalf("charges: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/statements", "", map[string]string{"Authorization": token(t, "other")}); w.Code != 403 {
		t.Fatalf("foreign: %d", w.Code)
	}
}

func TestAccountJSONHasBillingFields(t *testing.T) {
	h := billingServer(&fakeClockAccounts{}, &fakeBilling{}, false)
	w := do(h, "GET", "/v1/accounts/"+accID, "", map[string]string{"Authorization": token(t, accID)})
	for _, want := range []string{`"account_time":"2027-03-18T00:00:00Z"`, `"activated_at":"2027-01-10T15:00:00Z"`, `"delinquency":"past_due"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("missing %s in %s", want, w.Body.String())
		}
	}
}
