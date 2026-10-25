package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/httpx"
)

func (s *server) setFrozen(frozen bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := s.Accounts.SetFrozen(r.Context(), &accountsv1.SetFrozenRequest{AccountId: chi.URLParam(r, "accountID"), Frozen: frozen, Meta: meta(r, auth.Subject(r))})
		if err != nil {
			writeGRPCError(w, r, err)
			return
		}
		httpx.WriteJSON(w, 200, toAccountJSON(res.Account))
	}
}

func (s *server) setLimits(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PurchaseLimitMinor int64 `json:"purchase_limit_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.SetPurchaseLimit(r.Context(), &accountsv1.SetPurchaseLimitRequest{
		AccountId: chi.URLParam(r, "accountID"), PurchaseLimitMinor: body.PurchaseLimitMinor, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, toAccountJSON(res.Account))
}

func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	res, err := s.Accounts.ListTransactions(r.Context(), &accountsv1.ListTransactionsRequest{
		AccountId: chi.URLParam(r, "accountID"), PageSize: int32(min(max(size, 0), 100)), PageToken: r.URL.Query().Get("page_token")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]authorizationJSON, 0, len(res.Authorizations))
	for _, a := range res.Authorizations {
		out = append(out, toAuthorizationJSON(a))
	}
	httpx.WriteJSON(w, 200, map[string]any{"transactions": out, "next_page_token": res.NextPageToken})
}
