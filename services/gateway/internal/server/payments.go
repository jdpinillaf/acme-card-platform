package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/httpx"
)

type paymentJSON struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	ExternalRef string `json:"external_ref,omitempty"`
	ReturnRef   string `json:"return_ref,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	SettledAt   string `json:"settled_at,omitempty"`
	ReturnedAt  string `json:"returned_at,omitempty"`
}

var paymentStatuses = map[accountsv1.PaymentStatus]string{
	accountsv1.PaymentStatus_PAYMENT_STATUS_PENDING:   "pending",
	accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLING:  "settling",
	accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED:   "settled",
	accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED:    "failed",
	accountsv1.PaymentStatus_PAYMENT_STATUS_EXPIRED:   "expired",
	accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNING: "returning",
	accountsv1.PaymentStatus_PAYMENT_STATUS_RETURNED:  "returned",
}

func toPaymentJSON(p *accountsv1.Payment) paymentJSON {
	out := paymentJSON{ID: p.Id, AccountID: p.AccountId, Status: paymentStatuses[p.Status], ExternalRef: p.ExternalRef, ReturnRef: p.ReturnRef,
		CreatedAt: ts(p.CreatedAt), SettledAt: ts(p.SettledAt), ReturnedAt: ts(p.ReturnedAt)}
	if p.Amount != nil {
		out.AmountMinor, out.Currency = p.Amount.AmountMinor, p.Amount.Currency
	}
	return out
}

func paymentBody(res *accountsv1.PaymentResponse) map[string]any {
	return map[string]any{"payment": toPaymentJSON(res.Payment), "account": toAccountJSON(res.Account), "replayed": res.Replayed}
}

func (s *server) createPayment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AmountMinor int64 `json:"amount_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.CreatePayment(r.Context(), &accountsv1.CreatePaymentRequest{
		AccountId: chi.URLParam(r, "accountID"), AmountMinor: body.AmountMinor, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, paymentBody(res))
}

func (s *server) listPayments(w http.ResponseWriter, r *http.Request) {
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	res, err := s.Accounts.ListPayments(r.Context(), &accountsv1.ListPaymentsRequest{
		AccountId: chi.URLParam(r, "accountID"), PageSize: int32(min(max(size, 0), 100)), PageToken: r.URL.Query().Get("page_token")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]paymentJSON, 0, len(res.Payments))
	for _, p := range res.Payments {
		out = append(out, toPaymentJSON(p))
	}
	httpx.WriteJSON(w, 200, map[string]any{"payments": out, "next_page_token": res.NextPageToken})
}

var bankMeta = &commonv1.RequestMeta{Actor: "bank"}

// bankPaymentWebhook applies the bank's answer to a payment intent (settled | failed).
func (s *server) bankPaymentWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.BankSecret); !ok {
		return
	}
	var body struct {
		PaymentID   string `json:"payment_id"`
		ExternalRef string `json:"external_ref"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Status      string `json:"status"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	var st accountsv1.PaymentStatus
	switch body.Status {
	case "settled":
		st = accountsv1.PaymentStatus_PAYMENT_STATUS_SETTLED
	case "failed":
		st = accountsv1.PaymentStatus_PAYMENT_STATUS_FAILED
	default:
		httpx.WriteError(w, 400, "invalid_status", "status must be settled or failed")
		return
	}
	res, err := s.Accounts.SettlePayment(r.Context(), &accountsv1.SettlePaymentRequest{PaymentId: body.PaymentID, ExternalRef: body.ExternalRef,
		AmountMinor: body.AmountMinor, Currency: body.Currency, Status: st, Meta: bankMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	writeReplayable(w, 200, res.Replayed, paymentBody(res))
}

// bankPaymentReturn applies a bank return of a settled payment.
func (s *server) bankPaymentReturn(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.BankSecret); !ok {
		return
	}
	var body struct {
		ReturnRef   string `json:"return_ref"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.ReturnPayment(r.Context(), &accountsv1.ReturnPaymentRequest{PaymentId: chi.URLParam(r, "paymentID"),
		ReturnRef: body.ReturnRef, AmountMinor: body.AmountMinor, Meta: bankMeta})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	writeReplayable(w, 200, res.Replayed, paymentBody(res))
}
