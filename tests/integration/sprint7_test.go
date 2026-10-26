//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/webhook"
)

// partner is an httptest partner that verifies every delivery with its secret.
type partner struct {
	mu       sync.Mutex
	secret   string
	events   map[string]int // event id → deliveries received
	types    []string
	rejected int
}

func (p *partner) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	sig := strings.TrimPrefix(r.Header.Get("X-Acme-Signature"), "v1=")
	if p.secret == "" || webhook.Verify([]byte(p.secret), body, r.Header.Get("X-Acme-Timestamp"), sig, time.Now(), 5*time.Minute) != nil {
		p.rejected++
		w.WriteHeader(401)
		return
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &ev)
	if ev.ID != r.Header.Get("X-Acme-Event-Id") {
		p.rejected++
		w.WriteHeader(400)
		return
	}
	p.events[ev.ID]++
	p.types = append(p.types, ev.Type)
	w.WriteHeader(204)
}

func (s *stack) inboxTypes(t *testing.T, card, tok string) map[string]int {
	t.Helper()
	r := s.call(t, "GET", "/v1/accounts/"+card+"/notifications", tok, "", nil)
	if r.status != 200 {
		t.Fatalf("inbox: %d %s", r.status, r.raw)
	}
	out := map[string]int{}
	for _, it := range r.body["items"].([]any) {
		m := it.(map[string]any)
		out[m["type"].(string)+"/"+m["channel"].(string)]++
	}
	return out
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestSprint7NotificationsEndToEnd(t *testing.T) {
	s := startStack(t)
	support, _ := auth.SignWithRole(jwtSecret, "agent-e2e", nil, auth.RoleSupport, time.Hour)
	p := &partner{events: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(p.handler))
	defer srv.Close()

	// a partner subscribes; the secret is shown once, a replay of the request does not repeat it
	key := uuid.NewString()
	reg := map[string]any{"name": "crm", "url": srv.URL + "/hook", "event_types": []string{"card.activated", "authorization.approved", "authorization.declined"}}
	r := s.call(t, "POST", "/v1/support/webhook-subscriptions", support, key, reg)
	if r.status != 201 || len(r.body["secret"].(string)) != 64 {
		t.Fatalf("register partner: %d %s", r.status, r.raw)
	}
	p.mu.Lock()
	p.secret = r.body["secret"].(string)
	p.mu.Unlock()
	if again := s.call(t, "POST", "/v1/support/webhook-subscriptions", support, key, reg); !again.replayed || strings.Contains(again.raw, p.secret) {
		t.Fatalf("replayed registration leaks the secret: %v %s", again.replayed, again.raw)
	}
	if n := count(t, s.gateway, `SELECT count(*) FROM idempotency_keys WHERE position(convert_to($1, 'UTF8') in response_body) > 0`, p.secret); n != 0 {
		t.Fatal("partner secret stored in clear by the gateway")
	}

	// a customer life: activation, a purchase, three declines (suspected fraud), a payment
	card, tok := s.activeCard(t, 1_000)
	s.purchase(t, card, 600)
	for i := 0; i < 3; i++ {
		d := s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 5_000), networkSecret)
		if d.body["decline_reason"] != "insufficient_credit" {
			t.Fatalf("decline: %s", d.raw)
		}
	}
	pay := s.pay(t, card, tok, 200)
	if r := s.settle(t, pay, 200, "settled"); r.status != 200 {
		t.Fatalf("settle: %d %s", r.status, r.raw)
	}

	want := map[string]int{"card_activated": 1, "purchase_approved": 1, "purchase_declined": 3, "suspected_fraud": 1, "payment_received": 1}
	eventually(t, "customer notifications on every channel", func() bool {
		got := s.inboxTypes(t, card, tok)
		for typ, n := range want {
			for _, ch := range []string{"push", "sms", "email"} {
				if got[typ+"/"+ch] != n {
					return false
				}
			}
		}
		return true
	})
	// nothing else, and nothing twice
	got := s.inboxTypes(t, card, tok)
	total := 0
	for _, n := range got {
		total += n
	}
	if total != 7*3 {
		t.Fatalf("inbox: %v", got)
	}

	// the partner received its subscribed types once each, all verified
	eventually(t, "partner deliveries", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.types) >= 5 // activated + approved + 3 declined
	})
	time.Sleep(500 * time.Millisecond)
	p.mu.Lock()
	for id, n := range p.events {
		if n != 1 {
			t.Fatalf("event %s delivered %d times", id, n)
		}
	}
	types := strings.Join(p.types, ",")
	rejected := p.rejected
	p.mu.Unlock()
	if rejected != 0 || strings.Contains(types, "payment.settled") || strings.Count(types, "authorization.declined") != 3 {
		t.Fatalf("partner saw %s (rejected %d)", types, rejected)
	}

	// mandatory types keep a channel
	if r := s.call(t, "PUT", "/v1/accounts/"+card+"/notification-preferences", tok, uuid.NewString(), map[string]any{"type": "suspected_fraud"}); r.status != 422 {
		t.Fatalf("mandatory off: %d %s", r.status, r.raw)
	}

	// another customer cannot read this inbox
	_, otherTok := s.activeCard(t, 500)
	if r := s.call(t, "GET", "/v1/accounts/"+card+"/notifications", otherTok, "", nil); r.status != 403 {
		t.Fatalf("foreign inbox: %d", r.status)
	}

	// past due: the late fee and the 15-day block are mandatory notifications
	late, lateTok := s.activeCard(t, 1_000)
	s.purchase(t, late, 600)
	s.advance(t, late, lateTok, 68)
	eventually(t, "late fee and card blocked", func() bool {
		got := s.inboxTypes(t, late, lateTok)
		// card_blocked: the 15-day block, plus "over the credit limit" when interest and the fee push the
		// debt past the limit (a restriction; both are real reasons and both notify)
		return got["late_fee/push"] >= 1 && got["card_blocked/push"] >= 1 && got["card_blocked/sms"] >= 1 && got["card_blocked/email"] >= 1
	})

	// every delivery is audited
	eventually(t, "notification.delivered in the audit trail", func() bool {
		return count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type='notification.delivered'`) >= 7*3+5+6
	})
	if n := count(t, s.notifications, `SELECT count(*) FROM (SELECT event_id, target FROM deliveries GROUP BY 1,2 HAVING count(*) > 1) d`); n != 0 {
		t.Fatalf("duplicate deliveries: %d", n)
	}
}
