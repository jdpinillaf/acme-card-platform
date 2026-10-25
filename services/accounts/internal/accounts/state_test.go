package accounts

import (
	"errors"
	"testing"
)

func TestTransitions(t *testing.T) {
	ok := []struct {
		from State
		ev   Event
		to   State
	}{
		{StatePendingCollateral, EventCollateralConfirmed, StateActive},
		{StateActive, EventCollateralConfirmed, StateActive},
		{StatePendingCollateral, EventCancel, StateCancelled},
		{StateActive, EventRequestClose, StateClosing},
		{StateClosing, EventCloseCompleted, StateClosed},
		{StateClosing, EventCollateralConfirmed, StateClosing},
	}
	for _, c := range ok {
		got, err := Transition(c.from, c.ev)
		if err != nil || got != c.to {
			t.Errorf("%s --%s--> want %s, got %s (%v)", c.from, c.ev, c.to, got, err)
		}
	}
	bad := []struct {
		from State
		ev   Event
	}{
		{StateActive, EventCancel},
		{StatePendingCollateral, EventRequestClose},
		{StateActive, EventCloseCompleted},
		{StateClosing, EventCancel},
	}
	for _, c := range bad {
		if _, err := Transition(c.from, c.ev); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrAccountClosed) {
			t.Errorf("%s --%s--> must fail, got %v", c.from, c.ev, err)
		}
	}
	for _, terminal := range []State{StateClosed, StateCancelled} {
		for _, ev := range []Event{EventCollateralConfirmed, EventRequestClose, EventCancel, EventCloseCompleted} {
			if _, err := Transition(terminal, ev); err != ErrAccountClosed {
				t.Errorf("%s --%s--> want ErrAccountClosed, got %v", terminal, ev, err)
			}
		}
	}
	if _, err := Transition("bogus", EventCancel); err == nil {
		t.Error("unknown state must error")
	}
}

func TestCanAcceptDeposit(t *testing.T) {
	for _, s := range []State{StateClosed, StateCancelled} {
		if CanAcceptDeposit(s) != ErrAccountClosed {
			t.Fatalf("%s rejects deposits", s)
		}
	}
	if CanAcceptDeposit(StateClosing) != ErrAccountClosing {
		t.Fatal("closing rejects deposits with account_closing")
	}
	if CanAcceptDeposit(StatePendingCollateral) != nil || CanAcceptDeposit(StateActive) != nil {
		t.Fatal("pending and active accept deposits (top-ups grow the limit)")
	}
}
