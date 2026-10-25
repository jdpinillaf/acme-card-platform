package accounts

import "testing"

func okInput() DeclineInput {
	return DeclineInput{State: StateActive, AccountCurrency: "USD", Currency: "USD", VelocityOK: true, PurchaseLimit: 0, Amount: 100}
}

func TestEvaluateDeclineEachReasonAlone(t *testing.T) {
	cases := map[DeclineReason]func(*DeclineInput){
		DeclineAccountClosed:          func(in *DeclineInput) { in.State = StateClosed },
		DeclineCollateralNotConfirmed: func(in *DeclineInput) { in.State = StatePendingCollateral },
		DeclineCurrencyNotSupported:   func(in *DeclineInput) { in.Currency = "COP" },
		DeclineCardFrozen:             func(in *DeclineInput) { in.Frozen = true },
		DeclineVelocity:               func(in *DeclineInput) { in.VelocityOK = false },
		DeclinePurchaseLimit:          func(in *DeclineInput) { in.PurchaseLimit = 99 },
	}
	for want, mutate := range cases {
		in := okInput()
		mutate(&in)
		if got := EvaluateDecline(in); got != want {
			t.Errorf("want %s got %q", want, got)
		}
	}
	if got := EvaluateDecline(okInput()); got != "" {
		t.Fatalf("clean input must pass policy, got %s", got)
	}
	in := okInput()
	in.PurchaseLimit = 100 // equal to amount is allowed
	if got := EvaluateDecline(in); got != "" {
		t.Fatalf("amount == purchase limit must pass, got %s", got)
	}
}

func TestEvaluateDeclinePrecedence(t *testing.T) {
	in := DeclineInput{State: StateClosed, Frozen: true, AccountCurrency: "USD", Currency: "COP", VelocityOK: false, PurchaseLimit: 1, Amount: 100}
	order := []struct {
		want  DeclineReason
		clear func(*DeclineInput)
	}{
		{DeclineAccountClosed, func(in *DeclineInput) { in.State = StatePendingCollateral }},
		{DeclineCollateralNotConfirmed, func(in *DeclineInput) { in.State = StateActive }},
		{DeclineCurrencyNotSupported, func(in *DeclineInput) { in.Currency = "USD" }},
		{DeclineCardFrozen, func(in *DeclineInput) { in.Frozen = false }},
		{DeclineVelocity, func(in *DeclineInput) { in.VelocityOK = true }},
		{DeclinePurchaseLimit, func(in *DeclineInput) { in.PurchaseLimit = 0 }},
	}
	for _, step := range order {
		if got := EvaluateDecline(in); got != step.want {
			t.Fatalf("want %s got %s", step.want, got)
		}
		step.clear(&in)
	}
	if got := EvaluateDecline(in); got != "" {
		t.Fatalf("all cleared: got %s", got)
	}
}

func TestEvaluateDeclinePaymentReturnedPrecedence(t *testing.T) {
	in := okInput()
	in.Restricted, in.Frozen = true, true
	if got := EvaluateDecline(in); got != DeclinePaymentReturned {
		t.Fatalf("restricted+frozen: got %s want payment_returned", got)
	}
	in.State = StateClosed
	if got := EvaluateDecline(in); got != DeclineAccountClosed {
		t.Fatalf("closed wins: got %s", got)
	}
	in = okInput()
	in.Restricted, in.Currency = true, "COP"
	if got := EvaluateDecline(in); got != DeclineCurrencyNotSupported {
		t.Fatalf("currency before restriction: got %s", got)
	}
}
