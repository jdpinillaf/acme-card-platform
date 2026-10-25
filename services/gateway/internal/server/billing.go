package server

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/httpx"
)

type statementJSON struct {
	ID                     string `json:"id"`
	AccountID              string `json:"account_id"`
	Cycle                  int32  `json:"cycle"`
	Currency               string `json:"currency"`
	PeriodStart            string `json:"period_start"`
	PeriodEnd              string `json:"period_end"`
	DueDate                string `json:"due_date,omitempty"`
	PreviousBalanceMinor   int64  `json:"previous_balance_minor"`
	PurchasesMinor         int64  `json:"purchases_minor"`
	RefundsMinor           int64  `json:"refunds_minor"`
	PaymentsMinor          int64  `json:"payments_minor"`
	ReturnsMinor           int64  `json:"returns_minor"`
	InterestMinor          int64  `json:"interest_minor"`
	FeesMinor              int64  `json:"fees_minor"`
	WaiversMinor           int64  `json:"waivers_minor"`
	CollateralAppliedMinor int64  `json:"collateral_applied_minor"`
	NewBalanceMinor        int64  `json:"new_balance_minor"`
	MinimumDueMinor        int64  `json:"minimum_due_minor"`
	PrincipalMinor         int64  `json:"principal_minor"`
	InterestBucketMinor    int64  `json:"interest_bucket_minor"`
	FeeBucketMinor         int64  `json:"fee_bucket_minor"`
	SHA256                 string `json:"sha256"`
	GeneratedAt            string `json:"generated_at,omitempty"`
}

func toStatementJSON(s *billingv1.Statement) statementJSON {
	return statementJSON{ID: s.Id, AccountID: s.AccountId, Cycle: s.Cycle, Currency: s.Currency, PeriodStart: ts(s.PeriodStart), PeriodEnd: ts(s.PeriodEnd),
		DueDate: ts(s.DueDate), PreviousBalanceMinor: s.PreviousBalanceMinor, PurchasesMinor: s.PurchasesMinor, RefundsMinor: s.RefundsMinor,
		PaymentsMinor: s.PaymentsMinor, ReturnsMinor: s.ReturnsMinor, InterestMinor: s.InterestMinor, FeesMinor: s.FeesMinor, WaiversMinor: s.WaiversMinor,
		CollateralAppliedMinor: s.CollateralAppliedMinor, NewBalanceMinor: s.NewBalanceMinor, MinimumDueMinor: s.MinimumDueMinor,
		PrincipalMinor: s.PrincipalMinor, InterestBucketMinor: s.InterestBucketMinor, FeeBucketMinor: s.FeeBucketMinor, SHA256: s.Sha256,
		GeneratedAt: ts(s.GeneratedAt)}
}

type summaryJSON struct {
	AccountTime     string         `json:"account_time"`
	CycleStart      string         `json:"cycle_start,omitempty"`
	CycleEnd        string         `json:"cycle_end,omitempty"`
	LastStatement   *statementJSON `json:"last_statement,omitempty"`
	MinimumDueMinor int64          `json:"minimum_due_minor"`
	DueDate         string         `json:"due_date,omitempty"`
	PastDueMinor    int64          `json:"past_due_minor"`
	DPD             int32          `json:"dpd"`
	Grace           bool           `json:"grace"`
	Stage           string         `json:"stage"`
	Currency        string         `json:"currency"`
}

func toSummaryJSON(s *billingv1.BillingSummary) summaryJSON {
	out := summaryJSON{AccountTime: ts(s.AccountTime), CycleStart: ts(s.CycleStart), CycleEnd: ts(s.CycleEnd), MinimumDueMinor: s.MinimumDueMinor,
		DueDate: ts(s.DueDate), PastDueMinor: s.PastDueMinor, DPD: s.Dpd, Grace: s.Grace, Stage: s.Stage, Currency: s.Currency}
	if s.LastStatement != nil {
		st := toStatementJSON(s.LastStatement)
		out.LastStatement = &st
	}
	return out
}

type chargeJSON struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Cycle       int32  `json:"cycle"`
	AmountMinor int64  `json:"amount_minor"`
	WaivedMinor int64  `json:"waived_minor"`
	EffectiveAt string `json:"effective_at,omitempty"`
	Status      string `json:"status"`
}

func toChargeJSON(c *billingv1.Charge) chargeJSON {
	return chargeJSON{ID: c.Id, Kind: c.Kind, Cycle: c.Cycle, AmountMinor: c.AmountMinor, WaivedMinor: c.WaivedMinor, EffectiveAt: ts(c.EffectiveAt), Status: c.Status}
}

func (s *server) listStatements(w http.ResponseWriter, r *http.Request) {
	res, err := s.Billing.ListStatements(r.Context(), &billingv1.ListStatementsRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]statementJSON, 0, len(res.Statements))
	for _, st := range res.Statements {
		out = append(out, toStatementJSON(st))
	}
	httpx.WriteJSON(w, 200, map[string]any{"statements": out})
}

func (s *server) getStatement(w http.ResponseWriter, r *http.Request) {
	res, err := s.Billing.GetStatement(r.Context(), &billingv1.GetStatementRequest{AccountId: chi.URLParam(r, "accountID"), StatementId: chi.URLParam(r, "statementID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"statement": toStatementJSON(res.Statement), "document": json.RawMessage(res.BodyJson)})
}

func (s *server) getBilling(w http.ResponseWriter, r *http.Request) {
	res, err := s.Billing.GetSummary(r.Context(), &billingv1.GetSummaryRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, toSummaryJSON(res))
}

func (s *server) listCharges(w http.ResponseWriter, r *http.Request) {
	res, err := s.Billing.ListCharges(r.Context(), &billingv1.ListChargesRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]chargeJSON, 0, len(res.Charges))
	for _, c := range res.Charges {
		out = append(out, toChargeJSON(c))
	}
	httpx.WriteJSON(w, 200, map[string]any{"charges": out})
}

// waiveCharge is the support route: any account, role-gated (auth.RequireRole).
func (s *server) waiveCharge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AmountMinor int64  `json:"amount_minor"`
		Reason      string `json:"reason"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Billing.WaiveCharge(r.Context(), &billingv1.WaiveChargeRequest{AccountId: chi.URLParam(r, "accountID"), ChargeId: chi.URLParam(r, "chargeID"),
		AmountMinor: body.AmountMinor, Reason: body.Reason, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, map[string]any{"charge": toChargeJSON(res.Charge), "waiver": map[string]any{
		"id": res.Waiver.Id, "charge_id": res.Waiver.ChargeId, "amount_minor": res.Waiver.AmountMinor, "reason": res.Waiver.Reason, "actor": res.Waiver.Actor,
		"created_at": ts(res.Waiver.CreatedAt)}, "replayed": res.Replayed})
}

// advanceClock is the dev-only simulated clock: advance the account, then run billing up to the
// new account time so the caller sees the statements, fees and stage that fell due.
func (s *server) advanceClock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Days int32 `json:"days"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	id := chi.URLParam(r, "accountID")
	if _, err := s.Accounts.AdvanceClock(r.Context(), &accountsv1.AdvanceClockRequest{AccountId: id, Days: body.Days, Meta: meta(r, auth.Subject(r))}); err != nil {
		writeGRPCError(w, r, err)
		return
	}
	if _, err := s.Billing.CatchUp(r.Context(), &billingv1.CatchUpRequest{AccountId: id}); err != nil {
		writeGRPCError(w, r, err)
		return
	}
	acc, err := s.Accounts.GetAccount(r.Context(), &accountsv1.GetAccountRequest{AccountId: id})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	sum, err := s.Billing.GetSummary(r.Context(), &billingv1.GetSummaryRequest{AccountId: id})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"account": toAccountJSON(acc.Account), "billing": toSummaryJSON(sum)})
}
