//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"

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
