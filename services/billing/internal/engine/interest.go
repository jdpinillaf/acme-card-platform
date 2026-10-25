package engine

import "time"

// Interest is the cycle's interest: per day, the day's base × APR / 365 in micro-units (integer
// division per day), summed and floored to minor units. The base is the positive principal when
// grace is lost, and the whole positive balance (principal + interest + fees) on past-due days.
func Interest(days []DayBalance, aprBps int64, grace bool, pastDue func(day time.Time) bool) int64 {
	var micro int64
	for _, d := range days {
		var base int64
		switch {
		case pastDue(d.Day):
			base = d.Buckets.Total()
		case !grace:
			base = d.Buckets.Principal
		}
		if base <= 0 {
			continue
		}
		// base × apr_bps × 1_000_000 / (10_000 × 365), reduced to avoid overflow.
		micro += base * aprBps * 100 / 365
	}
	return micro / 1_000_000
}
