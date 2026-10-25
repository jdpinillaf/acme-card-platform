package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/httpx"
	"github.com/acme/acme-card/pkg/idempotency"
)

type merchantJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MCC  string `json:"mcc"`
}

type authorizationJSON struct {
	ID                  string       `json:"id"`
	AccountID           string       `json:"account_id"`
	NetworkRef          string       `json:"network_ref"`
	AmountMinor         int64        `json:"amount_minor"`
	Currency            string       `json:"currency"`
	Merchant            merchantJSON `json:"merchant"`
	Status              string       `json:"status"`
	DeclineReason       string       `json:"decline_reason,omitempty"`
	CapturedAmountMinor int64        `json:"captured_amount_minor"`
	RefundedAmountMinor int64        `json:"refunded_amount_minor"`
	CreatedAt           string       `json:"created_at,omitempty"`
	ExpiresAt           string       `json:"expires_at,omitempty"`
	CapturedAt          string       `json:"captured_at,omitempty"`
	ReversedAt          string       `json:"reversed_at,omitempty"`
}

var authStatuses = map[accountsv1.AuthorizationStatus]string{
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_APPROVED: "approved",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_DECLINED: "declined",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_CAPTURED: "captured",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_REVERSED: "reversed",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_EXPIRED:  "expired",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_PENDING:  "pending",
	accountsv1.AuthorizationStatus_AUTHORIZATION_STATUS_FAILED:   "failed",
}

var declineReasons = map[commonv1.DeclineReason]string{
	commonv1.DeclineReason_DECLINE_REASON_ACCOUNT_CLOSED:           "account_closed",
	commonv1.DeclineReason_DECLINE_REASON_COLLATERAL_NOT_CONFIRMED: "collateral_not_confirmed",
	commonv1.DeclineReason_DECLINE_REASON_CURRENCY_NOT_SUPPORTED:   "currency_not_supported",
	commonv1.DeclineReason_DECLINE_REASON_PAYMENT_RETURNED:         "payment_returned",
	commonv1.DeclineReason_DECLINE_REASON_PAST_DUE:                 "past_due",
	commonv1.DeclineReason_DECLINE_REASON_CHARGED_OFF:              "charged_off",
	commonv1.DeclineReason_DECLINE_REASON_OVER_LIMIT:               "over_limit",
	commonv1.DeclineReason_DECLINE_REASON_CARD_FROZEN:              "card_frozen",
	commonv1.DeclineReason_DECLINE_REASON_VELOCITY_LIMIT_EXCEEDED:  "velocity_limit_exceeded",
	commonv1.DeclineReason_DECLINE_REASON_PURCHASE_LIMIT_EXCEEDED:  "purchase_limit_exceeded",
	commonv1.DeclineReason_DECLINE_REASON_INSUFFICIENT_CREDIT:      "insufficient_credit",
}

func toAuthorizationJSON(a *accountsv1.Authorization) authorizationJSON {
	out := authorizationJSON{ID: a.Id, AccountID: a.AccountId, NetworkRef: a.NetworkRef, Status: authStatuses[a.Status],
		DeclineReason: declineReasons[a.DeclineReason], CapturedAmountMinor: a.CapturedAmountMinor, RefundedAmountMinor: a.RefundedAmountMinor,
		CreatedAt: ts(a.CreatedAt), ExpiresAt: ts(a.ExpiresAt), CapturedAt: ts(a.CapturedAt), ReversedAt: ts(a.ReversedAt)}
	if a.Amount != nil {
		out.AmountMinor, out.Currency = a.Amount.AmountMinor, a.Amount.Currency
	}
	if m := a.Merchant; m != nil {
		out.Merchant = merchantJSON{ID: m.Id, Name: m.Name, MCC: m.Mcc}
	}
	return out
}

var networkMeta = &commonv1.RequestMeta{Actor: "network"}

func writeReplayable(w http.ResponseWriter, status int, replayed bool, body any) {
	if replayed {
		w.Header().Set(idempotency.HeaderReplayed, "true")
	}
	httpx.WriteJSON(w, status, body)
}

func (s *server) networkAuthorize(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.NetworkSecret); !ok {
		return
	}
	var body struct {
		NetworkRef  string       `json:"network_ref"`
		CardID      string       `json:"card_id"`
		AmountMinor int64        `json:"amount_minor"`
		Currency    string       `json:"currency"`
		Merchant    merchantJSON `json:"merchant"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.Authorize(r.Context(), &accountsv1.AuthorizeRequest{
		NetworkRef: body.NetworkRef, CardId: body.CardID, AmountMinor: body.AmountMinor, Currency: body.Currency,
		Merchant: &accountsv1.Merchant{Id: body.Merchant.ID, Name: body.Merchant.Name, Mcc: body.Merchant.MCC}, Meta: networkMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	// Approved and declined are both handled outcomes: 201 with the stored authorization.
	writeReplayable(w, 201, res.Replayed, toAuthorizationJSON(res.Authorization))
}

func (s *server) networkCapture(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.NetworkSecret); !ok {
		return
	}
	var body struct {
		NetworkRef  string `json:"network_ref"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.Capture(r.Context(), &accountsv1.CaptureRequest{AuthorizationId: chi.URLParam(r, "authID"),
		NetworkRef: body.NetworkRef, AmountMinor: body.AmountMinor, Meta: networkMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	writeReplayable(w, 201, res.Replayed, toAuthorizationJSON(res.Authorization))
}

func (s *server) networkReverse(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.NetworkSecret); !ok {
		return
	}
	var body struct {
		NetworkRef string `json:"network_ref"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.Reverse(r.Context(), &accountsv1.ReverseRequest{AuthorizationId: chi.URLParam(r, "authID"), NetworkRef: body.NetworkRef, Meta: networkMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	writeReplayable(w, 200, res.Replayed, toAuthorizationJSON(res.Authorization))
}

type refundJSON struct {
	ID              string `json:"id"`
	AuthorizationID string `json:"authorization_id"`
	NetworkRef      string `json:"network_ref"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	CreatedAt       string `json:"created_at,omitempty"`
}

func (s *server) networkRefund(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.NetworkSecret); !ok {
		return
	}
	var body struct {
		NetworkRef  string `json:"network_ref"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.Refund(r.Context(), &accountsv1.RefundRequest{AuthorizationId: chi.URLParam(r, "authID"),
		NetworkRef: body.NetworkRef, AmountMinor: body.AmountMinor, Meta: networkMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	rf := refundJSON{ID: res.Refund.Id, AuthorizationID: res.Refund.AuthorizationId, NetworkRef: res.Refund.NetworkRef, CreatedAt: ts(res.Refund.CreatedAt)}
	if res.Refund.Amount != nil {
		rf.AmountMinor, rf.Currency = res.Refund.Amount.AmountMinor, res.Refund.Amount.Currency
	}
	writeReplayable(w, 201, res.Replayed, map[string]any{"refund": rf, "authorization": toAuthorizationJSON(res.Authorization)})
}
