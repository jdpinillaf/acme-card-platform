//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/auth"
)

var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.`)

// auditedEventually waits until the audit log has an eventType row naming account.
func (s *stack) auditedEventually(t *testing.T, eventType, account string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for count(t, s.audit, `SELECT count(*) FROM audit_events WHERE event_type=$1 AND ($2='' OR aggregate_id=$2 OR after->>'account_id'=$2)`, eventType, account) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s for %s never reached the audit log", eventType, account)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSprint6AuditorEndToEnd(t *testing.T) {
	s := startStack(t)
	auditor, _ := auth.SignWithRole(jwtSecret, "auditor-e2e", nil, auth.RoleAuditor, time.Hour)

	// a full life: open, buy, close with a payout destination, pay out
	card, tok := s.activeCard(t, 1_000)
	s.purchase(t, card, 200)
	if r := s.call(t, "POST", "/v1/accounts/"+card+"/close", tok, uuid.NewString(), map[string]any{"destination": destination}); r.status != 202 {
		t.Fatalf("close: %d %s", r.status, r.raw)
	}
	s.advance(t, card, tok, 23)
	po := s.openPayout(t, card, tok)
	if r := s.payOut(t, po, "settled"); r.status != 200 {
		t.Fatalf("payout: %d %s", r.status, r.raw)
	}
	s.auditedEventually(t, "account.closed", card)
	s.auditedEventually(t, "payout.instruction_read", card)

	t.Run("customer and support tokens are refused, and the refusal is audited", func(t *testing.T) {
		support, _ := auth.SignWithRole(jwtSecret, "agent-e2e", nil, auth.RoleSupport, time.Hour)
		for _, tk := range []string{tok, support} {
			if r := s.call(t, "GET", "/v1/audit/events?account_id="+card, tk, "", nil); r.status != 403 {
				t.Fatalf("non-auditor: %d %s", r.status, r.raw)
			}
		}
		s.auditedEventually(t, "audit.access_denied", "")
	})

	t.Run("search by account returns its chained trail", func(t *testing.T) {
		r := s.call(t, "GET", "/v1/audit/events?page_size=500&account_id="+card, auditor, "", nil)
		if r.status != 200 {
			t.Fatalf("search: %d %s", r.status, r.raw)
		}
		types := map[string]bool{}
		for _, e := range r.body["events"].([]any) {
			ev := e.(map[string]any)
			types[ev["event_type"].(string)] = true
			if len(ev["hash"].(string)) != 64 || ev["chain_seq"].(float64) < 1 {
				t.Fatalf("unchained event: %v", ev)
			}
		}
		for _, want := range []string{"account.opened", "account.changed", "account.closed", "payout.instruction_read"} {
			if !types[want] {
				t.Errorf("search misses %s (got %v)", want, types)
			}
		}
		if strings.Contains(r.raw, "000123456789") || strings.Contains(r.raw, "Jane Doe") {
			t.Fatalf("search leaks the payout destination: %s", r.raw)
		}
	})

	t.Run("verify, checkpoint, reconciliation and replay are clean", func(t *testing.T) {
		if r := s.call(t, "GET", "/v1/audit/verify", auditor, "", nil); r.status != 200 || r.body["chain_valid"] != true ||
			len(r.body["gaps"].([]any)) != 0 || len(r.body["stuck_outbox"].([]any)) != 0 {
			t.Fatalf("verify: %d %s", r.status, r.raw)
		}
		if r := s.call(t, "POST", "/v1/audit/checkpoints", auditor, uuid.NewString(), nil); r.status != 201 || r.body["chain_seq"].(float64) < 1 {
			t.Fatalf("checkpoint: %d %s", r.status, r.raw)
		}
		if r := s.call(t, "POST", "/v1/audit/reconciliation", auditor, uuid.NewString(), nil); r.status != 201 || r.body["total_violations"] != 0.0 {
			t.Fatalf("reconciliation: %d %s", r.status, r.raw)
		}
		if r := s.call(t, "GET", "/v1/audit/replay/"+card, auditor, "", nil); r.status != 200 || len(r.body["differences"].([]any)) != 0 {
			t.Fatalf("replay: %d %s", r.status, r.raw)
		}
	})

	t.Run("export holds no secrets and matches its sha256", func(t *testing.T) {
		r := s.call(t, "POST", "/v1/audit/exports", auditor, uuid.NewString(), map[string]any{"filter": map[string]any{"account_id": card}})
		if r.status != 201 || r.body["count"].(float64) < 5 {
			t.Fatalf("export: %d %s", r.status, r.raw)
		}
		req, _ := http.NewRequest("GET", s.url+"/v1/audit/exports/"+r.body["id"].(string), nil)
		req.Header.Set("Authorization", "Bearer "+auditor)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		sum := sha256.Sum256(body)
		if res.StatusCode != 200 || hex.EncodeToString(sum[:]) != r.body["sha256"] {
			t.Fatalf("download: %d sha mismatch", res.StatusCode)
		}
		for _, secret := range []string{"000123456789", "Jane Doe", tok, auditor} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("export leaks %q", secret[:min(len(secret), 12)])
			}
		}
		if jwtLike.Match(body) {
			t.Fatal("export contains a JWT")
		}
		s.auditedEventually(t, "audit.exported", "")
	})

	t.Run("a tampered row is named by verify", func(t *testing.T) {
		var seq int64
		if err := s.audit.QueryRow(context.Background(), `SELECT chain_seq FROM audit_events WHERE event_type='account.closed' AND aggregate_id=$1`, card).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		tamper(t, s, `UPDATE audit_events SET actor='mallory' WHERE chain_seq=$1`, seq)
		r := s.call(t, "GET", "/v1/audit/verify", auditor, "", nil)
		if r.status != 200 || r.body["chain_valid"] != false || r.body["first_bad_chain_seq"] != float64(seq) {
			t.Fatalf("verify after tamper (seq %d): %d %s", seq, r.status, r.raw)
		}
	})
}

// tamper runs sql on the audit schema with triggers disabled (the test role is a superuser).
func tamper(t *testing.T, s *stack, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.audit.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []string{`SET session_replication_role = replica`, sql, `SET session_replication_role = origin`} {
		a := args
		if q != sql {
			a = nil
		}
		if _, err := conn.Exec(ctx, q, a...); err != nil {
			t.Fatal(err)
		}
	}
}
