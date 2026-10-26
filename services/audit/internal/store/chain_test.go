package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/events"
)

func TestRedactKeysAnyDepth(t *testing.T) {
	in := json.RawMessage(`{"authorization":{"id":"a1","amount_minor":500},"payout":{"Account_Number":"000123456789","x":[{"holder_name":"Jane"},{"ok":1}]},"token":"eyJ","n":12345678901234567890}`)
	out, redacted := Redact(in)
	if !redacted {
		t.Fatal("must report redaction")
	}
	s := string(out)
	for _, leak := range []string{"000123456789", "Jane", "eyJ"} {
		if strings.Contains(s, leak) {
			t.Fatalf("leaked %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"authorization":{"amount_minor":500,"id":"a1"}`) {
		t.Fatalf("authorization is a domain object and must be kept: %s", s)
	}
	if !strings.Contains(s, "12345678901234567890") {
		t.Fatalf("numbers must stay exact: %s", s)
	}
	if _, r := Redact(json.RawMessage(`{"a":1}`)); r {
		t.Fatal("nothing to redact")
	}
}

func TestCanonicalIsStableAcrossKeyOrderAndPrecision(t *testing.T) {
	at := time.Date(2027, 1, 2, 3, 4, 5, 123456789, time.UTC)
	a := events.Event{ID: "e1", Type: "t", AggregateType: "x", AggregateID: "1", After: json.RawMessage(`{"b":1,"a":2}`), OccurredAt: at, Source: "ledger", SourceSeq: 7}
	b := a
	b.After = json.RawMessage(`{ "a": 2, "b": 1 }`)
	b.OccurredAt = at.Truncate(time.Microsecond)
	ca, err := Canonical(a, 5)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := Canonical(b, 5)
	if string(ca) != string(cb) {
		t.Fatalf("canonical differs:\n%s\n%s", ca, cb)
	}
	if Hash(GenesisHash, ca) == Hash(GenesisHash, mustCanonical(t, a, 6)) {
		t.Fatal("chain_seq must be part of the hash")
	}
	if len(Hash(GenesisHash, ca)) != 64 {
		t.Fatal("hex sha256")
	}
}

func mustCanonical(t *testing.T, ev events.Event, seq int64) []byte {
	c, err := Canonical(ev, seq)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
