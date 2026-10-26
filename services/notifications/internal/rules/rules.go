// Package rules decides which domain events become customer notifications, renders their text,
// and holds the delivery policy: backoff, mandatory types, the partner allow-list (ADR-022).
package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/acme/acme-card/pkg/events"
)

// Notification types, in the order customers see them.
var AllTypes = []string{"card_activated", "purchase_approved", "purchase_declined", "card_freeze", "payment_received",
	"late_fee", "card_blocked", "payout_update", "suspected_fraud"}

// MandatoryTypes cannot be fully disabled: at least one channel stays on.
var MandatoryTypes = map[string]bool{"late_fee": true, "card_blocked": true, "suspected_fraud": true}

// Channels a customer notification goes to.
var Channels = []string{"push", "sms", "email"}

var (
	ErrUnknownType = errors.New("unknown notification type")
	ErrMandatory   = errors.New("mandatory_notification")
)

// Notice is what an event means for its customer. Text renders it; cur is the account currency,
// used when the payload carries none. CustomerID is empty when the payload does not name it.
type Notice struct {
	Type, AccountID, CustomerID string
	Currency                    string // set when the payload is an account snapshot
	Mandatory                   bool
	Text                        func(cur string) string
}

type payload map[string]any

func decode(raw []byte) payload {
	var p payload
	_ = json.Unmarshal(raw, &p)
	return p
}

func (p payload) str(k string) string {
	if v, ok := p[k].(string); ok {
		return v
	}
	return ""
}

func (p payload) num(k string) int64 {
	if v, ok := p[k].(float64); ok {
		return int64(v)
	}
	return 0
}

func (p payload) obj(k string) payload {
	if v, ok := p[k].(map[string]any); ok {
		return v
	}
	return nil
}

// FormatMinor renders minor units with two decimals and the currency code.
func FormatMinor(amount int64, cur string) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, amount/100, amount%100, cur)
}

func currency(p payload, cur string) string {
	if c := p.str("currency"); c != "" {
		return c
	}
	return cur
}

var declineText = map[string]string{
	"insufficient_credit": "insufficient credit", "over_limit": "over the credit limit", "purchase_limit_exceeded": "over your purchase limit",
	"velocity_limit_exceeded": "too many purchases in a short time", "card_frozen": "the card is frozen", "past_due": "the account is past due",
	"payment_returned": "a payment was returned", "account_closed": "the account is closed", "account_closing": "the account is closing",
	"charged_off": "the account was charged off", "collateral_not_confirmed": "the collateral is not confirmed yet",
	"currency_not_supported": "the currency is not supported",
}

func human(reason string) string {
	if t, ok := declineText[reason]; ok {
		return t
	}
	return strings.ReplaceAll(reason, "_", " ")
}

func notice(typ, account, customer string, text func(string) string) Notice {
	return Notice{Type: typ, AccountID: account, CustomerID: customer, Mandatory: MandatoryTypes[typ], Text: text}
}

func (n Notice) with(cur string) Notice {
	if n.CustomerID != "" {
		n.Currency = cur
	}
	return n
}

// Classify maps an event to its customer notification. Suspected fraud is not decided here: it
// needs the decline history (see CountsTowardFraud and FraudNotice).
func Classify(ev events.Event) (Notice, bool) {
	after, before := decode(ev.After), decode(ev.Before)
	switch ev.Type {
	case "card.activated":
		return notice("card_activated", ev.AggregateID, after.str("customer_id"), func(cur string) string {
			return fmt.Sprintf("Your card is active. Credit limit: %s.", FormatMinor(after.num("credit_limit_minor"), currency(after, cur)))
		}).with(after.str("currency")), true
	case "authorization.approved":
		return notice("purchase_approved", after.str("account_id"), "", func(cur string) string {
			return fmt.Sprintf("Purchase approved: %s at %s.", FormatMinor(after.num("amount_minor"), currency(after, cur)), after.str("merchant_name"))
		}), true
	case "authorization.declined":
		return notice("purchase_declined", after.str("account_id"), "", func(cur string) string {
			return fmt.Sprintf("Purchase declined: %s at %s (%s).", FormatMinor(after.num("amount_minor"), currency(after, cur)), after.str("merchant_name"),
				human(after.str("decline_reason")))
		}), true
	case "card.frozen", "card.unfrozen":
		state := "frozen"
		if ev.Type == "card.unfrozen" {
			state = "unfrozen"
		}
		return notice("card_freeze", ev.AggregateID, after.str("customer_id"), func(string) string { return "Your card is " + state + "." }).with(after.str("currency")), true
	case "payment.settled":
		return notice("payment_received", after.str("account_id"), "", func(cur string) string {
			return fmt.Sprintf("Payment received: %s. Your available credit is restored.", FormatMinor(after.num("amount_minor"), currency(after, cur)))
		}), true
	case "fee.charged": // billing's charge payload: Go field names, no currency
		if !strings.HasPrefix(after.str("Reference"), "late_fee:") {
			return Notice{}, false
		}
		return notice("late_fee", after.str("AccountID"), "", func(cur string) string {
			return fmt.Sprintf("Your payment is past due. A late fee of %s was charged.", FormatMinor(after.num("Amount"), cur))
		}), true
	case "account.delinquency_changed":
		if after.str("delinquency") != "past_due" || before.str("delinquency") == "past_due" {
			return Notice{}, false
		}
		return notice("card_blocked", ev.AggregateID, after.str("customer_id"), func(string) string {
			return "Your card is blocked: your payment is 15 days past due. Pay the minimum to unblock it."
		}).with(after.str("currency")), true
	case "account.charged_off":
		acc := after.obj("account")
		return notice("card_blocked", ev.AggregateID, acc.str("customer_id"), func(cur string) string {
			return fmt.Sprintf("Your card is permanently blocked: 60 days past due. %s of collateral was applied to the debt.",
				FormatMinor(after.num("collateral_applied_minor"), currency(acc, cur)))
		}).with(acc.str("currency")), true
	case "card.restricted":
		return notice("card_blocked", ev.AggregateID, after.str("customer_id"), func(string) string {
			return "Your card is blocked: " + human(after.str("restricted_reason")) + "."
		}).with(after.str("currency")), true
	case "payout.requested", "payout.settled", "payout.failed":
		verb := map[string]string{"payout.requested": "is on its way", "payout.settled": "was completed", "payout.failed": "failed; support will retry it"}[ev.Type]
		return notice("payout_update", after.str("account_id"), "", func(cur string) string {
			return fmt.Sprintf("Your payout of %s to ****%s %s.", FormatMinor(after.num("amount_minor"), currency(after, cur)), after.str("destination_last4"), verb)
		}), true
	}
	return Notice{}, false
}

var fraudReasons = map[string]bool{"insufficient_credit": true, "over_limit": true, "purchase_limit_exceeded": true, "velocity_limit_exceeded": true}

// CountsTowardFraud reports whether a decline suggests someone else is using the card (user
// decision: a frozen, closed or past-due card is not suspicious).
func CountsTowardFraud(ev events.Event) bool {
	return ev.Type == "authorization.declined" && fraudReasons[decode(ev.After).str("decline_reason")]
}

// FraudNotice is the suspected-fraud alert for account (customer looked up by the caller).
func FraudNotice(accountID string) Notice {
	return notice("suspected_fraud", accountID, "", func(string) string {
		return "Several purchases on your card were declined in a few minutes. If this wasn't you, freeze your card now."
	})
}

// Fraud window: the 3rd counting decline within FraudWindow raises an alert, at most once per FraudCooldown.
const (
	FraudDeclines = 3
	FraudWindow   = 10 * time.Minute
	FraudCooldown = time.Hour
)

var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// MaxAttempts is how many sends a delivery gets before it is failed.
const MaxAttempts = 6

// Backoff is the wait after the n-th failed attempt; false means give up (the delivery fails).
func Backoff(failures int) (time.Duration, bool) {
	if failures < 1 || failures > len(backoff) {
		return 0, false
	}
	return backoff[failures-1], true
}

// ValidatePreference refuses unknown types and a mandatory type with every channel off.
func ValidatePreference(typ string, push, sms, email bool) error {
	known := false
	for _, t := range AllTypes {
		known = known || t == typ
	}
	if !known {
		return ErrUnknownType
	}
	if MandatoryTypes[typ] && !push && !sms && !email {
		return ErrMandatory
	}
	return nil
}

var partnerPrefixes = []string{"account.", "card.", "authorization.", "payment."}

var partnerExact = map[string]bool{"payout.requested": true, "payout.settled": true, "payout.failed": true, "statement.generated": true,
	"fee.charged": true, "interest.charged": true, "charge.waived": true, "collateral.confirmed": true, "collateral.deposit_failed": true}

// PartnerEventAllowed is the public event allow-list partners may subscribe to.
func PartnerEventAllowed(typ string) bool {
	if partnerExact[typ] {
		return true
	}
	for _, p := range partnerPrefixes {
		if strings.HasPrefix(typ, p) && len(typ) > len(p) {
			return true
		}
	}
	return false
}
