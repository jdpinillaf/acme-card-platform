// Package accounts implements account/card lifecycle and collateral deposits.
package accounts

import (
	"errors"
	"fmt"
)

// State is the card/account lifecycle state. Frozen is a separate flag, not a state.
type State string

const (
	StatePendingCollateral State = "pending_collateral"
	StateActive            State = "active"
	StateClosed            State = "closed"
)

// Event drives a state transition.
type Event string

const (
	EventCollateralConfirmed Event = "collateral_confirmed"
	EventClose               Event = "close"
)

var (
	ErrAccountClosed     = errors.New("account_closed")
	ErrInvalidTransition = errors.New("invalid_transition")
)

// Transition returns the next state or an error. closed is terminal.
func Transition(from State, ev Event) (State, error) {
	switch from {
	case StateClosed:
		return "", ErrAccountClosed
	case StatePendingCollateral, StateActive:
	default:
		return "", fmt.Errorf("%w: unknown state %q", ErrInvalidTransition, from)
	}
	switch ev {
	case EventCollateralConfirmed:
		return StateActive, nil
	case EventClose:
		return StateClosed, nil
	}
	return "", fmt.Errorf("%w: %s on %s", ErrInvalidTransition, ev, from)
}

// CanAcceptDeposit reports whether collateral may be added in this state.
func CanAcceptDeposit(s State) error {
	if s == StateClosed {
		return ErrAccountClosed
	}
	return nil
}
