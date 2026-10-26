package rules

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/events"
)

func ev(typ, aggType, aggID string, before, after any) events.Event {
	e := events.New(typ, aggType, aggID)
	if before != nil {
		e.Before, _ = json.Marshal(before)
	}
	if after != nil {
		e.After, _ = json.Marshal(after)
	}
	return e
}

func acct(extra map[string]any) map[string]any {
	m := map[string]any{"id": "acc-1", "customer_id": "cust-1", "currency": "USD", "state": "active", "credit_limit_minor": 50000, "delinquency": "none"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func auth(status, reason string) map[string]any {
	m := map[string]any{"id": "au-1", "account_id": "acc-1", "amount_minor": 1234, "currency": "USD", "merchant_name": "Corner Bookstore", "status": status}
	if reason != "" {
		m["decline_reason"] = reason
	}
	return m
}

func TestClassifyEveryType(t *testing.T) {
	cases := []struct {
		name      string
		e         events.Event
		typ       string
		mandatory bool
		contains  string
	}{
		{"activated", ev("card.activated", "account", "acc-1", acct(map[string]any{"state": "pending_collateral"}), acct(nil)), "card_activated", false, "500.00 USD"},
		{"approved", ev("authorization.approved", "authorization", "au-1", nil, auth("approved", "")), "purchase_approved", false, "12.34 USD at Corner Bookstore"},
		{"declined", ev("authorization.declined", "authorization", "au-1", nil, auth("declined", "insufficient_credit")), "purchase_declined", false, "insufficient credit"},
		{"frozen", ev("card.frozen", "account", "acc-1", acct(nil), acct(map[string]any{"frozen": true})), "card_freeze", false, "frozen"},
		{"unfrozen", ev("card.unfrozen", "account", "acc-1", acct(map[string]any{"frozen": true}), acct(nil)), "card_freeze", false, "unfrozen"},
		{"payment", ev("payment.settled", "payment", "pa-1", nil, map[string]any{"id": "pa-1", "account_id": "acc-1", "amount_minor": 1000, "currency": "USD", "status": "settled"}), "payment_received", false, "10.00 USD"},
		{"late fee", ev("fee.charged", "charge", "ch-1", nil, map[string]any{"ID": "ch-1", "AccountID": "acc-1", "Kind": "fee", "Reference": "late_fee:acc-1:2", "Amount": 2500}), "late_fee", true, "25.00 USD"},
		{"blocked past due", ev("account.delinquency_changed", "account", "acc-1", acct(nil), acct(map[string]any{"delinquency": "past_due"})), "card_blocked", true, "15 days past due"},
		{"charged off", ev("account.charged_off", "account", "acc-1", acct(map[string]any{"delinquency": "past_due"}), map[string]any{"account": acct(map[string]any{"delinquency": "charged_off"}), "collateral_applied_minor": 30000}), "card_blocked", true, "300.00 USD"},
		{"restricted", ev("card.restricted", "account", "acc-1", acct(nil), acct(map[string]any{"restricted_reason": "payment_returned"})), "card_blocked", true, "payment was returned"},
		{"payout requested", ev("payout.requested", "payout", "po-1", nil, map[string]any{"account_id": "acc-1", "amount_minor": 70000, "currency": "USD", "status": "pending", "destination_last4": "6789"}), "payout_update", false, "****6789"},
		{"payout settled", ev("payout.settled", "payout", "po-1", nil, map[string]any{"account_id": "acc-1", "amount_minor": 70000, "currency": "USD", "status": "settled", "destination_last4": "6789"}), "payout_update", false, "700.00 USD"},
		{"payout failed", ev("payout.failed", "payout", "po-1", nil, map[string]any{"account_id": "acc-1", "amount_minor": 70000, "currency": "USD", "status": "failed", "destination_last4": "6789"}), "payout_update", false, "failed"},
	}
	for _, c := range cases {
		n, ok := Classify(c.e)
		if !ok {
			t.Fatalf("%s: not classified", c.name)
		}
		if n.Type != c.typ || n.Mandatory != c.mandatory || n.AccountID != "acc-1" {
			t.Fatalf("%s: %+v", c.name, n)
		}
		if txt := n.Text("USD"); !strings.Contains(txt, c.contains) {
			t.Fatalf("%s: text %q lacks %q", c.name, txt, c.contains)
		}
	}
	// account-shaped payloads name their customer; the others need a lookup
	if n, _ := Classify(cases[0].e); n.CustomerID != "cust-1" {
		t.Fatalf("customer from snapshot: %+v", n)
	}
	if n, _ := Classify(cases[1].e); n.CustomerID != "" {
		t.Fatalf("authorization carries no customer: %+v", n)
	}
}

func TestClassifySkips(t *testing.T) {
	for _, e := range []events.Event{
		ev("fee.charged", "charge", "ch-1", nil, map[string]any{"AccountID": "acc-1", "Reference": "other:1", "Amount": 100}),
		ev("interest.charged", "charge", "ch-1", nil, map[string]any{"AccountID": "acc-1", "Reference": "interest:1", "Amount": 100}),
		ev("account.delinquency_changed", "account", "acc-1", acct(map[string]any{"delinquency": "past_due"}), acct(map[string]any{"delinquency": "past_due"})),
		ev("account.delinquency_changed", "account", "acc-1", acct(map[string]any{"delinquency": "past_due"}), acct(nil)),
		ev("http.request", "http", "/v1/x", nil, map[string]any{}),
		ev("payout.instruction_read", "payout", "po-1", nil, map[string]any{"account_id": "acc-1"}),
	} {
		if n, ok := Classify(e); ok {
			t.Fatalf("%s classified as %+v", e.Type, n)
		}
	}
}

func TestTextsCarryNoSecrets(t *testing.T) {
	e := ev("payout.requested", "payout", "po-1", nil, map[string]any{"account_id": "acc-1", "amount_minor": 70000, "currency": "USD",
		"destination_last4": "6789", "account_number": "000123456789", "holder_name": "Jane Doe", "destination_ciphertext": "c2VjcmV0"})
	n, _ := Classify(e)
	txt := n.Text("USD")
	for _, s := range []string{"000123456789", "Jane Doe", "c2VjcmV0"} {
		if strings.Contains(txt, s) {
			t.Fatalf("text leaks %q: %s", s, txt)
		}
	}
	if !strings.Contains(txt, "****6789") {
		t.Fatalf("text: %s", txt)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if d, ok := Backoff(i + 1); !ok || d != w {
			t.Fatalf("failure %d: %v %v", i+1, d, ok)
		}
	}
	if _, ok := Backoff(6); ok {
		t.Fatal("6th failure must give up")
	}
}

func TestMandatoryCannotBeSilenced(t *testing.T) {
	for _, typ := range []string{"late_fee", "card_blocked", "suspected_fraud"} {
		if err := ValidatePreference(typ, false, false, false); !errors.Is(err, ErrMandatory) {
			t.Fatalf("%s all off: %v", typ, err)
		}
		if err := ValidatePreference(typ, false, true, false); err != nil {
			t.Fatalf("%s one channel: %v", typ, err)
		}
	}
	if err := ValidatePreference("purchase_approved", false, false, false); err != nil {
		t.Fatalf("optional all off: %v", err)
	}
	if err := ValidatePreference("nope", true, true, true); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("unknown: %v", err)
	}
	if len(AllTypes) != 9 {
		t.Fatalf("types: %v", AllTypes)
	}
}

func TestPartnerAllowList(t *testing.T) {
	for _, typ := range []string{"account.opened", "card.frozen", "authorization.declined", "payment.settled", "payout.settled", "statement.generated",
		"fee.charged", "interest.charged", "charge.waived", "collateral.confirmed", "collateral.deposit_failed"} {
		if !PartnerEventAllowed(typ) {
			t.Fatalf("%s refused", typ)
		}
	}
	for _, typ := range []string{"payout.instruction_read", "http.request", "audit.queried", "snapshot.account", "ledger.journal_posted", "notification.delivered", "statement.read", "account"} {
		if PartnerEventAllowed(typ) {
			t.Fatalf("%s allowed", typ)
		}
	}
}

func TestCountsTowardFraud(t *testing.T) {
	for reason, want := range map[string]bool{"insufficient_credit": true, "over_limit": true, "purchase_limit_exceeded": true, "velocity_limit_exceeded": true,
		"card_frozen": false, "past_due": false, "account_closed": false, "collateral_not_confirmed": false} {
		if got := CountsTowardFraud(ev("authorization.declined", "authorization", "au-1", nil, auth("declined", reason))); got != want {
			t.Fatalf("%s: %v", reason, got)
		}
	}
	if CountsTowardFraud(ev("authorization.approved", "authorization", "au-1", nil, auth("approved", ""))) {
		t.Fatal("approval counted")
	}
	if n := FraudNotice("acc-1"); n.Type != "suspected_fraud" || !n.Mandatory || n.Text("USD") == "" {
		t.Fatalf("fraud notice: %+v", n)
	}
}

func TestFormatMinor(t *testing.T) {
	if FormatMinor(123456, "USD") != "1234.56 USD" || FormatMinor(5, "EUR") != "0.05 EUR" {
		t.Fatal(FormatMinor(123456, "USD"))
	}
}
