//go:build integration

package integration

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/webhook"
	"github.com/acme/acme-card/services/accounts/accountstest"
)

var destination = map[string]any{"bank_code": "BANK001", "account_number": "000123456789", "holder_name": "Jane Doe"}

func (s *stack) bankGet(t *testing.T, path string) resp {
	t.Helper()
	req, _ := http.NewRequest("GET", s.url+path, nil)
	ts, sig := webhook.SignRequest(bankSecret, "GET", path, nil, time.Now())
	req.Header.Set(webhook.HeaderTimestamp, ts)
	req.Header.Set(webhook.HeaderSignature, sig)
	return do(t, req)
}

func (s *stack) openPayout(t *testing.T, card, tok string) map[string]any {
	t.Helper()
	for _, p := range s.call(t, "GET", "/v1/accounts/"+card+"/payouts", tok, "", nil).body["payouts"].([]any) {
		if po := p.(map[string]any); po["status"] == "pending" {
			return po
		}
	}
	t.Fatalf("no pending payout for %s", card)
	return nil
}

// payOut fetches the bank instruction and answers the attempt.
func (s *stack) payOut(t *testing.T, po map[string]any, status string) resp {
	t.Helper()
	attempt := po["attempt_id"].(string)
	ins := s.bankGet(t, "/v1/bank/payout-instructions/"+attempt)
	if ins.status != 200 || ins.body["account_number"] != "000123456789" {
		t.Fatalf("instruction: %d %s", ins.status, ins.raw)
	}
	return s.bankReq(t, "/v1/webhooks/bank/payouts", map[string]any{"attempt_id": attempt, "amount_minor": po["amount_minor"], "currency": "USD",
		"destination_last4": "6789", "status": status}, bankSecret)
}

func TestSprint5EndToEnd(t *testing.T) {
	s := startStack(t)

	t.Run("close in good standing, payout, reopen", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		r := s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, uuid.NewString(), map[string]any{"destination": destination})
		if r.status != 202 || strings.Contains(r.raw, "000123456789") {
			t.Fatalf("close: %d %s", r.status, r.raw)
		}
		po := s.openPayout(t, card, tok)
		if po["amount_minor"] != 1000.0 {
			t.Fatalf("payout: %v", po)
		}
		if r := s.payOut(t, po, "settled"); r.status != 200 {
			t.Fatalf("settle: %d %s", r.status, r.raw)
		}
		if a := s.account(t, card, tok); a["state"] != "closed" || a["collateral_minor"] != 0.0 {
			t.Fatalf("after payout: %v", a)
		}
		sub := s.account(t, card, tok)["customer_id"].(string)
		if r := s.call(t, "POST", "/v1/accounts", token(t, sub), uuid.NewString(), map[string]any{"currency": "USD"}); r.status != 201 {
			t.Fatalf("reopen: %d %s", r.status, r.raw)
		}
	})

	t.Run("close with debt: collateral applied on the final due date, remainder paid out", func(t *testing.T) {
		card, tok := s.activeCard(t, 1_000)
		s.purchase(t, card, 600)
		if r := s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, uuid.NewString(), map[string]any{"destination": destination}); r.status != 202 || r.body["close_stage"] != "awaiting_due" {
			t.Fatalf("close: %d %s", r.status, r.raw)
		}
		a := s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 1), networkSecret)
		if a.body["decline_reason"] != "account_closing" {
			t.Fatalf("authorization while closing: %s", a.raw)
		}
		s.advance(t, card, tok, 23)
		po := s.openPayout(t, card, tok)
		if po["amount_minor"] != 400.0 {
			t.Fatalf("payout of the remainder: %v", po)
		}
		if r := s.payOut(t, po, "settled"); r.status != 200 {
			t.Fatalf("settle: %d %s", r.status, r.raw)
		}
		if a := s.account(t, card, tok); a["state"] != "closed" || a["outstanding_minor"] != 0.0 {
			t.Fatalf("closed: %v", a)
		}
	})

	t.Run("failed payout, support retry", func(t *testing.T) {
		card, tok := s.activeCard(t, 500)
		s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, uuid.NewString(), map[string]any{"destination": destination})
		po := s.openPayout(t, card, tok)
		if r := s.payOut(t, po, "failed"); r.status != 200 {
			t.Fatalf("fail: %d %s", r.status, r.raw)
		}
		support, _ := auth.SignWithRole(jwtSecret, "agent-e2e", nil, auth.RoleSupport, time.Hour)
		if r := s.call(t, "POST", "/v1/support/accounts/"+card+"/payouts/"+po["id"].(string)+"/retry", tok, uuid.NewString(), nil); r.status != 403 {
			t.Fatalf("customer retry: %d", r.status)
		}
		if r := s.call(t, "POST", "/v1/support/accounts/"+card+"/payouts/"+po["id"].(string)+"/retry", support, uuid.NewString(), nil); r.status != 201 {
			t.Fatalf("retry: %d %s", r.status, r.raw)
		}
		po2 := s.openPayout(t, card, tok)
		if r := s.payOut(t, po2, "settled"); r.status != 200 {
			t.Fatalf("settle attempt 2: %d %s", r.status, r.raw)
		}
		late := s.bankReq(t, "/v1/webhooks/bank/payouts", map[string]any{"attempt_id": po["attempt_id"], "amount_minor": 500, "currency": "USD",
			"destination_last4": "6789", "status": "settled"}, bankSecret)
		if late.status != 409 {
			t.Fatalf("late settled of the failed attempt: %d %s", late.status, late.raw)
		}
		if n := count(t, s.ledger, `SELECT count(*) FROM journal_entries WHERE account_id=$1 AND entry_type='payout_settled'`, card); n != 1 {
			t.Fatalf("payout journals=%d", n)
		}
	})

	t.Run("cancel voids deposits; a late deposit webhook is rejected", func(t *testing.T) {
		acc, dep, tok := s.openAndDeposit(t, 700)
		if r := s.call(t, "POST", "/v1/accounts/"+acc+"/cancel", tok, uuid.NewString(), nil); r.status != 200 || r.body["state"] != "cancelled" {
			t.Fatalf("cancel: %d %s", r.status, r.raw)
		}
		if r := s.bank(t, map[string]any{"deposit_id": dep, "external_ref": "bank-" + dep, "amount_minor": 700, "currency": "USD", "status": "settled"}, bankSecret); r.status != 409 {
			t.Fatalf("late deposit: %d %s", r.status, r.raw)
		}
	})

	t.Run("close ×50 with one key closes once", func(t *testing.T) {
		card, tok := s.activeCard(t, 300)
		key := uuid.NewString()
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r := s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, key, map[string]any{"destination": destination}); r.status != 202 && r.status != 409 {
					t.Errorf("status %d %s", r.status, r.raw)
				}
			}()
		}
		wg.Wait()
		if n := count(t, s.accounts, `SELECT count(*) FROM payouts WHERE account_id=$1`, card); n != 1 {
			t.Fatalf("payouts=%d", n)
		}
	})

	t.Run("audit trail", func(t *testing.T) {
		deadline := time.Now().Add(30 * time.Second)
		for _, typ := range []string{"account.close_requested", "account.closed", "payout.requested", "payout.settled", "payout.failed", "payout.retried",
			"account.cancelled", "deposit.voided", "ledger.payout_posted"} {
			for count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1`, typ) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("no %s in audit", typ)
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
		if n := count(t, s.audit, `SELECT count(*) FROM audit_events WHERE after::text LIKE '%000123456789%' OR before::text LIKE '%000123456789%'`); n != 0 {
			t.Fatalf("%d audit events carry the full destination number", n)
		}
	})
}

// With a short return window (load/e2e only) a payment settled just before the close holds the
// payout until the window passes.
func TestSprint5ReturnWindowHoldsPayout(t *testing.T) {
	s := startStackWith(t, stackConfig{Accounts: []accountstest.Option{accountstest.WithReturnWindow(3 * time.Second)}})
	card, tok := s.activeCard(t, 1_000)
	s.purchase(t, card, 300)
	p := s.pay(t, card, tok, 300)
	if r := s.settle(t, p, 300, "settled"); r.status != 200 {
		t.Fatalf("settle: %d", r.status)
	}
	if r := s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, uuid.NewString(), map[string]any{"destination": destination}); r.body["close_stage"] != "awaiting_returns" {
		t.Fatalf("close: %s", r.raw)
	}
	time.Sleep(3500 * time.Millisecond)
	s.advance(t, card, tok, 1) // any advance runs the saga on a closing account
	if po := s.openPayout(t, card, tok); po["amount_minor"] != 1000.0 {
		t.Fatalf("payout: %v", po)
	}
}
