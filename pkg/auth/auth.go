// Package auth implements JWT HS256 authentication with per-token account authorization.
// A token lists the accounts its subject may act on in the authorized_accounts claim.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/acme/acme-card/pkg/httpx"
)

// Claims is the token payload. exp is mandatory.
type Claims struct {
	jwt.RegisteredClaims
	AuthorizedAccounts []string `json:"authorized_accounts"`
	// Role is empty for customers; "support" may reverse charges on any account (Sprint 4).
	Role string `json:"role,omitempty"`
}

// RoleSupport is the support-agent role (charge waivers).
const RoleSupport = "support"

// RoleAuditor is the read-only auditor role (audit API, Sprint 6).
const RoleAuditor = "auditor"

// Authorized reports whether the token may act on accountID.
func (c *Claims) Authorized(accountID string) bool {
	for _, a := range c.AuthorizedAccounts {
		if a == accountID {
			return true
		}
	}
	return false
}

// Sign mints an HS256 token for sub that may act on accounts, valid for ttl.
func Sign(secret []byte, sub string, accounts []string, ttl time.Duration) (string, error) {
	return SignWithRole(secret, sub, accounts, "", ttl)
}

// SignWithRole is Sign with a role claim (e.g. RoleSupport).
func SignWithRole(secret []byte, sub string, accounts []string, role string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "acme-card",
		},
		AuthorizedAccounts: accounts,
		Role:               role,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// Parse verifies an HS256 token (and only HS256) and returns its claims.
func Parse(secret []byte, token string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &claims, nil
}

type ctxKey struct{}

// FromContext returns the verified claims stored by Middleware.
func FromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

// Middleware requires a valid Bearer token and stores its claims in the context.
func Middleware(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			claims, err := Parse(secret, strings.TrimPrefix(h, "Bearer "))
			if err != nil {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, claims)))
		})
	}
}

// RequireAccount denies the request unless the token's authorized_accounts contains the account
// identified by extract. Must run after Middleware.
func RequireAccount(extract func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := FromContext(r.Context())
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			if id := extract(r); id == "" || !claims.Authorized(id) {
				httpx.WriteError(w, http.StatusForbidden, "account_not_authorized", "token is not authorized for this account")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole denies the request (403 forbidden) unless the token carries role. Must run after
// Middleware.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := FromContext(r.Context())
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}
			if claims.Role != role {
				httpx.WriteError(w, http.StatusForbidden, "forbidden", "token lacks the required role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ErrNoClaims is returned by Subject when the context has no verified token.
var ErrNoClaims = errors.New("auth: no claims in context")

// Subject returns the token subject from the context, or "" if absent.
func Subject(r *http.Request) string {
	if c, ok := FromContext(r.Context()); ok {
		return c.Subject
	}
	return ""
}
