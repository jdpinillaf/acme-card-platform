//go:build integration

package notify

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/events"
)

func TestProcessCreatesOneDeliveryPerChannel(t *testing.T) {
	f := setup(t)
	if err := f.svc.Process(context.Background(), approved("acc-1")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='purchase_approved' AND customer_id='cust-acc-1' AND account_id='acc-1'
		AND status='pending' AND target IN ('push','sms','email') AND text LIKE '%12.34 USD at Corner Bookstore%'`); n != 3 {
		t.Fatalf("deliveries=%d", n)
	}
	var email string
	_ = f.pool.QueryRow(context.Background(), `SELECT recipient FROM deliveries WHERE target='email'`).Scan(&email)
	if email != "cust-acc-1@example.test" {
		t.Fatalf("email recipient %q", email)
	}
}

func TestRedeliveredEventCreatesNothing(t *testing.T) {
	f := setup(t)
	ev := approved("acc-1")
	if err := f.svc.Process(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.svc.Process(context.Background(), ev); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count(t, `SELECT count(*) FROM deliveries`); n != 3 {
		t.Fatalf("deliveries=%d", n)
	}
}

func TestPreferencesFilterChannels(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO preferences (customer_id, type, push, sms, email) VALUES ('cust-acc-1','purchase_approved',true,false,false)`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Process(context.Background(), approved("acc-1")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE target='push'`); n != 1 || f.count(t, `SELECT count(*) FROM deliveries`) != 1 {
		t.Fatal("preferences ignored")
	}
}

func TestMandatoryIgnoresAllOffRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO preferences (customer_id, type, push, sms, email) VALUES ('cust-acc-1','late_fee',false,false,false)`); err != nil {
		t.Fatal(err)
	}
	fee := event("fee.charged", "charge", "ch-1", map[string]any{"ID": "ch-1", "AccountID": "acc-1", "Kind": "fee", "Reference": "late_fee:acc-1:1", "Amount": 2500})
	if err := f.svc.Process(ctx, fee); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='late_fee' AND target='push' AND text LIKE '%25.00 USD%'`); n != 1 {
		t.Fatalf("mandatory late fee push=%d", n)
	}
}

func TestFraudAfterThreeDeclinesOncePerHour(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		if err := f.svc.Process(ctx, declined("acc-1", "insufficient_credit", now.Add(time.Duration(i)*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='suspected_fraud'`); n != 3 {
		t.Fatalf("fraud deliveries=%d (one alert × 3 channels)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='purchase_declined'`); n != 18 {
		t.Fatalf("decline deliveries=%d", n)
	}
	// declines that do not suggest someone else is using the card never raise it
	for i := 0; i < 3; i++ {
		if err := f.svc.Process(ctx, declined("acc-2", "card_frozen", now)); err != nil {
			t.Fatal(err)
		}
	}
	// and declines spread over more than the window do not either
	for i := 0; i < 3; i++ {
		if err := f.svc.Process(ctx, declined("acc-3", "over_limit", now.Add(time.Duration(i)*6*time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='suspected_fraud' AND account_id <> 'acc-1'`); n != 0 {
		t.Fatalf("unexpected fraud alerts=%d", n)
	}
}

func TestCardBlockedOnPastDueChargeOffAndRestriction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	acc := func(delinquency, restricted string) map[string]any {
		m := map[string]any{"id": "acc-1", "customer_id": "cust-acc-1", "currency": "USD", "delinquency": delinquency}
		if restricted != "" {
			m["restricted_reason"] = restricted
		}
		return m
	}
	past := event("account.delinquency_changed", "account", "acc-1", acc("past_due", ""))
	past.Before = []byte(`{"delinquency":"none"}`)
	evs := []struct{ name, contains string }{{"past due", "15 days"}, {"charge-off", "permanently"}, {"restricted", "payment was returned"}}
	all := []events.Event{past,
		event("account.charged_off", "account", "acc-1", map[string]any{"account": acc("charged_off", ""), "collateral_applied_minor": 30000}),
		event("card.restricted", "account", "acc-1", acc("none", "payment_returned"))}
	for i, e := range all {
		if err := f.svc.Process(ctx, e); err != nil {
			t.Fatal(err)
		}
		if n := f.count(t, `SELECT count(*) FROM deliveries WHERE notification_type='card_blocked' AND text LIKE '%'||$1||'%'`, evs[i].contains); n != 3 {
			t.Fatalf("%s: card_blocked deliveries=%d", evs[i].name, n)
		}
	}
}

func TestPartnerDeliveriesOnlyForSubscribedActiveAllowedTypes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sealed, _ := f.svc.box.Seal([]byte("secret"))
	for _, s := range []struct {
		id, types string
		active    bool
	}{{"0192f000-0000-7000-8000-00000000000a", "{authorization.approved}", true}, {"0192f000-0000-7000-8000-00000000000b", "{authorization.approved}", false},
		{"0192f000-0000-7000-8000-00000000000c", "{http.request}", true}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO subscriptions (id, name, url, event_types, secret_sealed, active) VALUES ($1,'p','https://partner.example/h',$2::text[],$3,$4)`,
			s.id, s.types, sealed, s.active); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Process(ctx, approved("acc-1")); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Process(ctx, event("http.request", "http", "/v1/x", map[string]any{"path": "/v1/x"})); err != nil {
		t.Fatal(err)
	}
	var target, payload string
	if err := f.pool.QueryRow(ctx, `SELECT target, payload::text FROM deliveries WHERE target LIKE 'webhook:%'`).Scan(&target, &payload); err != nil {
		t.Fatal(err)
	}
	if target != "webhook:0192f000-0000-7000-8000-00000000000a" || !strings.Contains(payload, `"authorization.approved"`) ||
		f.count(t, `SELECT count(*) FROM deliveries WHERE target LIKE 'webhook:%'`) != 1 {
		t.Fatalf("partner deliveries: %s %s", target, payload)
	}
}

func TestAccountLookupFailureLeavesMessage(t *testing.T) {
	f := setup(t)
	f.accounts.fail = true
	if err := f.svc.Process(context.Background(), approved("acc-1")); err == nil {
		t.Fatal("lookup failure swallowed")
	}
	if f.count(t, `SELECT count(*) FROM processed_events`) != 0 || f.count(t, `SELECT count(*) FROM deliveries`) != 0 {
		t.Fatal("state written despite the failure")
	}
	f.accounts.fail = false // SQS redelivers: now it goes through
	if err := f.svc.Process(context.Background(), approved("acc-1")); err != nil || f.count(t, `SELECT count(*) FROM deliveries`) != 3 {
		t.Fatalf("redelivery: %v", err)
	}
}

// Load fix: events that cannot notify anyone (http.request, ledger.*, audit.*, its own
// notification.* results…) cost no database work and no account lookup.
func TestIrrelevantEventsTouchNothing(t *testing.T) {
	f := setup(t)
	for _, e := range []events.Event{
		event("http.request", "http", "/v1/x", map[string]any{"path": "/v1/x"}),
		event("ledger.journal_posted", "journal_entry", "j-1", map[string]any{"account_id": "acc-1"}),
		event("notification.delivered", "notification", "d-1", map[string]any{"event_id": "e"}),
	} {
		if err := f.svc.Process(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t, `SELECT count(*) FROM processed_events`) != 0 || f.accounts.calls != 0 {
		t.Fatalf("irrelevant events did work: processed=%d lookups=%d", f.count(t, `SELECT count(*) FROM processed_events`), f.accounts.calls)
	}
}

// Load fix: an account's customer and currency are looked up once, then kept; account snapshots
// (card.*, account.*) seed them without any lookup.
func TestAccountLookupCached(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := f.svc.Process(ctx, approved("acc-1")); err != nil {
			t.Fatal(err)
		}
	}
	if f.accounts.calls != 1 {
		t.Fatalf("lookups=%d, want 1", f.accounts.calls)
	}
	activated := event("card.activated", "account", "acc-9", map[string]any{"id": "acc-9", "customer_id": "cust-9", "currency": "EUR", "credit_limit_minor": 100})
	if err := f.svc.Process(ctx, activated); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Process(ctx, approved("acc-9")); err != nil {
		t.Fatal(err)
	}
	if f.accounts.calls != 1 || f.count(t, `SELECT count(*) FROM deliveries WHERE account_id='acc-9' AND customer_id='cust-9' AND notification_type='purchase_approved'`) != 3 {
		t.Fatalf("snapshot did not seed the directory: lookups=%d", f.accounts.calls)
	}
}
