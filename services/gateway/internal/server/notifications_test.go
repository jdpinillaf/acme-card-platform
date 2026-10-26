package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

type notifAccounts struct{ fakeAccounts }

func (f *notifAccounts) GetAccount(_ context.Context, r *accountsv1.GetAccountRequest, _ ...grpc.CallOption) (*accountsv1.GetAccountResponse, error) {
	return &accountsv1.GetAccountResponse{Account: &accountsv1.Account{Id: r.AccountId, CustomerId: "cust-1", Currency: "USD"}}, nil
}

type fakeNotifications struct {
	notificationsv1.NotificationsServiceClient
	lastSet      *notificationsv1.SetPreferenceRequest
	lastCreate   *notificationsv1.CreateSubscriptionRequest
	lastInbox    *notificationsv1.ListInboxRequest
	disabled     string
	prefCustomer string
}

func (f *fakeNotifications) GetPreferences(_ context.Context, r *notificationsv1.GetPreferencesRequest, _ ...grpc.CallOption) (*notificationsv1.GetPreferencesResponse, error) {
	f.prefCustomer = r.CustomerId
	return &notificationsv1.GetPreferencesResponse{Preferences: []*notificationsv1.Preference{{Type: "late_fee", Push: true, Sms: true, Email: true, Mandatory: true}}}, nil
}

func (f *fakeNotifications) SetPreference(_ context.Context, r *notificationsv1.SetPreferenceRequest, _ ...grpc.CallOption) (*notificationsv1.Preference, error) {
	f.lastSet = r
	if r.Type == "late_fee" && !r.Push && !r.Sms && !r.Email {
		return nil, grpcx.Error(codes.FailedPrecondition, "mandatory_notification", "this notification is mandatory")
	}
	return &notificationsv1.Preference{Type: r.Type, Push: r.Push, Sms: r.Sms, Email: r.Email}, nil
}

func (f *fakeNotifications) ListInbox(_ context.Context, r *notificationsv1.ListInboxRequest, _ ...grpc.CallOption) (*notificationsv1.ListInboxResponse, error) {
	f.lastInbox = r
	return &notificationsv1.ListInboxResponse{Items: []*notificationsv1.InboxItem{{Id: 7, Channel: "push", Type: "purchase_approved", Text: "Purchase approved",
		CreatedAt: timestamppb.New(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))}}}, nil
}

func (f *fakeNotifications) CreateSubscription(_ context.Context, r *notificationsv1.CreateSubscriptionRequest, _ ...grpc.CallOption) (*notificationsv1.CreateSubscriptionResponse, error) {
	f.lastCreate = r
	return &notificationsv1.CreateSubscriptionResponse{Subscription: &notificationsv1.Subscription{Id: "sub-1", Name: r.Name, Url: r.Url, EventTypes: r.EventTypes, Active: true,
		CreatedAt: timestamppb.Now()}, Secret: "deadbeef"}, nil
}

func (f *fakeNotifications) ListSubscriptions(context.Context, *notificationsv1.ListSubscriptionsRequest, ...grpc.CallOption) (*notificationsv1.ListSubscriptionsResponse, error) {
	return &notificationsv1.ListSubscriptionsResponse{Subscriptions: []*notificationsv1.Subscription{{Id: "sub-1", Name: "crm", Active: true}}}, nil
}

func (f *fakeNotifications) DisableSubscription(_ context.Context, r *notificationsv1.DisableSubscriptionRequest, _ ...grpc.CallOption) (*notificationsv1.Subscription, error) {
	f.disabled = r.Id
	return &notificationsv1.Subscription{Id: r.Id, Active: false, DisabledAt: timestamppb.Now()}, nil
}

func (f *fakeNotifications) ListDeliveries(_ context.Context, r *notificationsv1.ListDeliveriesRequest, _ ...grpc.CallOption) (*notificationsv1.ListDeliveriesResponse, error) {
	return &notificationsv1.ListDeliveriesResponse{Deliveries: []*notificationsv1.Delivery{{Id: "d1", Status: "delivered", Attempts: 1}}}, nil
}

func notifServer(n *fakeNotifications) http.Handler {
	return New(Deps{Accounts: &notifAccounts{}, Notifications: n, JWTSecret: jwtSecret, Rejections: &nopRejections{}, RequestTimeout: time.Second, DisableStateful: true})
}

func TestNotificationPreferenceRoutes(t *testing.T) {
	n := &fakeNotifications{}
	h := notifServer(n)
	hdr := map[string]string{"Authorization": token(t, accID)}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/notification-preferences", "", hdr); w.Code != 200 || !strings.Contains(w.Body.String(), `"mandatory":true`) || n.prefCustomer != "cust-1" {
		t.Fatalf("get: %d %s %q", w.Code, w.Body.String(), n.prefCustomer)
	}
	if w := do(h, "PUT", "/v1/accounts/"+accID+"/notification-preferences", `{"type":"purchase_approved","push":true,"sms":false,"email":false}`, hdr); w.Code != 200 ||
		n.lastSet.CustomerId != "cust-1" || !n.lastSet.Push || n.lastSet.Sms {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "PUT", "/v1/accounts/"+accID+"/notification-preferences", `{"type":"late_fee","push":false,"sms":false,"email":false}`, hdr); w.Code != 422 ||
		!strings.Contains(w.Body.String(), "mandatory_notification") {
		t.Fatalf("mandatory: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "PUT", "/v1/accounts/"+accID+"/notification-preferences", `{"type":"x","push":true,"extra":1}`, hdr); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/notifications?after=5", "", hdr); w.Code != 200 || n.lastInbox.AfterId != 5 || n.lastInbox.AccountId != accID ||
		!strings.Contains(w.Body.String(), `"id":7`) {
		t.Fatalf("inbox: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/accounts/"+accID+"/notifications?after=x", "", hdr); w.Code != 400 {
		t.Fatalf("bad after: %d", w.Code)
	}
	// someone else's account
	other := map[string]string{"Authorization": token(t, "0192f000-0000-7000-8000-000000000999")}
	for _, path := range []string{"/notification-preferences", "/notifications"} {
		if w := do(h, "GET", "/v1/accounts/"+accID+path, "", other); w.Code != 403 {
			t.Fatalf("%s for another account: %d", path, w.Code)
		}
	}
}

func TestWebhookSubscriptionRoutes(t *testing.T) {
	n := &fakeNotifications{}
	h := notifServer(n)
	sup := map[string]string{"Authorization": supportToken(t)}
	w := do(h, "POST", "/v1/support/webhook-subscriptions", `{"name":"crm","url":"https://partner.example/h","event_types":["authorization.approved"]}`, sup)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"secret":"deadbeef"`) || n.lastCreate.Meta.GetActor() != "agent-1" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/v1/support/webhook-subscriptions", `{"name":"crm","url":"u","event_types":[],"x":1}`, sup); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	if w := do(h, "GET", "/v1/support/webhook-subscriptions", "", sup); w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "DELETE", "/v1/support/webhook-subscriptions/sub-1", "", sup); w.Code != 200 || n.disabled != "sub-1" {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/v1/support/webhook-subscriptions/sub-1/deliveries", "", sup); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"delivered"`) {
		t.Fatalf("deliveries: %d %s", w.Code, w.Body.String())
	}
	cust := map[string]string{"Authorization": token(t, accID)}
	for _, c := range [][2]string{{"POST", "/v1/support/webhook-subscriptions"}, {"GET", "/v1/support/webhook-subscriptions"}, {"DELETE", "/v1/support/webhook-subscriptions/sub-1"},
		{"GET", "/v1/support/webhook-subscriptions/sub-1/deliveries"}} {
		if w := do(h, c[0], c[1], `{}`, cust); w.Code != 403 {
			t.Fatalf("customer %s %s: %d", c[0], c[1], w.Code)
		}
	}
}
