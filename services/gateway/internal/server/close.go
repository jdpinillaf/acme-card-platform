package server

import (
	"net/http"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/httpx"
	"github.com/acme/acme-card/pkg/idempotency"
)

type destinationJSON struct {
	BankCode      string `json:"bank_code"`
	AccountNumber string `json:"account_number"`
	HolderName    string `json:"holder_name"`
}

func (d *destinationJSON) proto() *accountsv1.Destination {
	if d == nil {
		return nil
	}
	return &accountsv1.Destination{BankCode: d.BankCode, AccountNumber: d.AccountNumber, HolderName: d.HolderName}
}

type payoutJSON struct {
	ID               string `json:"id"`
	AccountID        string `json:"account_id"`
	AmountMinor      int64  `json:"amount_minor"`
	Currency         string `json:"currency"`
	Status           string `json:"status"`
	Attempt          int32  `json:"attempt"`
	AttemptID        string `json:"attempt_id"`
	DestinationLast4 string `json:"destination_last4"`
	CreatedAt        string `json:"created_at,omitempty"`
	SettledAt        string `json:"settled_at,omitempty"`
}

var payoutStatuses = map[accountsv1.PayoutStatus]string{
	accountsv1.PayoutStatus_PAYOUT_STATUS_PENDING:  "pending",
	accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLING: "settling",
	accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED:  "settled",
	accountsv1.PayoutStatus_PAYOUT_STATUS_FAILED:   "failed",
}

func toPayoutJSON(p *accountsv1.Payout) payoutJSON {
	out := payoutJSON{ID: p.Id, AccountID: p.AccountId, Status: payoutStatuses[p.Status], Attempt: p.Attempt, AttemptID: p.AttemptId,
		DestinationLast4: p.DestinationLast4, CreatedAt: ts(p.CreatedAt), SettledAt: ts(p.SettledAt)}
	if p.Amount != nil {
		out.AmountMinor, out.Currency = p.Amount.AmountMinor, p.Amount.Currency
	}
	return out
}

func payoutBody(res *accountsv1.PayoutResponse) map[string]any {
	return map[string]any{"payout": toPayoutJSON(res.Payout), "account": toAccountJSON(res.Account), "replayed": res.Replayed}
}

// closeAccount: customer close (destination only) or support close (destination + reason).
func (s *server) closeAccount(support bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Destination *destinationJSON `json:"destination"`
			Reason      string           `json:"reason"`
		}
		if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
			httpx.WriteError(w, 400, "invalid_body", err.Error())
			return
		}
		if support && (utf8.RuneCountInString(body.Reason) < 1 || utf8.RuneCountInString(body.Reason) > 200) {
			httpx.WriteError(w, 400, "invalid_reason", "support closes need a reason (1-200 characters)")
			return
		}
		if !support && body.Reason != "" {
			httpx.WriteError(w, 400, "invalid_body", "reason is only accepted on the support route")
			return
		}
		res, err := s.Accounts.CloseAccount(r.Context(), &accountsv1.CloseAccountRequest{AccountId: chi.URLParam(r, "accountID"),
			Destination: body.Destination.proto(), Reason: body.Reason, Meta: meta(r, auth.Subject(r))})
		if err != nil {
			writeGRPCError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, toAccountJSON(res.Account))
	}
}

func (s *server) cancelAccount(w http.ResponseWriter, r *http.Request) {
	res, err := s.Accounts.CancelAccount(r.Context(), &accountsv1.CancelAccountRequest{AccountId: chi.URLParam(r, "accountID"), Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, toAccountJSON(res.Account))
}

func (s *server) listPayouts(w http.ResponseWriter, r *http.Request) {
	res, err := s.Accounts.ListPayouts(r.Context(), &accountsv1.ListPayoutsRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]payoutJSON, 0, len(res.Payouts))
	for _, p := range res.Payouts {
		out = append(out, toPayoutJSON(p))
	}
	httpx.WriteJSON(w, 200, map[string]any{"payouts": out})
}

func (s *server) retryPayout(w http.ResponseWriter, r *http.Request) {
	res, err := s.Accounts.RetryPayout(r.Context(), &accountsv1.RetryPayoutRequest{AccountId: chi.URLParam(r, "accountID"),
		PayoutId: chi.URLParam(r, "payoutID"), Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, payoutBody(res))
}

// bankPayoutInstruction gives the (simulated) bank the decrypted destination of an open payout
// attempt. Bank signature bound to method and path; the only response with a full account number.
func (s *server) bankPayoutInstruction(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.BankSecret); !ok {
		return
	}
	res, err := s.Accounts.GetPayoutInstruction(r.Context(), &accountsv1.GetPayoutInstructionRequest{AttemptId: chi.URLParam(r, "attemptID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, 200, map[string]any{"attempt_id": res.AttemptId, "amount_minor": res.AmountMinor, "currency": res.Currency,
		"bank_code": res.BankCode, "account_number": res.AccountNumber, "holder_name": res.HolderName})
}

func (s *server) bankPayoutWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySignedRequest(w, r, s.BankSecret); !ok {
		return
	}
	var body struct {
		AttemptID        string `json:"attempt_id"`
		AmountMinor      int64  `json:"amount_minor"`
		Currency         string `json:"currency"`
		DestinationLast4 string `json:"destination_last4"`
		Status           string `json:"status"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	var st accountsv1.PayoutStatus
	switch body.Status {
	case "settled":
		st = accountsv1.PayoutStatus_PAYOUT_STATUS_SETTLED
	case "failed":
		st = accountsv1.PayoutStatus_PAYOUT_STATUS_FAILED
	default:
		httpx.WriteError(w, 400, "invalid_status", "status must be settled or failed")
		return
	}
	res, err := s.Accounts.SettlePayout(r.Context(), &accountsv1.SettlePayoutRequest{AttemptId: body.AttemptID, AmountMinor: body.AmountMinor,
		Currency: body.Currency, DestinationLast4: body.DestinationLast4, Status: st, Meta: &commonv1.RequestMeta{Actor: "bank"}})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	if res.Replayed {
		w.Header().Set(idempotency.HeaderReplayed, "true")
	}
	httpx.WriteJSON(w, 200, payoutBody(res))
}
