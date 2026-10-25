package ledger

import (
	"errors"
	"testing"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
)

func p(sa ledgerv1.SubAccount, side ledgerv1.Side, amt int64) *ledgerv1.Posting {
	return &ledgerv1.Posting{SubAccount: sa, Side: side, AmountMinor: amt}
}

const (
	cash = ledgerv1.SubAccount_SUB_ACCOUNT_CASH_CLEARING
	coll = ledgerv1.SubAccount_SUB_ACCOUNT_COLLATERAL_HELD
	recv = ledgerv1.SubAccount_SUB_ACCOUNT_RECEIVABLE
	avl  = ledgerv1.SubAccount_SUB_ACCOUNT_CREDIT_AVAILABLE
	dr   = ledgerv1.Side_SIDE_DEBIT
	cr   = ledgerv1.Side_SIDE_CREDIT
)

func TestValidatePostingsBalanced(t *testing.T) {
	if err := ValidatePostings([]*ledgerv1.Posting{p(cash, dr, 100), p(coll, cr, 100)}); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePostingsRejects(t *testing.T) {
	cases := map[string][]*ledgerv1.Posting{
		"empty":       nil,
		"single":      {p(cash, dr, 100)},
		"unbalanced":  {p(cash, dr, 100), p(coll, cr, 99)},
		"zero":        {p(cash, dr, 0), p(coll, cr, 0)},
		"negative":    {p(cash, dr, -5), p(coll, cr, -5)},
		"no side":     {p(cash, ledgerv1.Side_SIDE_UNSPECIFIED, 1), p(coll, cr, 1)},
		"no account":  {p(ledgerv1.SubAccount_SUB_ACCOUNT_UNSPECIFIED, dr, 1), p(coll, cr, 1)},
		"memo posted": {p(avl, dr, 1), p(coll, cr, 1)},
		"overflow":    {p(cash, dr, 1<<62), p(cash, dr, 1<<62), p(coll, cr, 1<<62), p(coll, cr, 1<<62)},
	}
	for name, ps := range cases {
		if err := ValidatePostings(ps); !errors.Is(err, ErrInvalidPostings) {
			t.Errorf("%s: want ErrInvalidPostings, got %v", name, err)
		}
	}
}

func TestNormalBalanceSign(t *testing.T) {
	// raw = Σdebit − Σcredit. Liabilities (collateral_held) are credit-normal.
	if NormalBalance(coll, -500) != 500 || NormalBalance(cash, 500) != 500 || NormalBalance(recv, 70) != 70 {
		t.Fatal("normal-balance conversion wrong")
	}
}

func TestPostingsHashOrderInsensitive(t *testing.T) {
	a := PostingsHash("deposit_settled", []*ledgerv1.Posting{p(cash, dr, 100), p(coll, cr, 100)})
	b := PostingsHash("deposit_settled", []*ledgerv1.Posting{p(coll, cr, 100), p(cash, dr, 100)})
	c := PostingsHash("deposit_settled", []*ledgerv1.Posting{p(cash, dr, 101), p(coll, cr, 101)})
	if a != b || a == c {
		t.Fatalf("a=%s b=%s c=%s", a, b, c)
	}
}
