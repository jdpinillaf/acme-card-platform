//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/webhook"
)

func (s *stack) network(t *testing.T, path string, body map[string]any, secret []byte) resp {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.url+path, bytes.NewReader(b))
	if secret != nil {
		ts, sig := webhook.SignRequest(secret, "POST", path, b, time.Now())
		req.Header.Set(webhook.HeaderTimestamp, ts)
		req.Header.Set(webhook.HeaderSignature, sig)
	}
	return do(t, req)
}

// activeCard opens an account, settles a deposit of amt and returns (accountID, token).
func (s *stack) activeCard(t *testing.T, amt int64) (string, string) {
	acc, dep, tok := s.openAndDeposit(t, amt)
	if r := s.bank(t, map[string]any{"deposit_id": dep, "external_ref": "bank-" + dep, "amount_minor": amt, "currency": "USD", "status": "settled"}, bankSecret); r.status != 200 {
		t.Fatalf("settle: %d %s", r.status, r.raw)
	}
	return acc, tok
}

func authBody(card, ref string, amt int64) map[string]any {
	return map[string]any{"network_ref": ref, "card_id": card, "amount_minor": amt, "currency": "USD",
		"merchant": map[string]any{"id": "m-42", "name": "Bookstore", "mcc": "5942"}}
}

func TestSprint2EndToEnd(t *testing.T) {
	s := startStack(t)

	t.Run("authorize, capture less, partial refunds, list", func(t *testing.T) {
		card, tok := s.activeCard(t, 100_000)
		a := s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 30_000), networkSecret)
		if a.status != 201 || a.body["status"] != "approved" {
			t.Fatalf("authorize: %d %s", a.status, a.raw)
		}
		id := a.body["id"].(string)
		c := s.network(t, "/v1/network/authorizations/"+id+"/capture", map[string]any{"network_ref": "c-" + id, "amount_minor": 25_000}, networkSecret)
		if c.status != 201 || c.body["status"] != "captured" {
			t.Fatalf("capture: %d %s", c.status, c.raw)
		}
		for i, amt := range []int64{10_000, 15_000} {
			r := s.network(t, "/v1/network/authorizations/"+id+"/refunds", map[string]any{"network_ref": fmt.Sprintf("r-%s-%d", id, i), "amount_minor": amt}, networkSecret)
			if r.status != 201 {
				t.Fatalf("refund %d: %d %s", i, r.status, r.raw)
			}
		}
		if r := s.network(t, "/v1/network/authorizations/"+id+"/refunds", map[string]any{"network_ref": "r-over-" + id, "amount_minor": 1}, networkSecret); r.status != 409 || r.body["error"].(map[string]any)["code"] != "refund_exceeds_captured" {
			t.Fatalf("over-refund: %d %s", r.status, r.raw)
		}
		g := s.call(t, "GET", "/v1/accounts/"+card, tok, "", nil)
		if g.body["available_credit_minor"] != 100_000.0 {
			t.Fatalf("available after full refund: %s", g.raw)
		}
		l := s.call(t, "GET", "/v1/accounts/"+card+"/transactions", tok, "", nil)
		txs := l.body["transactions"].([]any)
		if l.status != 200 || len(txs) != 1 || txs[0].(map[string]any)["refunded_amount_minor"] != 25_000.0 {
			t.Fatalf("list: %d %s", l.status, l.raw)
		}
	})

	t.Run("10 concurrent authorizations at 25% approve exactly 4", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		out := make([]resp, 10)
		var wg sync.WaitGroup
		for i := range out {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out[i] = s.network(t, "/v1/network/authorizations", authBody(card, fmt.Sprintf("n-%s-%d", card, i), 250), networkSecret)
			}(i)
		}
		wg.Wait()
		approved := 0
		for _, r := range out {
			if r.status != 201 {
				t.Fatalf("%d %s", r.status, r.raw)
			}
			if r.body["status"] == "approved" {
				approved++
			} else if r.body["decline_reason"] != "insufficient_credit" {
				t.Fatalf("unexpected decline: %s", r.raw)
			}
		}
		if approved != 4 {
			t.Fatalf("approved=%d want 4", approved)
		}
		if g := s.call(t, "GET", "/v1/accounts/"+card, tok, "", nil); g.body["available_credit_minor"] != 0.0 {
			t.Fatalf("available: %s", g.raw)
		}
	})

	t.Run("same network_ref x50 creates one authorization", func(t *testing.T) {
		card, _ := s.activeCard(t, 1_000)
		ref := "n-dup-" + card
		var wg sync.WaitGroup
		out := make([]resp, 50)
		for i := range out {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out[i] = s.network(t, "/v1/network/authorizations", authBody(card, ref, 100), networkSecret)
			}(i)
		}
		wg.Wait()
		replays := 0
		for _, r := range out {
			if r.status != 201 || r.body["id"] != out[0].body["id"] {
				t.Fatalf("divergent: %d %s", r.status, r.raw)
			}
			if r.replayed {
				replays++
			}
		}
		if replays != 49 || count(t, s.accounts, `SELECT count(*) FROM authorizations WHERE network_ref=$1`, ref) != 1 {
			t.Fatalf("replays=%d", replays)
		}
	})

	t.Run("freeze declines with card_frozen, unfreeze approves", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		if r := s.call(t, "POST", "/v1/accounts/"+card+"/freeze", tok, uuid.NewString(), nil); r.status != 200 || r.body["frozen"] != true {
			t.Fatalf("freeze: %d %s", r.status, r.raw)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-fz-"+card, 10), networkSecret); r.status != 201 || r.body["decline_reason"] != "card_frozen" {
			t.Fatalf("frozen auth: %d %s", r.status, r.raw)
		}
		_ = s.call(t, "POST", "/v1/accounts/"+card+"/unfreeze", tok, uuid.NewString(), nil)
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-ufz-"+card, 10), networkSecret); r.body["status"] != "approved" {
			t.Fatalf("after unfreeze: %s", r.raw)
		}
		if r := s.call(t, "PUT", "/v1/accounts/"+card+"/limits", tok, uuid.NewString(), map[string]any{"purchase_limit_minor": 50}); r.status != 200 {
			t.Fatalf("limits: %d %s", r.status, r.raw)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-pl-"+card, 51), networkSecret); r.body["decline_reason"] != "purchase_limit_exceeded" {
			t.Fatalf("purchase limit: %s", r.raw)
		}
		if r := s.call(t, "POST", "/v1/accounts/"+card+"/freeze", token(t, "intruder", "other"), uuid.NewString(), nil); r.status != 403 {
			t.Fatalf("foreign freeze: %d", r.status)
		}
	})

	t.Run("11th attempt in a minute hits velocity", func(t *testing.T) {
		card, _ := s.activeCard(t, 1_000)
		for i := 0; i < 10; i++ {
			s.network(t, "/v1/network/authorizations", authBody(card, fmt.Sprintf("n-vel-%s-%d", card, i), 5_000), networkSecret)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-vel-11-"+card, 1), networkSecret); r.body["decline_reason"] != "velocity_limit_exceeded" {
			t.Fatalf("11th: %s", r.raw)
		}
	})

	t.Run("unsigned or wrongly signed network calls are rejected", func(t *testing.T) {
		card, _ := s.activeCard(t, 1_000)
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-uns", 1), nil); r.status != 401 {
			t.Fatalf("unsigned: %d", r.status)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-bank", 1), bankSecret); r.status != 401 {
			t.Fatalf("bank secret on network route: %d", r.status)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(uuid.NewString(), "n-unknown", 1), networkSecret); r.status != 404 {
			t.Fatalf("unknown card: %d %s", r.status, r.raw)
		}
	})

	t.Run("sprint 2 events reach the append-only audit", func(t *testing.T) {
		deadline := time.Now().Add(45 * time.Second)
		want := []string{"authorization.approved", "authorization.declined", "authorization.captured", "refund.posted", "card.frozen", "card.unfrozen", "limits.updated", "ledger.hold_placed", "ledger.hold_captured"}
		for {
			missing := ""
			for _, typ := range want {
				if count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1`, typ) == 0 {
					missing = typ
					break
				}
			}
			if missing == "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("audit never received %s", missing)
			}
			time.Sleep(300 * time.Millisecond)
		}
		if c := count(t, s.ledger, `SELECT count(*) FROM (SELECT h.account_id FROM holds h GROUP BY h.account_id) x`); c == 0 {
			t.Fatal("no holds recorded")
		}
	})
}
