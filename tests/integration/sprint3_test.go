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

// bankReq sends a request-bound, bank-signed webhook (payments use method+path binding).
func (s *stack) bankReq(t *testing.T, path string, body map[string]any, secret []byte) resp {
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

// purchase authorizes and captures amt on card via the network routes.
func (s *stack) purchase(t *testing.T, card string, amt int64) (authID string) {
	t.Helper()
	a := s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), amt), networkSecret)
	if a.status != 201 || a.body["status"] != "approved" {
		t.Fatalf("authorize %d: %d %s", amt, a.status, a.raw)
	}
	id := a.body["id"].(string)
	if c := s.network(t, "/v1/network/authorizations/"+id+"/capture", map[string]any{"network_ref": "c-" + id, "amount_minor": amt}, networkSecret); c.status != 201 {
		t.Fatalf("capture: %d %s", c.status, c.raw)
	}
	return id
}

func (s *stack) pay(t *testing.T, card, tok string, amt int64) (payID string) {
	t.Helper()
	r := s.call(t, "POST", "/v1/accounts/"+card+"/payments", tok, uuid.NewString(), map[string]any{"amount_minor": amt})
	if r.status != 201 {
		t.Fatalf("create payment: %d %s", r.status, r.raw)
	}
	return r.body["payment"].(map[string]any)["id"].(string)
}

func (s *stack) settle(t *testing.T, payID string, amt int64, status string) resp {
	t.Helper()
	return s.bankReq(t, "/v1/webhooks/bank/payments", map[string]any{"payment_id": payID, "external_ref": "b-" + payID, "amount_minor": amt, "currency": "USD", "status": status}, bankSecret)
}

func (s *stack) account(t *testing.T, card, tok string) map[string]any {
	return s.call(t, "GET", "/v1/accounts/"+card, tok, "", nil).body
}

func TestSprint3EndToEnd(t *testing.T) {
	s := startStack(t)

	t.Run("pay and settle restores credit; failed payment changes nothing", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		s.purchase(t, card, 600)
		p := s.pay(t, card, tok, 600)
		if a := s.account(t, card, tok); a["available_credit_minor"] != 400.0 || a["outstanding_minor"] != 600.0 {
			t.Fatalf("before settle: %v", a)
		}
		if r := s.settle(t, p, 600, "settled"); r.status != 200 {
			t.Fatalf("settle: %d %s", r.status, r.raw)
		}
		if a := s.account(t, card, tok); a["available_credit_minor"] != 1000.0 || a["outstanding_minor"] != 0.0 {
			t.Fatalf("after settle: %v", a)
		}
		s.purchase(t, card, 300)
		f := s.pay(t, card, tok, 300)
		if r := s.settle(t, f, 300, "failed"); r.status != 200 || r.body["payment"].(map[string]any)["status"] != "failed" {
			t.Fatalf("fail: %d %s", r.status, r.raw)
		}
		if a := s.account(t, card, tok); a["available_credit_minor"] != 700.0 {
			t.Fatalf("failed payment must not restore credit: %v", a)
		}
	})

	t.Run("the same settlement webhook x50 settles once", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		s.purchase(t, card, 500)
		p := s.pay(t, card, tok, 500)
		var wg sync.WaitGroup
		out := make([]resp, 50)
		for i := range out {
			wg.Add(1)
			go func(i int) { defer wg.Done(); out[i] = s.settle(t, p, 500, "settled") }(i)
		}
		wg.Wait()
		fresh := 0
		for _, r := range out {
			if r.status != 200 {
				t.Fatalf("%d %s", r.status, r.raw)
			}
			if !r.replayed {
				fresh++
			}
		}
		if fresh != 1 || count(t, s.ledger, `SELECT count(*) FROM journal_entries WHERE reference=$1`, "payment:"+p) != 1 {
			t.Fatalf("fresh=%d", fresh)
		}
	})

	t.Run("credit balance is spent on the next purchase", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		auth := s.purchase(t, card, 700)
		p := s.pay(t, card, tok, 700)
		s.settle(t, p, 700, "settled")
		if r := s.network(t, "/v1/network/authorizations/"+auth+"/refunds", map[string]any{"network_ref": "rf-" + auth, "amount_minor": 700}, networkSecret); r.status != 201 {
			t.Fatalf("refund: %d %s", r.status, r.raw)
		}
		a := s.account(t, card, tok)
		if a["credit_balance_minor"] != 700.0 || a["available_credit_minor"] != 1700.0 {
			t.Fatalf("credit balance: %v", a)
		}
		s.purchase(t, card, 500)
		a = s.account(t, card, tok)
		if a["credit_balance_minor"] != 200.0 || a["outstanding_minor"] != 0.0 {
			t.Fatalf("500 paid from the credit balance: %v", a)
		}
	})

	t.Run("return over the limit restricts until paid back", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		s.purchase(t, card, 1_000)
		p := s.pay(t, card, tok, 1_000)
		s.settle(t, p, 1_000, "settled")
		s.purchase(t, card, 1_000)
		path := "/v1/webhooks/bank/payments/" + p + "/return"
		if r := s.bankReq(t, path, map[string]any{"return_ref": "r-" + p, "amount_minor": 1_000}, bankSecret); r.status != 200 {
			t.Fatalf("return: %d %s", r.status, r.raw)
		}
		if a := s.account(t, card, tok); a["restricted_reason"] != "payment_returned" || a["outstanding_minor"] != 2000.0 {
			t.Fatalf("after return: %v", a)
		}
		if r := s.network(t, "/v1/network/authorizations", authBody(card, "n-rst-"+card, 1), networkSecret); r.body["decline_reason"] != "payment_returned" {
			t.Fatalf("decline: %s", r.raw)
		}
		q := s.pay(t, card, tok, 1_000)
		s.settle(t, q, 1_000, "settled")
		if a := s.account(t, card, tok); a["restricted_reason"] != nil {
			t.Fatalf("restriction must lift: %v", a)
		}
	})

	t.Run("payment webhooks need the bound bank signature", func(t *testing.T) {
		body := map[string]any{"payment_id": uuid.NewString(), "external_ref": "x", "amount_minor": 100, "currency": "USD", "status": "settled"}
		if r := s.bankReq(t, "/v1/webhooks/bank/payments", body, nil); r.status != 401 {
			t.Fatalf("unsigned: %d", r.status)
		}
		if r := s.bankReq(t, "/v1/webhooks/bank/payments", body, networkSecret); r.status != 401 {
			t.Fatalf("wrong secret: %d", r.status)
		}
	})

	t.Run("payment events reach the audit", func(t *testing.T) {
		want := []string{"payment.created", "payment.settled", "payment.failed", "payment.returned", "card.restricted", "card.unrestricted", "ledger.payment_posted", "ledger.payment_returned"}
		deadline := time.Now().Add(45 * time.Second)
		for _, typ := range want {
			for count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1`, typ) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("audit never received %s", typ)
				}
				time.Sleep(300 * time.Millisecond)
			}
		}
	})
}

var _ = fmt.Sprint
