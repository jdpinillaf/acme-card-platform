// Package ledger implements the double-entry, append-only ledger gRPC service.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

// ErrInvalidPostings marks a journal that violates double-entry rules.
var ErrInvalidPostings = errors.New("ledger: invalid postings")

var subAccountNames = map[ledgerv1.SubAccount]string{
	ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING:    "cash_clearing",
	ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD:  "collateral_held",
	ledgerv1.SubAccount_SUB_ACCOUNT_CREDIT_AVAILABLE: "credit_available",
	ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE:       "receivable",
	ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME:  "interest_income",
	ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME:       "fee_income",
}

// postable sub-accounts. credit_available is derived, never posted (see ADR / ledger ruling).
var postable = []ledgerv1.SubAccount{
	ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING,
	ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD,
	ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE,
	ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME,
	ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME,
}

func subAccountFromName(s string) ledgerv1.SubAccount {
	for k, v := range subAccountNames {
		if v == s {
			return k
		}
	}
	return ledgerv1.SubAccount_SUB_ACCOUNT_UNSPECIFIED
}

func isPostable(sa ledgerv1.SubAccount) bool {
	for _, p := range postable {
		if p == sa {
			return true
		}
	}
	return false
}

// ValidatePostings enforces: ≥2 legs, positive amounts, known postable sub-accounts, explicit
// side, no overflow, Σdebit == Σcredit.
func ValidatePostings(ps []*ledgerv1.Posting) error {
	if len(ps) < 2 {
		return fmt.Errorf("%w: need at least two postings", ErrInvalidPostings)
	}
	var debit, credit int64
	for i, p := range ps {
		if p.AmountMinor <= 0 {
			return fmt.Errorf("%w: posting %d amount must be positive", ErrInvalidPostings, i)
		}
		if !isPostable(p.SubAccount) {
			return fmt.Errorf("%w: posting %d sub-account %s not postable", ErrInvalidPostings, i, p.SubAccount)
		}
		total := &debit
		switch p.Side {
		case ledgerv1.Side_SIDE_DEBIT:
		case ledgerv1.Side_SIDE_CREDIT:
			total = &credit
		default:
			return fmt.Errorf("%w: posting %d has no side", ErrInvalidPostings, i)
		}
		if *total > math.MaxInt64-p.AmountMinor {
			return fmt.Errorf("%w: overflow", ErrInvalidPostings)
		}
		*total += p.AmountMinor
	}
	if debit != credit {
		return fmt.Errorf("%w: debits %d != credits %d", ErrInvalidPostings, debit, credit)
	}
	return nil
}

// NormalBalance converts a raw (Σdebit − Σcredit) balance to the sub-account's natural sign.
// collateral_held is a liability to the customer and the income accounts are revenue, hence
// credit-normal.
func NormalBalance(sa ledgerv1.SubAccount, raw int64) int64 {
	switch sa {
	case ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD, ledgerv1.SubAccount_SUB_ACCOUNT_INTEREST_INCOME, ledgerv1.SubAccount_SUB_ACCOUNT_FEE_INCOME:
		return -raw
	}
	return raw
}

// PostingsHash fingerprints a journal independent of leg order, to detect reference reuse.
func PostingsHash(entryType string, ps []*ledgerv1.Posting) string {
	legs := make([]string, len(ps))
	for i, p := range ps {
		legs[i] = fmt.Sprintf("%d:%d:%d", p.SubAccount, p.Side, p.AmountMinor)
	}
	sort.Strings(legs)
	sum := sha256.Sum256([]byte(entryType + "|" + strings.Join(legs, ",")))
	return hex.EncodeToString(sum[:])
}
