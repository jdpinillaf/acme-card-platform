// Package money provides exact monetary arithmetic in integer minor units.
// No floats, ever. Currency is an ISO-4217 code (three uppercase letters).
package money

import (
	"errors"
	"fmt"
	"math"
)

var (
	ErrNonPositive      = errors.New("money: amount must be positive")
	ErrBadCurrency      = errors.New("money: currency must be 3 uppercase letters")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: int64 overflow")
	ErrNegativeResult   = errors.New("money: result would be negative")
)

// Money is an exact amount in minor units of a currency.
type Money struct {
	AmountMinor int64
	Currency    string
}

// New validates and builds a positive Money.
func New(amountMinor int64, currency string) (Money, error) {
	if amountMinor <= 0 {
		return Money{}, ErrNonPositive
	}
	if !ValidCurrency(currency) {
		return Money{}, ErrBadCurrency
	}
	return Money{AmountMinor: amountMinor, Currency: currency}, nil
}

// ValidCurrency reports whether s looks like an ISO-4217 code.
func ValidCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// Add returns m + o, failing on currency mismatch or overflow.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if o.AmountMinor > 0 && m.AmountMinor > math.MaxInt64-o.AmountMinor {
		return Money{}, ErrOverflow
	}
	return Money{AmountMinor: m.AmountMinor + o.AmountMinor, Currency: m.Currency}, nil
}

// Sub returns m - o, failing if the result would be negative.
func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	if o.AmountMinor > m.AmountMinor {
		return Money{}, ErrNegativeResult
	}
	return Money{AmountMinor: m.AmountMinor - o.AmountMinor, Currency: m.Currency}, nil
}

func (m Money) String() string { return fmt.Sprintf("%d %s", m.AmountMinor, m.Currency) }
