// Package engine holds billing's pure rules (ADR-019): cycle dates, bucket replay and payment
// allocation, daily interest, minimum payment, delinquency derivation and statement rendering.
// Nothing here does I/O; every amount is int64 minor units.
package engine

import "time"

const dayDur = 24 * time.Hour

// GraceDays separates a statement close from its due date.
const GraceDays = 21

// anchorDay is the cycle day of month: the activation day, with 29–31 becoming 28.
func anchorDay(activatedAt time.Time) int { return min(activatedAt.UTC().Day(), 28) }

// CycleClose is close_n: 00:00 UTC on the anchor day of the n-th month after activation. close_0 is
// the activation instant itself.
func CycleClose(activatedAt time.Time, n int) time.Time {
	if n == 0 {
		return activatedAt.UTC()
	}
	a := activatedAt.UTC()
	return time.Date(a.Year(), a.Month()+time.Month(n), anchorDay(a), 0, 0, 0, 0, time.UTC)
}

// DueDate is the statement close plus 21 days.
func DueDate(close time.Time) time.Time { return close.AddDate(0, 0, GraceDays) }

// OnTimeCutoff is the end of the due date: a payment received before it is on time.
func OnTimeCutoff(due time.Time) time.Time { return due.Add(dayDur) }

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
