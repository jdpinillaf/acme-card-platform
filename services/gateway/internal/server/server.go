// Package server is the public REST gateway: JWT auth, account authorization, rate limiting,
// idempotency, audit, strict JSON, and the HMAC-signed bank webhook.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	commonv1 "github.com/acme/acme-card/gen/go/acme/common/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/httpx"
	"github.com/acme/acme-card/pkg/idempotency"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/webhook"
)

const maxBody = 64 << 10

// Limiter is the rate limiter contract (see internal/ratelimit).
type Limiter interface {
	Allow(ctx context.Context, bucket string, now time.Time) (bool, time.Duration, error)
}

// Deps wires the gateway.
type Deps struct {
	Accounts         accountsv1.AccountsServiceClient
	JWTSecret        []byte
	BankSecret       []byte
	NetworkSecret    []byte
	WebhookTolerance time.Duration
	Rejections       RejectionRecorder
	RequestTimeout   time.Duration

	Idempotency *idempotency.Store
	Limiter     Limiter
	AuditSink   outbox.Execer // gateway pool; audit events go to its outbox
	Ready       []func(context.Context) error

	// TrustedProxies are the load balancer networks whose X-Forwarded-For hops are believed.
	// Requests from any other address use RemoteAddr; client-supplied IP headers are ignored.
	TrustedProxies []netip.Prefix

	// DisableStateful skips idempotency, rate limit and audit (unit tests only).
	DisableStateful bool
}

type server struct{ Deps }

// New builds the HTTP handler.
func New(d Deps) http.Handler {
	s := &server{d}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.recoverer, s.timeout)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, 200, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)

	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(d.JWTSecret))
		if !d.DisableStateful {
			r.Use(s.rateLimit, s.audit, idempotency.Middleware(d.Idempotency, auth.Subject))
		}
		r.Post("/v1/accounts", s.createAccount)
		r.Route("/v1/accounts/{accountID}", func(r chi.Router) {
			r.Use(auth.RequireAccount(func(r *http.Request) string { return chi.URLParam(r, "accountID") }))
			r.Get("/", s.getAccount)
			r.Post("/collateral/deposits", s.createDeposit)
			r.Post("/freeze", s.setFrozen(true))
			r.Post("/unfreeze", s.setFrozen(false))
			r.Put("/limits", s.setLimits)
			r.Get("/transactions", s.listTransactions)
		})
	})

	r.Group(func(r chi.Router) {
		if !d.DisableStateful {
			r.Use(s.audit)
		}
		r.Post("/v1/webhooks/bank/deposits", s.bankDepositWebhook)
		r.Post("/v1/network/authorizations", s.networkAuthorize)
		r.Post("/v1/network/authorizations/{authID}/capture", s.networkCapture)
		r.Post("/v1/network/authorizations/{authID}/reversal", s.networkReverse)
		r.Post("/v1/network/authorizations/{authID}/refunds", s.networkRefund)
	})
	return r
}

// ---------- handlers ----------

type accountJSON struct {
	ID                   string `json:"id"`
	CustomerID           string `json:"customer_id"`
	Currency             string `json:"currency"`
	State                string `json:"state"`
	Frozen               bool   `json:"frozen"`
	CreditLimitMinor     int64  `json:"credit_limit_minor"`
	AvailableCreditMinor int64  `json:"available_credit_minor"`
	CollateralMinor      int64  `json:"collateral_minor"`
	PurchaseLimitMinor   int64  `json:"purchase_limit_minor"`
	CreatedAt            string `json:"created_at,omitempty"`
	UpdatedAt            string `json:"updated_at,omitempty"`
}

type depositJSON struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	ExternalRef string `json:"external_ref,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	SettledAt   string `json:"settled_at,omitempty"`
}

var accountStates = map[accountsv1.AccountState]string{
	accountsv1.AccountState_ACCOUNT_STATE_PENDING_COLLATERAL: "pending_collateral",
	accountsv1.AccountState_ACCOUNT_STATE_ACTIVE:             "active",
	accountsv1.AccountState_ACCOUNT_STATE_CLOSED:             "closed",
}

var depositStatuses = map[accountsv1.DepositStatus]string{
	accountsv1.DepositStatus_DEPOSIT_STATUS_PENDING: "pending",
	accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED: "settled",
	accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED:  "failed",
}

func ts(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

func toAccountJSON(a *accountsv1.Account) accountJSON {
	return accountJSON{ID: a.Id, CustomerID: a.CustomerId, Currency: a.Currency, State: accountStates[a.State], Frozen: a.Frozen,
		CreditLimitMinor: a.CreditLimitMinor, AvailableCreditMinor: a.AvailableCreditMinor, CollateralMinor: a.CollateralMinor,
		PurchaseLimitMinor: a.PurchaseLimitMinor, CreatedAt: ts(a.CreatedAt), UpdatedAt: ts(a.UpdatedAt)}
}

func toDepositJSON(d *accountsv1.Deposit) depositJSON {
	out := depositJSON{ID: d.Id, AccountID: d.AccountId, Status: depositStatuses[d.Status], ExternalRef: d.ExternalRef, CreatedAt: ts(d.CreatedAt), SettledAt: ts(d.SettledAt)}
	if d.Amount != nil {
		out.AmountMinor, out.Currency = d.Amount.AmountMinor, d.Amount.Currency
	}
	return out
}

func meta(r *http.Request, actor string) *commonv1.RequestMeta {
	return &commonv1.RequestMeta{Actor: actor, IdempotencyKeyHash: idempotency.KeyHashFromContext(r.Context())}
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Currency string `json:"currency"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	sub := auth.Subject(r)
	res, err := s.Accounts.CreateAccount(r.Context(), &accountsv1.CreateAccountRequest{CustomerId: sub, Currency: body.Currency, Meta: meta(r, sub)})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, toAccountJSON(res.Account))
}

func (s *server) getAccount(w http.ResponseWriter, r *http.Request) {
	res, err := s.Accounts.GetAccount(r.Context(), &accountsv1.GetAccountRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, toAccountJSON(res.Account))
}

func (s *server) createDeposit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AmountMinor int64 `json:"amount_minor"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Accounts.CreateDeposit(r.Context(), &accountsv1.CreateDepositRequest{
		AccountId: chi.URLParam(r, "accountID"), AmountMinor: body.AmountMinor, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 201, toDepositJSON(res.Deposit))
}

func (s *server) bankDepositWebhook(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifySigned(w, r, s.BankSecret); !ok {
		return
	}
	var body struct {
		DepositID   string `json:"deposit_id"`
		ExternalRef string `json:"external_ref"`
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Status      string `json:"status"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	var st accountsv1.DepositStatus
	switch body.Status {
	case "settled":
		st = accountsv1.DepositStatus_DEPOSIT_STATUS_SETTLED
	case "failed":
		st = accountsv1.DepositStatus_DEPOSIT_STATUS_FAILED
	default:
		httpx.WriteError(w, 400, "invalid_status", "status must be settled or failed")
		return
	}
	res, err := s.Accounts.SettleDeposit(r.Context(), &accountsv1.SettleDepositRequest{
		DepositId: body.DepositID, ExternalRef: body.ExternalRef, AmountMinor: body.AmountMinor, Currency: body.Currency, Status: st,
		Meta: &commonv1.RequestMeta{Actor: "bank"},
	})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	if res.Replayed {
		w.Header().Set(idempotency.HeaderReplayed, "true")
	}
	httpx.WriteJSON(w, 200, map[string]any{"deposit": toDepositJSON(res.Deposit), "account": toAccountJSON(res.Account), "replayed": res.Replayed})
}

// verifySigned checks the HMAC signature with secret and, on success, leaves the raw body readable
// again on r.Body. Rejections are recorded (deduped per source/window) and not audited per request.
func (s *server) verifySigned(w http.ResponseWriter, r *http.Request, secret []byte) ([]byte, bool) {
	return s.verify(w, r, secret, false)
}

// verifySignedRequest is verifySigned with method and path bound into the signature.
func (s *server) verifySignedRequest(w http.ResponseWriter, r *http.Request, secret []byte) ([]byte, bool) {
	return s.verify(w, r, secret, true)
}

func (s *server) verify(w http.ResponseWriter, r *http.Request, secret []byte, bindRequest bool) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		httpx.WriteError(w, 413, "body_too_large", "request body too large")
		return nil, false
	}
	ts, sig := r.Header.Get(webhook.HeaderTimestamp), r.Header.Get(webhook.HeaderSignature)
	var err2 error
	if bindRequest {
		err2 = webhook.VerifyRequest(secret, r.Method, r.URL.Path, raw, ts, sig, time.Now(), s.WebhookTolerance)
	} else {
		err2 = webhook.Verify(secret, raw, ts, sig, time.Now(), s.WebhookTolerance)
	}
	if err := err2; err != nil {
		skipAudit(r.Context())
		if recErr := s.Rejections.Record(r.Context(), s.clientIP(r), err.Error(), time.Now()); recErr != nil {
			slog.ErrorContext(r.Context(), "record webhook rejection", "err", recErr)
		}
		httpx.WriteError(w, 401, err.Error(), "webhook signature rejected")
		return nil, false
	}
	r.Body = io.NopCloser(bytesReader(raw))
	return raw, true
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	for _, check := range s.Ready {
		if err := check(ctx); err != nil {
			httpx.WriteJSON(w, 503, map[string]string{"status": "unavailable", "error": err.Error()})
			return
		}
	}
	httpx.WriteJSON(w, 200, map[string]string{"status": "ready"})
}

// ---------- middleware ----------

func (s *server) timeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.RequestTimeout <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				slog.ErrorContext(r.Context(), "panic", "panic", p)
				httpx.WriteError(w, 500, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retry, err := s.Limiter.Allow(r.Context(), "sub:"+auth.Subject(r), time.Now())
		if err != nil {
			slog.ErrorContext(r.Context(), "rate limit check failed", "err", err)
			httpx.WriteError(w, 503, "unavailable", "rate limiter unavailable")
			return
		}
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			httpx.WriteError(w, 429, "velocity_limit_exceeded", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type outcomeKey struct{}

type skipAuditKey struct{}

func skipAudit(ctx context.Context) {
	if p, ok := ctx.Value(skipAuditKey{}).(*bool); ok {
		*p = true
	}
}

// setOutcome records the outcome for both the idempotency store and the audit event.
func setOutcome(ctx context.Context, o idempotency.Outcome) {
	idempotency.SetOutcome(ctx, o)
	if p, ok := ctx.Value(outcomeKey{}).(*idempotency.Outcome); ok {
		*p = o
	}
}

// audit emits one http.request event per mutating request (including replays and rejections).
func (s *server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		var outcome idempotency.Outcome
		var skip bool
		ctx := context.WithValue(r.Context(), outcomeKey{}, &outcome)
		ctx = context.WithValue(ctx, skipAuditKey{}, &skip)
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r.WithContext(ctx))
		if skip {
			return
		}
		if outcome == "" {
			outcome = idempotency.OutcomeForStatus(ww.Status())
		}
		route := r.URL.Path
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		actor := auth.Subject(r)
		if actor == "" {
			actor = "anonymous@" + s.clientIP(r)
		}
		ev := events.New("http.request", "http_request", middleware.GetReqID(r.Context()))
		ev.Actor = actor
		ev.HTTPStatus = ww.Status()
		if key := r.Header.Get(idempotency.HeaderKey); key != "" && actor != "" {
			ev.IdempotencyKeyHash = idempotency.KeyHash(auth.Subject(r), key)
		}
		ev.After, _ = json.Marshal(map[string]any{
			"method": r.Method, "route": route, "path": r.URL.Path, "status": ww.Status(), "outcome": outcome,
			"replayed": ww.Header().Get(idempotency.HeaderReplayed) == "true", "duration_ms": time.Since(start).Milliseconds(),
		})
		// Detached context: the request may have timed out, the audit write must still happen.
		actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		if err := outbox.Enqueue(actx, s.AuditSink, ev); err != nil {
			slog.ErrorContext(actx, "audit enqueue failed", "err", err)
		}
	})
}

// ---------- errors ----------

func writeGRPCError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		setOutcome(r.Context(), idempotency.OutcomeTimeout)
		httpx.WriteError(w, 504, "timeout", "upstream timed out")
		return
	}
	st, _ := status.FromError(err)
	reason := grpcx.Reason(err)
	httpStatus, code := 500, "internal"
	switch st.Code() {
	case codes.InvalidArgument, codes.OutOfRange:
		httpStatus, code = 400, "invalid_request"
	case codes.NotFound:
		httpStatus, code = 404, "not_found"
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		httpStatus, code = 409, "conflict"
	case codes.PermissionDenied:
		httpStatus, code = 403, "forbidden"
	case codes.Unauthenticated:
		httpStatus, code = 401, "unauthorized"
	case codes.ResourceExhausted:
		httpStatus, code = 429, "rate_limited"
	case codes.DeadlineExceeded:
		httpStatus, code = 504, "timeout"
		setOutcome(r.Context(), idempotency.OutcomeTimeout)
	case codes.Unavailable:
		httpStatus, code = 503, "unavailable"
	}
	if reason != "" {
		code = reason
	}
	msg := st.Message()
	if httpStatus == 500 {
		// The upstream transaction was rolled back; never leak internal details.
		setOutcome(r.Context(), idempotency.OutcomeRolledBack)
		slog.ErrorContext(r.Context(), "upstream error", "err", err)
		msg = "internal error"
	}
	httpx.WriteError(w, httpStatus, code, msg)
}

// clientIP returns the caller address. X-Forwarded-For is only trusted when the direct peer is a
// configured proxy, and then read right to left, skipping trusted hops: the left part of the
// header is client-controlled. True-Client-IP / X-Real-IP from clients are never trusted.
func (s *server) clientIP(r *http.Request) string {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	addr := peer.Addr().Unmap()
	if !s.trusted(addr) {
		return addr.String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !s.trusted(hop.Unmap()) {
			return hop.Unmap().String()
		}
	}
	return addr.String()
}

func (s *server) trusted(a netip.Addr) bool {
	for _, p := range s.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
