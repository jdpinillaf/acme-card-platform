//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/webhook"
	"github.com/acme/acme-card/services/accounts/accountstest"
)

// The journey stack exposes two test-only knobs: the per-subject gateway rate limit and the
// per-card authorization velocity. The first call past each limit is rejected.
func TestJourneyStackKnobs(t *testing.T) {
	s := startStackWith(t, stackConfig{RateLimitPerMinute: 4, Accounts: []accountstest.Option{accountstest.WithVelocity(2, time.Minute)}})
	card, tok := s.activeCard(t, 1_000) // two authenticated calls by this subject: open, deposit

	// Rate limit: calls 3 and 4 pass, call 5 is 429 with Retry-After.
	var last resp
	for i := 0; i < 3; i++ {
		last = s.call(t, "GET", "/v1/accounts/"+card, tok, "", nil)
		if i < 2 && last.status != 200 {
			t.Fatalf("call %d: %d %s", i+3, last.status, last.raw)
		}
	}
	if last.status != 429 || errCode(last) != "velocity_limit_exceeded" {
		t.Fatalf("5th call: want 429 velocity_limit_exceeded, got %d %s", last.status, last.raw)
	}

	// Velocity: the 3rd authorization attempt on the card is declined velocity_limit_exceeded.
	var a resp
	for i := 0; i < 3; i++ {
		a = s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 1), networkSecret)
		if a.status != 201 {
			t.Fatalf("authorization %d: %d %s", i+1, a.status, a.raw)
		}
	}
	if a.body["decline_reason"] != "velocity_limit_exceeded" {
		t.Fatalf("3rd authorization: %s", a.raw)
	}
}

// TestCustomerJourney walks one customer through Sprints 1–4 and, at every stage, lets an
// intruder try what an attacker would. Timings are recorded by the journal and reported, never
// asserted (v1). Phases share state and run in order; a failing phase stops the journey.
func TestCustomerJourney(t *testing.T) {
	s := startStackWith(t, stackConfig{RateLimitPerMinute: 500, Accounts: []accountstest.Option{accountstest.WithVelocity(30, time.Minute)}})
	j := newJournal(s)
	defer j.report(t)

	// Actors.
	sub := "cust-J-" + uuid.NewString()[:8]
	var acc, tok string
	authed := 0 // authenticated calls made with the customer's token (audit completeness check)
	cust := func(name, method, path, key string, body any) resp {
		authed++
		r := j.step(name, func() resp { return s.call(t, method, path, tok, key, body) })
		if r.status < 300 {
			assertNoLeak(t, name, r)
		}
		return r
	}
	net := func(name, path string, body map[string]any) resp {
		r := j.step(name, func() resp { return s.network(t, path, body, networkSecret) })
		if r.status < 300 {
			assertNoLeak(t, name, r)
		}
		return r
	}
	expect := func(name string, r resp, status int, code string) {
		t.Helper()
		if r.status != status || (code != "" && errCode(r) != code) {
			t.Fatalf("%s: want %d %s, got %d %s", name, status, code, r.status, r.raw)
		}
	}
	authorize := func(name string, amt int64, mutate func(map[string]any)) resp {
		b := authBody(acc, "n-"+uuid.NewString(), amt)
		if mutate != nil {
			mutate(b)
		}
		r := net(name, "/v1/network/authorizations", b)
		if r.status != 201 {
			t.Fatalf("%s: %d %s", name, r.status, r.raw)
		}
		return r
	}
	declined := func(name string, amt int64, reason string, mutate func(map[string]any)) {
		t.Helper()
		if r := authorize(name, amt, mutate); r.body["status"] != "declined" || r.body["decline_reason"] != reason {
			t.Fatalf("%s: want declined %s, got %s", name, reason, r.raw)
		}
	}
	acct := func() map[string]any { return s.account(t, acc, tok) }
	waitAccount := func(name string, ok func(a map[string]any) bool) {
		t.Helper()
		if j.until(name, 10*time.Second, func() bool { return ok(acct()) }) < 0 {
			t.Fatalf("%s: account never reached the expected state: %v", name, acct())
		}
	}
	xcard, xtok := s.activeCard(t, 50_000) // the intruder: a legitimate customer of their own
	support, err := auth.SignWithRole(jwtSecret, "agent-J", nil, auth.RoleSupport, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	var authID, payID, feeID string
	var statement1 map[string]any
	failed := false
	phase := func(name string, fn func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			if failed {
				t.Skip("an earlier phase failed")
			}
			defer func() {
				if t.Failed() {
					failed = true
				}
			}()
			fn(t)
		})
	}

	phase("onboarding", func(t *testing.T) {
		j.begin("onboarding")
		k1 := uuid.NewString()
		tok = token(t, sub)
		r := cust("open account", "POST", "/v1/accounts", k1, map[string]any{"currency": "USD"})
		expect("open", r, 201, "")
		if r.body["state"] != "pending_collateral" || r.body["credit_limit_minor"] != 0.0 {
			t.Fatalf("open: %s", r.raw)
		}
		acc = r.body["id"].(string)
		r2 := cust("open again, same key", "POST", "/v1/accounts", k1, map[string]any{"currency": "USD"})
		if !r2.replayed || r2.body["id"] != acc {
			t.Fatalf("replay: %v %s", r2.replayed, r2.raw)
		}
		expect("open same key other body", cust("open same key, other body", "POST", "/v1/accounts", k1, map[string]any{"currency": "EUR"}), 422, "idempotency_key_reuse")
		j.audit(t, "audit: account.opened", "account.opened", acc)
		tok = token(t, sub, acc)

		// Tokens.
		get := func(name, bearer string) resp {
			return j.step(name, func() resp { return s.call(t, "GET", "/v1/accounts/"+acc, bearer, "", nil) })
		}
		expect("alg none", get("token: alg none", noneAlgToken(sub, []string{acc})), 401, "unauthorized")
		expect("expired", get("token: expired", expiredToken(t, sub, acc)), 401, "unauthorized")
		expect("foreign secret", get("token: foreign secret", foreignSecretToken(t, sub, acc)), 401, "unauthorized")
		expect("no accounts", get("token: valid, no accounts", token(t, sub)), 403, "account_not_authorized")
		// Tenancy oracle: an existing account and a random one must be indistinguishable.
		a := get("tenancy: intruder reads customer account", xtok)
		b := j.step("tenancy: intruder reads random account", func() resp { return s.call(t, "GET", "/v1/accounts/"+uuid.NewString(), xtok, "", nil) })
		if a.status != 403 || b.status != 403 || a.raw != b.raw {
			t.Fatalf("existence oracle: %d %s vs %d %s", a.status, a.raw, b.status, b.raw)
		}

		// Deposit.
		d := cust("deposit intent 100,000", "POST", "/v1/accounts/"+acc+"/collateral/deposits", uuid.NewString(), map[string]any{"amount_minor": 100_000})
		expect("deposit", d, 201, "")
		if d.body["status"] != "pending" {
			t.Fatalf("deposit: %s", d.raw)
		}
		dep := d.body["id"].(string)
		expect("unknown field", cust("deposit: unknown field", "POST", "/v1/accounts/"+acc+"/collateral/deposits", uuid.NewString(), map[string]any{"amount_minor": 1, "note": "x"}), 400, "invalid_body")
		z := cust("deposit: amount 0", "POST", "/v1/accounts/"+acc+"/collateral/deposits", uuid.NewString(), map[string]any{"amount_minor": 0})
		if z.status != 400 {
			t.Fatalf("zero deposit: %d %s", z.status, z.raw)
		}
		t.Logf("deposit amount 0 → %d %s", z.status, errCode(z))

		settled := map[string]any{"deposit_id": dep, "external_ref": "bank-" + dep, "amount_minor": 100_000, "currency": "USD", "status": "settled"}
		wh := func(name string, body map[string]any, at time.Time, mutate func(*http.Request)) resp {
			return j.step(name, func() resp { return s.signedAt(t, "/v1/webhooks/bank/deposits", body, bankSecret, at, mutate) })
		}
		expect("bad signature", wh("webhook: bad signature", settled, time.Now(), func(r *http.Request) { r.Header.Set(webhook.HeaderSignature, "deadbeef") }), 401, "")
		expect("stale timestamp", wh("webhook: stale timestamp", settled, time.Now().Add(-10*time.Minute), nil), 401, "")
		big := map[string]any{"deposit_id": dep, "external_ref": strings.Repeat("x", 2<<20), "amount_minor": 100_000, "currency": "USD", "status": "settled"}
		expect("oversize", wh("webhook: oversize body", big, time.Now(), nil), 413, "body_too_large")
		mism := map[string]any{"deposit_id": dep, "external_ref": "bank-" + dep, "amount_minor": 99_999, "currency": "USD", "status": "settled"}
		expect("amount mismatch", wh("webhook: amount mismatch", mism, time.Now(), nil), 400, "amount_mismatch")
		if a := acct(); a["state"] != "pending_collateral" {
			t.Fatalf("after rejected webhooks: %v", a)
		}
		expect("settle", wh("webhook: settle deposit", settled, time.Now(), nil), 200, "")
		waitAccount("account active after webhook", func(a map[string]any) bool { return a["state"] == "active" })
		if a := acct(); a["credit_limit_minor"] != 100_000.0 || a["available_credit_minor"] != 100_000.0 {
			t.Fatalf("active: %v", a)
		}
		rep := wh("webhook: replay", settled, time.Now(), nil)
		expect("replay", rep, 200, "")
		if n := count(t, s.accounts, `SELECT count(*) FROM deposits WHERE account_id=$1 AND status='settled'`, acc); n != 1 {
			t.Fatalf("settled deposits after replay: %d", n)
		}
		d2 := cust("second deposit intent", "POST", "/v1/accounts/"+acc+"/collateral/deposits", uuid.NewString(), map[string]any{"amount_minor": 5})
		expect("deposit 2", d2, 201, "")
		reuse := map[string]any{"deposit_id": d2.body["id"], "external_ref": "bank-" + dep, "amount_minor": 5, "currency": "USD", "status": "settled"}
		expect("external_ref reuse", wh("webhook: same external_ref, other deposit", reuse, time.Now(), nil), 422, "external_ref_reused")
		j.audit(t, "audit: collateral.confirmed", "collateral.confirmed", dep)
		if n := count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type='collateral.confirmed' AND (aggregate_id=$1 OR after::text LIKE '%'||$1||'%')`, dep); n != 1 {
			t.Fatalf("collateral.confirmed events: %d", n)
		}
		j.audit(t, "audit: card.activated", "card.activated", acc)
	})

	phase("spending", func(t *testing.T) {
		j.begin("spending")
		a := authorize("authorize 30,000", 30_000, nil)
		if a.body["status"] != "approved" {
			t.Fatalf("authorize: %s", a.raw)
		}
		authID = a.body["id"].(string)
		c := net("capture 30,000", "/v1/network/authorizations/"+authID+"/capture", map[string]any{"network_ref": "c-" + authID, "amount_minor": 30_000})
		expect("capture", c, 201, "")
		if x := acct(); x["available_credit_minor"] != 70_000.0 || x["outstanding_minor"] != 30_000.0 {
			t.Fatalf("after capture: %v", x)
		}
		j.audit(t, "audit: authorization.captured", "authorization.captured", authID)

		// Reversal.
		rv := authorize("authorize 5,000 (to reverse)", 5_000, nil)
		rvID := rv.body["id"].(string)
		if x := acct(); x["available_credit_minor"] != 65_000.0 {
			t.Fatalf("hold not reflected: %v", x)
		}
		r := net("reversal", "/v1/network/authorizations/"+rvID+"/reversal", map[string]any{"network_ref": "rv-" + rvID})
		expect("reversal", r, 200, "")
		if r.body["status"] != "reversed" || acct()["available_credit_minor"] != 70_000.0 {
			t.Fatalf("reversal: %s / %v", r.raw, acct())
		}
		expect("reversal twice", net("reversal twice", "/v1/network/authorizations/"+rvID+"/reversal", map[string]any{"network_ref": "rv2-" + rvID}), 409, "already_reversed")

		// Refund.
		expect("refund", net("refund 5,000", "/v1/network/authorizations/"+authID+"/refunds", map[string]any{"network_ref": "rf-" + authID, "amount_minor": 5_000}), 201, "")
		if x := acct(); x["outstanding_minor"] != 25_000.0 || x["available_credit_minor"] != 75_000.0 {
			t.Fatalf("after refund: %v", x)
		}
		expect("refund over", net("refund over captured", "/v1/network/authorizations/"+authID+"/refunds", map[string]any{"network_ref": "rf2-" + authID, "amount_minor": 30_000}), 409, "refund_exceeds_captured")

		// Capture rules.
		small := authorize("authorize 1,000 (capture over)", 1_000, nil)
		smallID := small.body["id"].(string)
		expect("capture over", net("capture over authorized", "/v1/network/authorizations/"+smallID+"/capture", map[string]any{"network_ref": "co-" + smallID, "amount_minor": 1_001}), 400, "invalid_capture_amount")
		expect("cleanup reversal", net("reversal (cleanup)", "/v1/network/authorizations/"+smallID+"/reversal", map[string]any{"network_ref": "rvc-" + smallID}), 200, "")
		expect("capture twice", net("capture twice", "/v1/network/authorizations/"+authID+"/capture", map[string]any{"network_ref": "c2-" + authID, "amount_minor": 1}), 409, "already_captured")

		// Declines.
		declined("decline: currency", 100, "currency_not_supported", func(b map[string]any) { b["currency"] = "EUR" })
		declined("decline: insufficient credit", 1_000_000, "insufficient_credit", nil)
		expect("set limit", cust("set purchase limit 1,000", "PUT", "/v1/accounts/"+acc+"/limits", uuid.NewString(), map[string]any{"purchase_limit_minor": 1_000}), 200, "")
		declined("decline: purchase limit", 2_000, "purchase_limit_exceeded", nil)
		expect("clear limit", cust("clear purchase limit", "PUT", "/v1/accounts/"+acc+"/limits", uuid.NewString(), map[string]any{"purchase_limit_minor": 0}), 200, "")
		expect("freeze", cust("freeze", "POST", "/v1/accounts/"+acc+"/freeze", uuid.NewString(), nil), 200, "")
		declined("decline: frozen", 100, "card_frozen", nil)
		expect("unfreeze", cust("unfreeze", "POST", "/v1/accounts/"+acc+"/unfreeze", uuid.NewString(), nil), 200, "")

		// Velocity on a throwaway card: the 31st attempt in a minute is declined.
		vcard, _ := s.activeCard(t, 1_000)
		var last resp
		j.step("velocity: 31 authorizations", func() resp {
			for i := 0; i < 31; i++ {
				last = s.network(t, "/v1/network/authorizations", authBody(vcard, "v-"+uuid.NewString(), 1), networkSecret)
				if i < 30 && last.body["decline_reason"] == "velocity_limit_exceeded" {
					t.Fatalf("velocity hit at attempt %d", i+1)
				}
			}
			return last
		})
		if last.body["decline_reason"] != "velocity_limit_exceeded" {
			t.Fatalf("31st authorization: %s", last.raw)
		}

		// Tenancy: the intruder cannot mutate the customer's account.
		before := acct()
		for _, m := range []struct {
			name, method, path string
			body               any
		}{
			{"tenancy: intruder pays", "POST", "/v1/accounts/" + acc + "/payments", map[string]any{"amount_minor": 100}},
			{"tenancy: intruder sets limit", "PUT", "/v1/accounts/" + acc + "/limits", map[string]any{"purchase_limit_minor": 1}},
			{"tenancy: intruder freezes", "POST", "/v1/accounts/" + acc + "/freeze", nil},
			{"tenancy: intruder advances clock", "POST", "/v1/dev/accounts/" + acc + "/clock/advance", map[string]any{"days": 1}},
		} {
			m := m
			expect(m.name, j.step(m.name, func() resp { return s.call(t, m.method, m.path, xtok, uuid.NewString(), m.body) }), 403, "account_not_authorized")
		}
		after := acct()
		drift := parseTime(t, after["account_time"]).Sub(parseTime(t, before["account_time"]))
		if after["frozen"] != false || after["purchase_limit_minor"] != 0.0 || drift > time.Hour {
			t.Fatalf("intruder changed state (clock drift %s): %v", drift, after)
		}

		// Network authentication.
		expect("unsigned", j.step("network: unsigned", func() resp {
			return s.network(t, "/v1/network/authorizations", authBody(acc, "u-"+uuid.NewString(), 1), nil)
		}), 401, "")
		expect("wrong secret", j.step("network: bank secret", func() resp {
			return s.network(t, "/v1/network/authorizations", authBody(acc, "w-"+uuid.NewString(), 1), bankSecret)
		}), 401, "")

		// Lists.
		expect("page token", cust("payments: tampered page token", "GET", "/v1/accounts/"+acc+"/payments?page_token=AAAA", "", nil), 400, "invalid_page_token")
		l := cust("transactions list", "GET", "/v1/accounts/"+acc+"/transactions", "", nil)
		expect("transactions", l, 200, "")
		seen := map[string]bool{}
		for _, x := range l.body["transactions"].([]any) {
			seen[x.(map[string]any)["id"].(string)] = true
		}
		if !seen[authID] || !seen[rvID] {
			t.Fatalf("transactions missing capture or reversal: %s", l.raw)
		}
	})

	advanceTo := func(name string, at time.Time) map[string]any {
		t.Helper()
		days := daysUntil(t, s.summary(t, acc, tok), at)
		r := cust(name+" (+"+strconv.Itoa(days)+"d)", "POST", "/v1/dev/accounts/"+acc+"/clock/advance", uuid.NewString(), map[string]any{"days": days})
		expect(name, r, 200, "")
		return s.summary(t, acc, tok)
	}
	settlePay := func(name string, amt int64) string {
		t.Helper()
		p := cust(name, "POST", "/v1/accounts/"+acc+"/payments", uuid.NewString(), map[string]any{"amount_minor": amt})
		expect(name, p, 201, "")
		id := p.body["payment"].(map[string]any)["id"].(string)
		expect(name+" settle", j.step(name+": bank settles", func() resp { return s.settle(t, id, amt, "settled") }), 200, "")
		return id
	}

	phase("billing-1", func(t *testing.T) {
		j.begin("billing-1")
		sum := advanceTo("close cycle 1", parseTime(t, s.summary(t, acc, tok)["cycle_end"]))
		st := s.statementList(t, acc, tok)
		if len(st) != 1 || st[0]["new_balance_minor"] != 25_000.0 || st[0]["minimum_due_minor"] != 2_500.0 || st[0]["interest_minor"] != 0.0 || st[0]["fees_minor"] != 0.0 {
			t.Fatalf("statement 1: %v", st)
		}
		// grace reports whether the NEXT cycle is interest-free: not while statement 1 is unpaid.
		if sum["grace"] != false || sum["stage"] != "none" {
			t.Fatalf("summary after statement 1: %v", sum)
		}
		statement1 = st[0]
		j.audit(t, "audit: statement.generated", "statement.generated", st[0]["id"].(string))

		expect("pay below minimum", cust("pay 50 (below 1.00)", "POST", "/v1/accounts/"+acc+"/payments", uuid.NewString(), map[string]any{"amount_minor": 50}), 400, "invalid_amount")
		expect("pay over", cust("pay 1,000,000 (over balance)", "POST", "/v1/accounts/"+acc+"/payments", uuid.NewString(), map[string]any{"amount_minor": 1_000_000}), 409, "payment_exceeds_balance")

		// Idempotent payments: 20 concurrent creates with one key make one payment.
		key := uuid.NewString()
		var wg sync.WaitGroup
		results := make(chan resp, 20)
		j.step("pay 2,500 ×20 concurrent, same key", func() resp {
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results <- s.call(t, "POST", "/v1/accounts/"+acc+"/payments", tok, key, map[string]any{"amount_minor": 2_500})
				}()
			}
			wg.Wait()
			close(results)
			return resp{status: 201}
		})
		authed++
		for r := range results {
			if r.status != 201 {
				t.Fatalf("concurrent payment: %d %s", r.status, r.raw)
			}
			payID = r.body["payment"].(map[string]any)["id"].(string)
		}
		if n := count(t, s.accounts, `SELECT count(*) FROM payments WHERE account_id=$1`, acc); n != 1 {
			t.Fatalf("payments after 20 concurrent creates: %d", n)
		}
		// The same key string under another subject is a different key.
		s.purchase(t, xcard, 1_000)
		xr := j.step("other tenant, same key string", func() resp {
			return s.call(t, "POST", "/v1/accounts/"+xcard+"/payments", xtok, key, map[string]any{"amount_minor": 100})
		})
		if xr.status != 201 || xr.replayed {
			t.Fatalf("cross-subject key: %d replayed=%v %s", xr.status, xr.replayed, xr.raw)
		}

		expect("settle wrong amount", j.step("bank settles wrong amount", func() resp { return s.settle(t, payID, 2_400, "settled") }), 400, "amount_mismatch")
		if acct()["outstanding_minor"] != 25_000.0 {
			t.Fatalf("wrong-amount settle changed the balance: %v", acct())
		}
		expect("settle", j.step("bank settles 2,500", func() resp { return s.settle(t, payID, 2_500, "settled") }), 200, "")
		waitAccount("outstanding 22,500 after settlement", func(a map[string]any) bool { return a["outstanding_minor"] == 22_500.0 })
		j.audit(t, "audit: payment.settled", "payment.settled", payID)

		// The customer spends the freed credit; then the bank returns the payment. Exposure ends
		// above the limit (accepted risk, ADR-018): the card stays restricted until they pay again.
		big := authorize("authorize 76,000 (spend freed credit)", 76_000, nil)
		bigID := big.body["id"].(string)
		expect("capture 76,000", net("capture 76,000", "/v1/network/authorizations/"+bigID+"/capture", map[string]any{"network_ref": "c-" + bigID, "amount_minor": 76_000}), 201, "")
		if x := acct(); x["outstanding_minor"] != 98_500.0 || x["available_credit_minor"] != 1_500.0 {
			t.Fatalf("after spending freed credit: %v", x)
		}
		ret := j.step("bank returns the payment", func() resp {
			return s.bankReq(t, "/v1/webhooks/bank/payments/"+payID+"/return", map[string]any{"return_ref": "ret-" + payID, "amount_minor": 2_500}, bankSecret)
		})
		expect("return", ret, 200, "")
		waitAccount("restricted after return (exposure 101,000 > limit)", func(a map[string]any) bool {
			return a["restricted_reason"] == "payment_returned" && a["outstanding_minor"] == 101_000.0
		})
		declined("decline: payment_returned", 100, "payment_returned", nil)
		expect("return twice", j.step("return twice, other ref", func() resp {
			return s.bankReq(t, "/v1/webhooks/bank/payments/"+payID+"/return", map[string]any{"return_ref": "ret2-" + payID, "amount_minor": 2_500}, bankSecret)
		}), 409, "payment_already_returned")
		settlePay("pay 2,500 again", 2_500)
		waitAccount("restriction lifted", func(a map[string]any) bool {
			return a["restricted_reason"] == nil && a["outstanding_minor"] == 98_500.0
		})
		j.audit(t, "audit: card.restricted", "card.restricted", acc)
		j.audit(t, "audit: card.unrestricted", "card.unrestricted", acc)
	})

	phase("billing-2", func(t *testing.T) {
		j.begin("billing-2")
		advanceTo("close cycle 2", parseTime(t, s.summary(t, acc, tok)["cycle_end"]))
		st := s.statementList(t, acc, tok)
		if len(st) != 2 || st[1]["interest_minor"].(float64) <= 0 || st[1]["fees_minor"] != 0.0 {
			t.Fatalf("statement 2: %v", st)
		}
		if f, i := s.chargeList(t, acc, tok, "fee"), s.chargeList(t, acc, tok, "interest"); len(f) != 0 || len(i) != 1 {
			t.Fatalf("charges after statement 2: fees %v interest %v", f, i)
		}
		j.audit(t, "audit: interest.charged", "interest.charged", s.chargeList(t, acc, tok, "interest")[0]["id"].(string))

		full := int64(acct()["outstanding_minor"].(float64))
		settlePay("pay in full "+strconv.FormatInt(full, 10), full)
		waitAccount("paid in full", func(a map[string]any) bool {
			return a["outstanding_minor"] == 0.0 && a["available_credit_minor"] == 100_000.0
		})
		if b := s.summary(t, acc, tok); b["grace"] != true {
			t.Fatalf("grace after paying in full: %v", b)
		}

		advanceTo("close cycle 3", parseTime(t, s.summary(t, acc, tok)["cycle_end"]))
		st = s.statementList(t, acc, tok)
		if len(st) != 3 || st[2]["new_balance_minor"] != 0.0 || st[2]["interest_minor"] != 0.0 {
			t.Fatalf("statement 3: %v", st[len(st)-1])
		}

		p := authorize("authorize 10,000", 10_000, nil)
		pid := p.body["id"].(string)
		expect("capture 10,000", net("capture 10,000", "/v1/network/authorizations/"+pid+"/capture", map[string]any{"network_ref": "c-" + pid, "amount_minor": 10_000}), 201, "")
		sum := advanceTo("close cycle 4", parseTime(t, s.summary(t, acc, tok)["cycle_end"]))
		st = s.statementList(t, acc, tok)
		if len(st) != 4 || st[3]["new_balance_minor"] != 10_000.0 || st[3]["minimum_due_minor"] != 2_500.0 {
			t.Fatalf("statement 4: %v", st[3])
		}

		// Miss the minimum: 15 days past due → fee, block.
		sum = advanceTo("miss the minimum: due + 15d", parseTime(t, sum["due_date"]).Add(15*dayDur))
		if sum["stage"] != "past_due" || sum["dpd"].(float64) < 15 {
			t.Fatalf("past due: %v", sum)
		}
		fees := s.chargeList(t, acc, tok, "fee")
		if len(fees) != 1 || fees[0]["amount_minor"] != 2_500.0 {
			t.Fatalf("late fee: %v", fees)
		}
		feeID = fees[0]["id"].(string)
		declined("decline: past_due", 100, "past_due", nil)

		// Support boundary.
		waive := "/v1/support/accounts/" + acc + "/charges/" + feeID + "/waivers"
		expect("customer waiver", cust("support boundary: customer waives", "POST", waive, uuid.NewString(), map[string]any{"amount_minor": 2_500, "reason": "self-service"}), 403, "")
		expect("support reads account", j.step("support boundary: support reads account", func() resp { return s.call(t, "GET", "/v1/accounts/"+acc, support, "", nil) }), 403, "account_not_authorized")
		kw := uuid.NewString()
		w := j.step("support waives the fee", func() resp {
			return s.call(t, "POST", waive, support, kw, map[string]any{"amount_minor": 2_500, "reason": "journey goodwill"})
		})
		expect("waiver", w, 201, "")
		assertNoLeak(t, "waiver", w)
		if b := s.summary(t, acc, tok); b["stage"] != "none" {
			t.Fatalf("after waiver: %v", b)
		}
		w2 := j.step("waiver replay, same key", func() resp {
			return s.call(t, "POST", waive, support, kw, map[string]any{"amount_minor": 2_500, "reason": "journey goodwill"})
		})
		if !w2.replayed {
			t.Fatalf("waiver replay: %d %s", w2.status, w2.raw)
		}
		if fees = s.chargeList(t, acc, tok, "fee"); len(fees) != 1 || fees[0]["waived_minor"] != 2_500.0 {
			t.Fatalf("fee after waiver replay: %v", fees)
		}
		j.audit(t, "audit: fee.charged", "fee.charged", feeID)
		j.audit(t, "audit: charge.waived", "charge.waived", feeID)
	})

	phase("charge-off", func(t *testing.T) {
		j.begin("charge-off")
		before := acct()
		r := cust("stop paying (+200d)", "POST", "/v1/dev/accounts/"+acc+"/clock/advance", uuid.NewString(), map[string]any{"days": 200})
		expect("advance 200", r, 200, "")
		waitAccount("charged off", func(a map[string]any) bool { return a["delinquency"] == "charged_off" })
		after := acct()
		if after["credit_limit_minor"].(float64) >= before["credit_limit_minor"].(float64) {
			t.Fatalf("collateral not applied: before %v after %v", before, after)
		}
		// The charge-off statement shows the collateral application; it equals the limit reduction.
		var applied float64
		for _, st := range s.statementList(t, acc, tok) {
			applied += st["collateral_applied_minor"].(float64)
		}
		if lost := before["credit_limit_minor"].(float64) - after["credit_limit_minor"].(float64); applied <= 0 || applied != lost {
			t.Fatalf("collateral applied on statements %v != limit reduction %v", applied, lost)
		}
		declined("decline: charged_off", 100, "charged_off", nil)
		p := cust("payment after charge-off", "POST", "/v1/accounts/"+acc+"/payments", uuid.NewString(), map[string]any{"amount_minor": 100})
		t.Logf("payment after charge-off → %d %s", p.status, errCode(p))
		j.audit(t, "audit: account.charged_off", "account.charged_off", acc)
	})

	phase("closing-checks", func(t *testing.T) {
		j.begin("closing-checks")
		for _, st := range s.statementList(t, acc, tok) {
			id := st["id"].(string)
			r := cust("statement "+formatCycle(st["cycle"]), "GET", "/v1/accounts/"+acc+"/statements/"+id, "", nil)
			expect("statement", r, 200, "")
			if r.body["document"] == nil {
				t.Fatalf("statement %s: no document", id)
			}
			body, err := s.statements.Get(context.Background(), "statements/"+acc+"/"+formatCycle(st["cycle"]))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != st["sha256"] {
				t.Fatalf("statement %s: S3 hash mismatch", id)
			}
		}
		// IDOR through child resources.
		sid := statement1["id"].(string)
		expect("IDOR statement", j.step("IDOR: customer statement under intruder account", func() resp { return s.call(t, "GET", "/v1/accounts/"+xcard+"/statements/"+sid, xtok, "", nil) }), 404, "")
		iw := j.step("IDOR: waiver of customer fee under intruder account", func() resp {
			return s.call(t, "POST", "/v1/support/accounts/"+xcard+"/charges/"+feeID+"/waivers", support, uuid.NewString(), map[string]any{"amount_minor": 1, "reason": "idor"})
		})
		if iw.status == 201 {
			t.Fatalf("waiver accepted across accounts: %s", iw.raw)
		}
		if fees := s.chargeList(t, acc, tok, "fee"); fees[0]["waived_minor"] != 2_500.0 {
			t.Fatalf("fee changed by cross-account waiver: %v", fees)
		}
		// Audit completeness: every authenticated call by the customer left an http.request event.
		if j.until("audit: http.request coverage", 30*time.Second, func() bool {
			return count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type='http.request' AND actor=$1`, sub) >= authed
		}) < 0 {
			t.Fatalf("http.request events for %s: %d < %d calls", sub, count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type='http.request' AND actor=$1`, sub), authed)
		}
		// Rate limit: the 501st call in a minute by one subject is 429.
		burst := token(t, "cust-burst-"+uuid.NewString()[:8])
		var last resp
		var retryAfter string
		j.step("rate limit burst (501 calls)", func() resp {
			for i := 0; i < 501; i++ {
				req, _ := http.NewRequest("GET", s.url+"/v1/accounts/"+uuid.NewString(), nil)
				req.Header.Set("Authorization", "Bearer "+burst)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(res.Body)
				res.Body.Close()
				last = resp{status: res.StatusCode, raw: string(raw)}
				_ = json.Unmarshal(raw, &last.body)
				retryAfter = res.Header.Get("Retry-After")
				if i < 500 && res.StatusCode == 429 {
					t.Fatalf("rate limited at call %d", i+1)
				}
			}
			return last
		})
		if last.status != 429 || errCode(last) != "velocity_limit_exceeded" {
			t.Fatalf("501st call: %d %s", last.status, last.raw)
		}
		if _, err := strconv.Atoi(retryAfter); err != nil {
			t.Fatalf("Retry-After %q", retryAfter)
		}
	})
}
