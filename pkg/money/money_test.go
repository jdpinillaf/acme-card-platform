package money

import "testing"

func TestNewRejectsNonPositiveAmount(t *testing.T) {
	for _, amt := range []int64{0, -1} {
		if _, err := New(amt, "USD"); err != ErrNonPositive {
			t.Fatalf("amount %d: want ErrNonPositive, got %v", amt, err)
		}
	}
}

func TestNewRejectsBadCurrency(t *testing.T) {
	for _, cur := range []string{"", "us", "usd", "USDX", "U$D"} {
		if _, err := New(100, cur); err != ErrBadCurrency {
			t.Fatalf("currency %q: want ErrBadCurrency, got %v", cur, err)
		}
	}
}

func TestAddSameCurrency(t *testing.T) {
	a, _ := New(150, "USD")
	b, _ := New(250, "USD")
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AmountMinor != 400 || sum.Currency != "USD" {
		t.Fatalf("got %+v", sum)
	}
}

func TestAddCurrencyMismatch(t *testing.T) {
	a, _ := New(1, "USD")
	b, _ := New(1, "EUR")
	if _, err := a.Add(b); err != ErrCurrencyMismatch {
		t.Fatalf("want ErrCurrencyMismatch, got %v", err)
	}
}

func TestAddOverflow(t *testing.T) {
	a, _ := New(1<<62, "USD")
	b, _ := New(1<<62, "USD")
	if _, err := a.Add(b); err != ErrOverflow {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func TestSubCannotGoNegative(t *testing.T) {
	a, _ := New(100, "USD")
	b, _ := New(101, "USD")
	if _, err := a.Sub(b); err != ErrNegativeResult {
		t.Fatalf("want ErrNegativeResult, got %v", err)
	}
}

func TestValidCurrency(t *testing.T) {
	if !ValidCurrency("COP") || ValidCurrency("cop") {
		t.Fatal("ValidCurrency should accept 3 uppercase letters only")
	}
}
