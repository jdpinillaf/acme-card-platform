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
	PostedAt   time.Time // business date it reached the ledger; zero while pending
	Amount     int64
	Pending    bool
}

// anchor moves a payment received before a close but not reflected in that statement's balance
// (still pending at the close, or posted after it) to the close itself, so it pays toward that
// statement's minimum and grace (review C1). A payment already inside the previous statement's
// on-time window stays there: it pays that statement, once. stmts must be sorted by cycle.
func anchor(stmts []StatementFacts, cs []Credit) []Credit {
	out := make([]Credit, len(cs))
	for i, c := range cs {
		out[i] = c
		for j, s := range stmts {
			if !c.ReceivedAt.Before(s.Close) {
				continue
			}
			inPrevWindow := j > 0 && c.ReceivedAt.Before(OnTimeCutoff(stmts[j-1].Due))
			if !inPrevWindow && (c.Pending || (!c.PostedAt.IsZero() && !c.PostedAt.Before(s.Close))) {
				out[i].ReceivedAt = s.Close
			}
			break
		}
	}
	return out
}

// DelinquencyResult is the derived state at a moment (spec B-2).
type DelinquencyResult struct {
	PastDue int64
	DPD     int
	Missed  []Miss // statements that missed their due date with nothing pending: each owes one late fee
}

// Miss is a missed statement and the unpaid part of its minimum at the on-time cutoff.
type Miss struct {
	Cycle     int
	Shortfall int64
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

// missed reports whether s's minimum was not covered by its on-time cutoff (pending counts as
// paid), and by how much.
func missed(s StatementFacts, cs []Credit) (bool, bool, int64) {
	got, pending := credited(cs, s.Close, OnTimeCutoff(s.Due))
	return got < s.Minimum, pending, s.Minimum - got
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
	credits = anchor(sortedFacts(stmts), credits)
	var r DelinquencyResult
	for _, s := range due {
		if m, pending, short := missed(s, credits); m && !pending {
			r.Missed = append(r.Missed, Miss{Cycle: s.Cycle, Shortfall: short})
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
		if m, _, _ := missed(due[k-1], credits); !m {
			break
		}
		k--
	}
	r.DPD = int(now.Sub(OnTimeCutoff(due[k].Due))/dayDur) + 1
	return r
}

// Grace reports whether the cycle after the last of stmts is interest-free on purchases: the first
// cycle, a previous new balance ≤ 0, or a previous new balance paid in full by its on-time cutoff
// (pending counts).
func Grace(stmts []StatementFacts, credits []Credit) bool {
	if len(stmts) == 0 {
		return true
	}
	sorted := sortedFacts(stmts)
	prev := sorted[len(sorted)-1]
	if prev.NewBalance <= 0 {
		return true
	}
	got, _ := credited(anchor(sorted, credits), prev.Close, OnTimeCutoff(prev.Due))
	return got >= prev.NewBalance
}

func sortedFacts(stmts []StatementFacts) []StatementFacts {
	out := append([]StatementFacts(nil), stmts...)
	sort.Slice(out, func(i, j int) bool { return out[i].Cycle < out[j].Cycle })
	return out
}

// CapToOwed limits the past-due amount to what is owed now (receivable, ≤ 0 meaning nothing):
// with nothing owed the account is not past due, whatever cleared the debt. Owed late fees
// (Missed) are unaffected.
func CapToOwed(r DelinquencyResult, owed int64) DelinquencyResult {
	r.PastDue = min(r.PastDue, max(owed, 0))
	if r.PastDue == 0 {
		r.DPD = 0
	}
	return r
}
