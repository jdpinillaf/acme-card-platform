package engine

// Minimum is the minimum payment: 1% of principal + the cycle's interest and fees + any past-due
// minimum, at least the currency floor, never above the new balance; 0 without a balance.
func Minimum(newBalance int64, b Buckets, interestBilled, feesBilled, pastDueAtClose, floor int64) int64 {
	if newBalance <= 0 {
		return 0
	}
	m := max(floor, max(b.Principal, 0)/100+interestBilled+feesBilled+pastDueAtClose)
	return min(newBalance, m)
}
