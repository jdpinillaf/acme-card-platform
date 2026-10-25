package accounts

// DeclineReason is the machine-readable reason a card network authorization is refused.
type DeclineReason string

const (
	DeclineAccountClosed          DeclineReason = "account_closed"
	DeclineAccountClosing         DeclineReason = "account_closing"
	DeclineChargedOff             DeclineReason = "charged_off"
	DeclineCollateralNotConfirmed DeclineReason = "collateral_not_confirmed"
	DeclineCurrencyNotSupported   DeclineReason = "currency_not_supported"
	DeclinePaymentReturned        DeclineReason = "payment_returned"
	DeclineOverLimit              DeclineReason = "over_limit"
	DeclinePastDue                DeclineReason = "past_due"
	DeclineCardFrozen             DeclineReason = "card_frozen"
	DeclineVelocity               DeclineReason = "velocity_limit_exceeded"
	DeclinePurchaseLimit          DeclineReason = "purchase_limit_exceeded"
	DeclineInsufficientCredit     DeclineReason = "insufficient_credit"
)

// DeclineInput is everything policy needs to decide, before the ledger's money check.
type DeclineInput struct {
	State            State
	Frozen           bool
	RestrictedReason string // system restriction: payment_returned (ADR-018) or over_limit (ADR-019)
	Delinquency      string // none | past_due | charged_off (ADR-019)
	AccountCurrency  string
	Currency         string
	VelocityOK       bool
	PurchaseLimit    int64 // 0 = no customer cap
	Amount           int64
}

// EvaluateDecline applies the policy checks in the agreed order; first match wins. It returns ""
// when policy passes — insufficient_credit is decided afterwards by the ledger under its lock.
func EvaluateDecline(in DeclineInput) DeclineReason {
	switch {
	case in.State == StateClosed || in.State == StateCancelled:
		return DeclineAccountClosed
	case in.State == StateClosing:
		return DeclineAccountClosing
	case in.Delinquency == DelinquencyChargedOff:
		return DeclineChargedOff
	case in.State != StateActive:
		return DeclineCollateralNotConfirmed
	case in.Currency != in.AccountCurrency:
		return DeclineCurrencyNotSupported
	case in.RestrictedReason == RestrictionOverLimit:
		return DeclineOverLimit
	case in.RestrictedReason != "":
		return DeclinePaymentReturned
	case in.Delinquency == DelinquencyPastDue:
		return DeclinePastDue
	case in.Frozen:
		return DeclineCardFrozen
	case !in.VelocityOK:
		return DeclineVelocity
	case in.PurchaseLimit > 0 && in.Amount > in.PurchaseLimit:
		return DeclinePurchaseLimit
	}
	return ""
}
