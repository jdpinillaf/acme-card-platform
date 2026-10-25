package engine

import (
	"sort"
	"time"
)

// StatementFacts is what delinquency and grace need from a closed statement.
type StatementFacts struct {
	Cycle       int
	Close, Due  time.Time
	Minimum     int64
	NewBalance  int64
	BalanceOnly bool // after charge-off: no minimum, no due date
}

// Credit is something that pays toward a minimum: a payment (settled and not returned, or still
// pending) by its received time, or a waiver by its creation time. Refunds never count.
type Credit struct {
	ReceivedAt time.Time
	Amount     int64
	Pending    bool
}

// DelinquencyResult is the derived state at a moment (spec B-2).
type DelinquencyResult struct {
	PastDue int64
	DPD     int
	Missed  []int // cycles that missed their due date with nothing pending: each owes one late fee
}

func credited(cs []Credit, from, to time.Time) (sum int64, pending bool) {
	for _, c := range cs {
		if !c.ReceivedAt.Before(from) && c.ReceivedAt.Before(to) {
			sum += c.Amount
			pending = pending || c.Pending
		}
	}
	return
}

// missed reports whether s's minimum was not covered by its on-time cutoff (pending counts as paid).
func missed(s StatementFacts, cs []Credit) (bool, bool) {
	got, pending := credited(cs, s.Close, OnTimeCutoff(s.Due))
	return got < s.Minimum, pending
}

// Delinquency derives past-due amount, DPD and owed late fees at now. Minimums cumulate (each one
// includes the past-due minimum), so only L — the latest statement past its cutoff — decides the
// past-due amount; DPD counts from the oldest statement of the unbroken missed run ending at L.
func Delinquency(stmts []StatementFacts, credits []Credit, now time.Time) DelinquencyResult {
	var due []StatementFacts
	for _, s := range stmts {
		if !s.BalanceOnly && !OnTimeCutoff(s.Due).After(now) {
			due = append(due, s)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].Cycle < due[j].Cycle })
	var r DelinquencyResult
	for _, s := range due {
		if m, pending := missed(s, credits); m && !pending {
			r.Missed = append(r.Missed, s.Cycle)
		}
	}
	if len(due) == 0 {
		return r
	}
	l := due[len(due)-1]
	got, _ := credited(credits, l.Close, now.Add(time.Nanosecond))
	r.PastDue = max(0, l.Minimum-got)
	if r.PastDue == 0 {
		return r
	}
	k := len(due) - 1
	for k > 0 {
		if m, _ := missed(due[k-1], credits); !m {
			break
		}
		k--
	}
	r.DPD = int(now.Sub(OnTimeCutoff(due[k].Due))/dayDur) + 1
	return r
}

// Grace reports whether the next cycle is interest-free on purchases: the first cycle, a previous
// new balance ≤ 0, or a previous new balance paid in full by its on-time cutoff (pending counts).
func Grace(prev *StatementFacts, credits []Credit) bool {
	if prev == nil || prev.NewBalance <= 0 {
		return true
	}
	got, _ := credited(credits, prev.Close, OnTimeCutoff(prev.Due))
	return got >= prev.NewBalance
}
