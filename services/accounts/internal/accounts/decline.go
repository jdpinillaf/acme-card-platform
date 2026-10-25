package accounts

// DeclineReason is the machine-readable reason a card network authorization is refused.
type DeclineReason string

const (
	DeclineAccountClosed          DeclineReason = "account_closed"
	DeclineCollateralNotConfirmed DeclineReason = "collateral_not_confirmed"
	DeclineCurrencyNotSupported   DeclineReason = "currency_not_supported"
	DeclinePaymentReturned        DeclineReason = "payment_returned"
	DeclineCardFrozen             DeclineReason = "card_frozen"
	DeclineVelocity               DeclineReason = "velocity_limit_exceeded"
	DeclinePurchaseLimit          DeclineReason = "purchase_limit_exceeded"
	DeclineInsufficientCredit     DeclineReason = "insufficient_credit"
)

// DeclineInput is everything policy needs to decide, before the ledger's money check.
type DeclineInput struct {
	State           State
	Frozen          bool
	Restricted      bool // system restriction after a returned payment (ADR-018)
	AccountCurrency string
	Currency        string
	VelocityOK      bool
	PurchaseLimit   int64 // 0 = no customer cap
	Amount          int64
}

// EvaluateDecline applies the policy checks in the agreed order; first match wins. It returns ""
// when policy passes — insufficient_credit is decided afterwards by the ledger under its lock.
func EvaluateDecline(in DeclineInput) DeclineReason {
	switch {
	case in.State == StateClosed:
		return DeclineAccountClosed
	case in.State != StateActive:
		return DeclineCollateralNotConfirmed
	case in.Currency != in.AccountCurrency:
		return DeclineCurrencyNotSupported
	case in.Restricted:
		return DeclinePaymentReturned
	case in.Frozen:
		return DeclineCardFrozen
	case !in.VelocityOK:
		return DeclineVelocity
	case in.PurchaseLimit > 0 && in.Amount > in.PurchaseLimit:
		return DeclinePurchaseLimit
	}
	return ""
}
