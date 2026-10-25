//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

func TestJournalMarkdown(t *testing.T) {
	j := &journal{steps: []stepRecord{
		{Phase: "onboarding", Name: "open account", Kind: "http", Status: 201, Dur: 12 * time.Millisecond},
		{Phase: "onboarding", Name: "card active in audit", Kind: "async", Dur: 340 * time.Millisecond},
		{Phase: "spending", Name: "authorize", Kind: "http", Status: 201, Dur: 30 * time.Millisecond},
	}}
	md := j.markdown()
	for _, want := range []string{
		"| Phase | Step | Kind | Status | ms |",
		"| onboarding | open account | http | 201 | 12 |",
		"| onboarding | card active in audit | async | - | 340 |",
		"p50", "p95", "max",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	// http percentiles over {12, 30}: p50 = 12, p95 = 30, max = 30
	if !strings.Contains(md, "p50 12 ms") || !strings.Contains(md, "p95 30 ms") || !strings.Contains(md, "max 30 ms") {
		t.Fatalf("http percentiles wrong:\n%s", md)
	}
}
