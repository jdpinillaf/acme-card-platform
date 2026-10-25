//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/auth"
)

const dayDur = 24 * time.Hour

func (s *stack) summary(t *testing.T, card, tok string) map[string]any {
	t.Helper()
	r := s.call(t, "GET", "/v1/accounts/"+card+"/billing", tok, "", nil)
	if r.status != 200 {
		t.Fatalf("billing: %d %s", r.status, r.raw)
	}
	return r.body
}

func (s *stack) advance(t *testing.T, card, tok string, days int) map[string]any {
	t.Helper()
	for days > 0 {
		step := min(days, 400)
		r := s.call(t, "POST", "/v1/dev/accounts/"+card+"/clock/advance", tok, uuid.NewString(), map[string]any{"days": step})
		if r.status != 200 {
			t.Fatalf("advance %d: %d %s", step, r.status, r.raw)
		}
		days -= step
	}
	return s.summary(t, card, tok)
}

func parseTime(t *testing.T, v any) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, v.(string))
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// daysUntil is the whole number of days to advance from account time to reach at.
func daysUntil(t *testing.T, sum map[string]any, at time.Time) int {
	return int(math.Ceil(at.Sub(parseTime(t, sum["account_time"])).Hours() / 24))
}

func (s *stack) statementList(t *testing.T, card, tok string) []map[string]any {
	t.Helper()
	r := s.call(t, "GET", "/v1/accounts/"+card+"/statements", tok, "", nil)
	if r.status != 200 {
		t.Fatalf("statements: %d %s", r.status, r.raw)
	}
	var out []map[string]any
	for _, x := range r.body["statements"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func (s *stack) chargeList(t *testing.T, card, tok, kind string) []map[string]any {
	t.Helper()
	r := s.call(t, "GET", "/v1/accounts/"+card+"/charges", tok, "", nil)
	var out []map[string]any
	for _, x := range r.body["charges"].([]any) {
		if c := x.(map[string]any); kind == "" || c["kind"] == kind {
			out = append(out, c)
		}
	}
	return out
}

func TestSprint4EndToEnd(t *testing.T) {
	s := startStack(t)
	card, tok := s.activeCard(t, 100_000)
	s.purchase(t, card, 60_000)

	// Statement 1: grace period, minimum = $25 floor (1% of 60,000 is only 600).
	sum := s.summary(t, card, tok)
	sum = s.advance(t, card, tok, daysUntil(t, sum, parseTime(t, sum["cycle_end"])))
	st := s.statementList(t, card, tok)
	if len(st) != 1 || st[0]["new_balance_minor"] != 60000.0 || st[0]["minimum_due_minor"] != 2500.0 || st[0]["interest_minor"] != 0.0 {
		t.Fatalf("statement 1: %v", st)
	}

	// Pay the minimum on time: no fee, but the full balance was not paid, so cycle 2 accrues interest.
	p := s.pay(t, card, tok, 2500)
	if r := s.settle(t, p, 2500, "settled"); r.status != 200 {
		t.Fatalf("settle: %d %s", r.status, r.raw)
	}
	sum = s.advance(t, card, tok, daysUntil(t, sum, parseTime(t, sum["cycle_end"])))
	st = s.statementList(t, card, tok)
	if len(st) != 2 || st[1]["fees_minor"] != 0.0 || st[1]["interest_minor"].(float64) <= 0 {
		t.Fatalf("statement 2: %v", st[1])
	}
	if n := len(s.chargeList(t, card, tok, "fee")); n != 0 {
		t.Fatalf("fees after an on-time minimum: %d", n)
	}

	// Miss minimum 2: at 15 days past due the card is blocked.
	due2 := parseTime(t, sum["due_date"])
	sum = s.advance(t, card, tok, daysUntil(t, sum, due2.Add(15*dayDur)))
	if sum["stage"] != "past_due" || sum["dpd"].(float64) < 15 {
		t.Fatalf("DPD 15: %v", sum)
	}
	a := s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 100), networkSecret)
	if a.status != 201 || a.body["status"] != "declined" || a.body["decline_reason"] != "past_due" {
		t.Fatalf("authorization while past due: %d %s", a.status, a.raw)
	}

	// Support waives the late fee: the waiver counts toward the past-due minimum and cures it.
	fees := s.chargeList(t, card, tok, "fee")
	if len(fees) != 1 {
		t.Fatalf("fees: %v", fees)
	}
	support, err := auth.SignWithRole(jwtSecret, "agent-e2e", nil, auth.RoleSupport, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	waivePath := "/v1/support/accounts/" + card + "/charges/" + fees[0]["id"].(string) + "/waivers"
	if r := s.call(t, "POST", waivePath, tok, uuid.NewString(), map[string]any{"amount_minor": 2500, "reason": "e2e goodwill"}); r.status != 403 {
		t.Fatalf("customer waiver: %d", r.status)
	}
	if r := s.call(t, "POST", waivePath, support, uuid.NewString(), map[string]any{"amount_minor": 2500, "reason": "e2e goodwill"}); r.status != 201 {
		t.Fatalf("support waiver: %d %s", r.status, r.raw)
	}
	if b := s.summary(t, card, tok); b["stage"] != "none" {
		t.Fatalf("after waiver: %v", b)
	}

	// Stop paying: the collateral is applied at 60 days past due and the card is blocked for good.
	before := s.account(t, card, tok)
	s.advance(t, card, tok, 200)
	after := s.account(t, card, tok)
	if after["delinquency"] != "charged_off" || after["credit_limit_minor"].(float64) >= before["credit_limit_minor"].(float64) {
		t.Fatalf("charge-off: before %v after %v", before, after)
	}
	a = s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 100), networkSecret)
	if a.body["decline_reason"] != "charged_off" {
		t.Fatalf("authorization after charge-off: %s", a.raw)
	}

	// Every statement document matches its stored hash.
	for _, st := range s.statementList(t, card, tok) {
		r := s.call(t, "GET", "/v1/accounts/"+card+"/statements/"+st["id"].(string), tok, "", nil)
		if r.status != 200 || r.body["document"] == nil {
			t.Fatalf("statement detail: %d %s", r.status, r.raw)
		}
		body, err := s.statements.Get(context.Background(), "statements/"+card+"/"+formatCycle(st["cycle"]))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != st["sha256"] {
			t.Fatalf("cycle %v: S3 hash mismatch", st["cycle"])
		}
	}

	// Audit trail (outbox → SNS → SQS → audit).
	deadline := time.Now().Add(30 * time.Second)
	for _, typ := range []string{"statement.generated", "fee.charged", "interest.charged", "charge.waived", "account.charged_off", "account.clock_advanced"} {
		for count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1`, typ) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("no %s in audit", typ)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func formatCycle(v any) string { return strconv.Itoa(int(v.(float64))) + ".json" }

// The same Idempotency-Key ×50 advances the clock once.
func TestSprint4AdvanceIsIdempotent(t *testing.T) {
	s := startStack(t)
	card, tok := s.activeCard(t, 1_000)
	start := parseTime(t, s.summary(t, card, tok)["account_time"])
	key := uuid.NewString()
	var wg sync.WaitGroup
	codes := make(chan int, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.call(t, "POST", "/v1/dev/accounts/"+card+"/clock/advance", tok, key, map[string]any{"days": 10}).status
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 && c != 409 { // 409 request_in_progress while the first is running
			t.Fatalf("status %d", c)
		}
	}
	got := parseTime(t, s.summary(t, card, tok)["account_time"])
	if d := got.Sub(start); d < 10*dayDur || d > 10*dayDur+time.Minute {
		t.Fatalf("clock moved %v, want exactly 10 days", d)
	}
}
