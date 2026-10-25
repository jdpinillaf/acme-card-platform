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
	StateClosing           State = "closing"
	StateClosed            State = "closed"
	StateCancelled         State = "cancelled"
)

// Event drives a state transition.
type Event string

const (
	EventCollateralConfirmed Event = "collateral_confirmed"
	EventRequestClose        Event = "request_close"
	EventCloseCompleted      Event = "close_completed"
	EventCancel              Event = "cancel"
)

var (
	ErrAccountClosed     = errors.New("account_closed")
	ErrAccountClosing    = errors.New("account_closing")
	ErrInvalidTransition = errors.New("invalid_transition")
)

// Transition returns the next state or an error. closed and cancelled are terminal:
//
//	pending_collateral --collateral_confirmed--> active --request_close--> closing --close_completed--> closed
//	pending_collateral --cancel--> cancelled
func Transition(from State, ev Event) (State, error) {
	switch from {
	case StateClosed, StateCancelled:
		return "", ErrAccountClosed
	case StatePendingCollateral, StateActive, StateClosing:
	default:
		return "", fmt.Errorf("%w: unknown state %q", ErrInvalidTransition, from)
	}
	switch {
	case ev == EventCollateralConfirmed && from != StateClosing:
		return StateActive, nil
	case ev == EventCancel && from == StatePendingCollateral:
		return StateCancelled, nil
	case ev == EventRequestClose && from == StateActive:
		return StateClosing, nil
	case ev == EventCloseCompleted && from == StateClosing:
		return StateClosed, nil
	}
	return "", fmt.Errorf("%w: %s on %s", ErrInvalidTransition, ev, from)
}

// CanAcceptDeposit reports whether collateral may be added in this state.
func CanAcceptDeposit(s State) error {
	switch s {
	case StateClosed, StateCancelled:
		return ErrAccountClosed
	case StateClosing:
		return ErrAccountClosing
	}
	return nil
}
