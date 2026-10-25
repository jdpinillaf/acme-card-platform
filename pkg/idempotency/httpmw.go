package idempotency

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/acme/acme-card/pkg/httpx"
)

const (
	// HeaderKey is the client-supplied idempotency key header.
	HeaderKey = "Idempotency-Key"
	// HeaderReplayed marks responses served from the store.
	HeaderReplayed = "Idempotency-Replayed"

	maxKeyLen  = 255
	maxBodyLen = 1 << 20
)

// SubjectFunc extracts the scope for a key (e.g. JWT subject or webhook source).
type SubjectFunc func(*http.Request) string

type ctxKey int

const (
	outcomeCtxKey ctxKey = iota
	keyHashCtxKey
)

// SetOutcome lets a handler override the stored outcome (e.g. OutcomeRolledBack).
func SetOutcome(ctx context.Context, o Outcome) {
	if p, ok := ctx.Value(outcomeCtxKey).(*Outcome); ok {
		*p = o
	}
}

// KeyHashFromContext returns the key hash of the current request, if any (used by audit).
func KeyHashFromContext(ctx context.Context) string {
	s, _ := ctx.Value(keyHashCtxKey).(string)
	return s
}

// Middleware enforces Idempotency-Key on mutating requests and replays completed ones.
func Middleware(store *Store, subject SubjectFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(HeaderKey)
			if key == "" {
				httpx.WriteError(w, http.StatusBadRequest, "idempotency_key_required", HeaderKey+" header is required")
				return
			}
			if len(key) > maxKeyLen {
				httpx.WriteError(w, http.StatusBadRequest, "idempotency_key_invalid", HeaderKey+" must be at most 255 characters")
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyLen+1))
			if err != nil || len(body) > maxBodyLen {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			keyHash := KeyHash(subject(r), key)
			claim, replay, err := store.Acquire(r.Context(), keyHash, RequestHash(r.Method, r.URL.Path, body))
			switch {
			case errors.Is(err, ErrKeyReuseMismatch):
				httpx.WriteError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "Idempotency-Key was already used with a different request")
				return
			case errors.Is(err, ErrInProgress):
				w.Header().Set("Retry-After", "1")
				httpx.WriteError(w, http.StatusConflict, "request_in_progress", "a request with this Idempotency-Key is still being processed; retry later")
				return
			case err != nil:
				slog.ErrorContext(r.Context(), "idempotency acquire failed", "err", err)
				httpx.WriteError(w, http.StatusServiceUnavailable, "idempotency_unavailable", "could not acquire idempotency key")
				return
			case replay != nil:
				w.Header().Set(HeaderReplayed, "true")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(replay.Status)
				_, _ = w.Write(replay.Body)
				return
			}

			outcome := Outcome("")
			ctx := context.WithValue(r.Context(), outcomeCtxKey, &outcome)
			ctx = context.WithValue(ctx, keyHashCtxKey, keyHash)
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "handler panic", "panic", p)
					_ = claim.Complete(ctx, Result{Status: 500, Body: []byte(`{"error":{"code":"internal","message":"internal error"}}`), Outcome: OutcomeServerError})
					if !rec.wroteHeader {
						httpx.WriteError(w, http.StatusInternalServerError, "internal", "internal error")
					}
				}
			}()
			next.ServeHTTP(rec, r.WithContext(ctx))

			res := Result{Status: rec.status, Body: rec.buf.Bytes(), Outcome: outcome}
			if err := claim.Complete(ctx, res); err != nil {
				slog.ErrorContext(ctx, "idempotency complete failed", "err", err)
			}
		})
	}
}

// recorder tees the response so it can be stored for replay.
type recorder struct {
	http.ResponseWriter
	status      int
	buf         bytes.Buffer
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}
