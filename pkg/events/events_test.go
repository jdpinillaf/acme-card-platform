package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewAssignsIDAndTime(t *testing.T) {
	ev := New("account.opened", "account", "acc-1")
	if ev.ID == "" || ev.Type != "account.opened" || ev.AggregateType != "account" || ev.AggregateID != "acc-1" {
		t.Fatalf("%+v", ev)
	}
	if time.Since(ev.OccurredAt) > time.Minute {
		t.Fatal("OccurredAt should be now")
	}
	if New("x", "y", "z").ID == ev.ID {
		t.Fatal("ids must be unique")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	ev := New("collateral.confirmed", "deposit", "dep-1")
	ev.Actor = "bank"
	ev.HTTPStatus = 200
	ev.After = json.RawMessage(`{"state":"active"}`)
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != ev.ID || back.Actor != "bank" || back.HTTPStatus != 200 || string(back.After) != `{"state":"active"}` {
		t.Fatalf("%+v", back)
	}
}
