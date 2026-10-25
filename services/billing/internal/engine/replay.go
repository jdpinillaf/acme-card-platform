package engine

import (
	"strings"
	"time"
)

// Entry is a ledger journal entry as billing sees it: its signed receivable movement.
type Entry struct {
	ID, Reference, Type string
	EffectiveAt         time.Time
	Delta               int64
}

// Buckets splits the debt (ledger receivable) into principal, interest and fees.
type Buckets struct{ Principal, Interest, Fees int64 }

// Total is the receivable the buckets add up to.
func (b Buckets) Total() int64 { return b.Principal + b.Interest + b.Fees }

func (b Buckets) add(o Buckets) Buckets {
	return Buckets{b.Principal + o.Principal, b.Interest + o.Interest, b.Fees + o.Fees}
}

// DayBalance is the buckets at the end of one UTC day.
type DayBalance struct {
	Day     time.Time
	Buckets Buckets
}

// Totals are a period's movements by statement line.
type Totals struct{ Purchases, Refunds, Payments, Returns, Interest, Fees, Waivers, CollateralApplied int64 }

// ReplayResult is the state at the window's edges plus its per-day balances and totals.
type ReplayResult struct {
	Opening, Closing Buckets
	Days             []DayBalance
	Totals           Totals
}

type state struct {
	b     Buckets
	alloc map[string]Buckets // what each payment / collateral application reduced, for returns
}

// apply moves the buckets for one entry and returns the change.
func (s *state) apply(e Entry) Buckets {
	var ch Buckets
	switch e.Type {
	case "interest_charged", "interest_waived":
		ch.Interest = e.Delta
	case "fee_charged", "fee_waived":
		ch.Fees = e.Delta
	case "payment_settled", "collateral_applied":
		// Fees, then interest, then principal; any excess makes principal negative (credit).
		left := -e.Delta
		take := func(have int64) int64 {
			n := min(max(have, 0), left)
			left -= n
			return n
		}
		ch.Fees = -take(s.b.Fees)
		ch.Interest = -take(s.b.Interest)
		ch.Principal = -left
		s.alloc[e.Reference] = ch
	case "payment_returned":
		if orig, ok := s.alloc["payment:"+strings.TrimPrefix(e.Reference, "payment_return:")]; ok && orig.Total() == -e.Delta {
			ch = Buckets{-orig.Principal, -orig.Interest, -orig.Fees}
		} else {
			ch.Principal = e.Delta
		}
	default: // purchase_captured, purchase_refunded and anything else touching receivable
		ch.Principal = e.Delta
	}
	s.b = s.b.add(ch)
	return ch
}

func (t *Totals) count(e Entry) {
	switch e.Type {
	case "purchase_captured":
		t.Purchases += e.Delta
	case "purchase_refunded":
		t.Refunds -= e.Delta
	case "payment_settled":
		t.Payments -= e.Delta
	case "payment_returned":
		t.Returns += e.Delta
	case "interest_charged":
		t.Interest += e.Delta
	case "fee_charged":
		t.Fees += e.Delta
	case "interest_waived", "fee_waived":
		t.Waivers -= e.Delta
	case "collateral_applied":
		t.CollateralApplied -= e.Delta
	default:
		t.Purchases += e.Delta
	}
}

// Replay runs the account's whole journal (ordered by effective_at, id) and reports the window
// [from, to): opening and closing buckets, end-of-day balances for each UTC day that starts in the
// window (the first day may be partial), and the window's totals. Allocations need the full
// history because a return may arrive cycles after its payment.
func Replay(entries []Entry, from, to time.Time) ReplayResult {
	s := &state{alloc: map[string]Buckets{}}
	var r ReplayResult
	i := 0
	for ; i < len(entries) && entries[i].EffectiveAt.Before(from); i++ {
		if entries[i].Delta != 0 {
			s.apply(entries[i])
		}
	}
	r.Opening = s.b
	for day := startOfDay(from); day.Before(to); day = day.Add(dayDur) {
		end := earliest(day.Add(dayDur), to)
		for ; i < len(entries) && entries[i].EffectiveAt.Before(end); i++ {
			if entries[i].Delta != 0 {
				s.apply(entries[i])
				r.Totals.count(entries[i])
			}
		}
		r.Days = append(r.Days, DayBalance{Day: day, Buckets: s.b})
	}
	r.Closing = s.b
	return r
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
