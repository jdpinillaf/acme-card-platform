package accounts

import "testing"

func TestTransitions(t *testing.T) {
	ok := []struct {
		from State
		ev   Event
		to   State
	}{
		{StatePendingCollateral, EventCollateralConfirmed, StateActive},
		{StateActive, EventCollateralConfirmed, StateActive},
		{StatePendingCollateral, EventClose, StateClosed},
		{StateActive, EventClose, StateClosed},
	}
	for _, c := range ok {
		got, err := Transition(c.from, c.ev)
		if err != nil || got != c.to {
			t.Errorf("%s --%s--> want %s, got %s (%v)", c.from, c.ev, c.to, got, err)
		}
	}
	for _, ev := range []Event{EventCollateralConfirmed, EventClose} {
		if _, err := Transition(StateClosed, ev); err != ErrAccountClosed {
			t.Errorf("closed --%s--> want ErrAccountClosed, got %v", ev, err)
		}
	}
	if _, err := Transition("bogus", EventClose); err == nil {
		t.Error("unknown state must error")
	}
}

func TestCanAcceptDeposit(t *testing.T) {
	if CanAcceptDeposit(StateClosed) != ErrAccountClosed {
		t.Fatal("closed rejects deposits")
	}
	if CanAcceptDeposit(StatePendingCollateral) != nil || CanAcceptDeposit(StateActive) != nil {
		t.Fatal("pending and active accept deposits (top-ups grow the limit)")
	}
}
