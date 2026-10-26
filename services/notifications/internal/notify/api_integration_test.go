//go:build integration

package notify

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

func TestPreferencesDefaultsAndMandatory422(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.svc.GetPreferences(ctx, &notificationsv1.GetPreferencesRequest{CustomerId: "cust-1"})
	if err != nil || len(res.Preferences) != 9 {
		t.Fatalf("defaults: %v %+v", err, res)
	}
	for _, p := range res.Preferences {
		if !p.Push || !p.Sms || !p.Email || p.Mandatory != (p.Type == "late_fee" || p.Type == "card_blocked" || p.Type == "suspected_fraud") {
			t.Fatalf("default %+v", p)
		}
	}
	_, err = f.svc.SetPreference(ctx, &notificationsv1.SetPreferenceRequest{CustomerId: "cust-1", Type: "late_fee"})
	if status.Code(err) != codes.FailedPrecondition || grpcx.Reason(err) != "mandatory_notification" {
		t.Fatalf("mandatory all off: %v", err)
	}
	if _, err := f.svc.SetPreference(ctx, &notificationsv1.SetPreferenceRequest{CustomerId: "cust-1", Type: "nope", Push: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown type: %v", err)
	}
	if p, err := f.svc.SetPreference(ctx, &notificationsv1.SetPreferenceRequest{CustomerId: "cust-1", Type: "purchase_approved", Push: true}); err != nil || p.Sms || !p.Push {
		t.Fatalf("set: %v %+v", err, p)
	}
	res, _ = f.svc.GetPreferences(ctx, &notificationsv1.GetPreferencesRequest{CustomerId: "cust-1"})
	for _, p := range res.Preferences {
		if p.Type == "purchase_approved" && (p.Sms || p.Email || !p.Push) {
			t.Fatalf("stored %+v", p)
		}
	}
}

func TestCreateSubscriptionSecretOnceAndSealed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.svc.CreateSubscription(ctx, &notificationsv1.CreateSubscriptionRequest{Name: "crm", Url: "https://partner.example/hook",
		EventTypes: []string{"authorization.approved", "payout.settled"}})
	if err != nil || len(res.Secret) != 64 || res.Subscription.GetId() == "" || !res.Subscription.Active {
		t.Fatalf("create: %v %+v", err, res)
	}
	if f.count(t, `SELECT count(*) FROM subscriptions WHERE secret_sealed LIKE '%'||$1||'%'`, res.Secret) != 0 {
		t.Fatal("secret stored in clear")
	}
	list, _ := f.svc.ListSubscriptions(ctx, &notificationsv1.ListSubscriptionsRequest{})
	if len(list.Subscriptions) != 1 || strings.Contains(list.String(), res.Secret) {
		t.Fatalf("list: %+v", list)
	}
	off, err := f.svc.DisableSubscription(ctx, &notificationsv1.DisableSubscriptionRequest{Id: res.Subscription.Id})
	if err != nil || off.Active || off.DisabledAt == nil {
		t.Fatalf("disable: %v %+v", err, off)
	}
	if _, err := f.svc.DisableSubscription(ctx, &notificationsv1.DisableSubscriptionRequest{Id: "0192f000-0000-7000-8000-0000000000ff"}); status.Code(err) != codes.NotFound {
		t.Fatalf("disable unknown: %v", err)
	}
}

func TestSubscriptionValidation(t *testing.T) {
	f := setup(t)
	for _, req := range []*notificationsv1.CreateSubscriptionRequest{
		{Name: "a", Url: "http://partner.example/hook", EventTypes: []string{"authorization.approved"}},
		{Name: "a", Url: "https://169.254.169.254/x", EventTypes: []string{"authorization.approved"}},
		{Name: "a", Url: "https://partner.example/hook", EventTypes: []string{"payout.instruction_read"}},
		{Name: "a", Url: "https://partner.example/hook"},
		{Url: "https://partner.example/hook", EventTypes: []string{"authorization.approved"}},
	} {
		if _, err := f.svc.CreateSubscription(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%+v: %v", req, err)
		}
	}
}

func TestInboxAndDeliveriesListing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		f.insertDelivery(t, "push", "push:cust-acc-1", nil)
	}
	if _, err := f.svc.DeliverOnce(ctx, "customer", f.now); err != nil {
		t.Fatal(err)
	}
	inbox, err := f.svc.ListInbox(ctx, &notificationsv1.ListInboxRequest{AccountId: "acc-1"})
	if err != nil || len(inbox.Items) != 3 || inbox.Items[0].Id < inbox.Items[2].Id {
		t.Fatalf("inbox newest first: %v %+v", err, inbox)
	}
	after, _ := f.svc.ListInbox(ctx, &notificationsv1.ListInboxRequest{AccountId: "acc-1", AfterId: inbox.Items[1].Id})
	if len(after.Items) != 1 || after.Items[0].Id != inbox.Items[0].Id {
		t.Fatalf("after: %+v", after)
	}
	if other, _ := f.svc.ListInbox(ctx, &notificationsv1.ListInboxRequest{AccountId: "acc-2"}); len(other.Items) != 0 {
		t.Fatal("inbox leaks across accounts")
	}
	sub := f.subscription(t, "https://partner.example/hook", "s", true)
	f.insertDelivery(t, "webhook:"+sub, "https://partner.example/hook", []byte(`{}`))
	d, err := f.svc.ListDeliveries(ctx, &notificationsv1.ListDeliveriesRequest{SubscriptionId: sub})
	if err != nil || len(d.Deliveries) != 1 || d.Deliveries[0].Status != "pending" {
		t.Fatalf("deliveries: %v %+v", err, d)
	}
}
