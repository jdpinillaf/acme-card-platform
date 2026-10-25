// Package gatewaytest exposes the gateway handler and its stores for cross-service tests.
package gatewaytest

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/acme/acme-card/pkg/ratelimit"
	"github.com/acme/acme-card/services/gateway/internal/server"
)

// Deps aliases the internal server dependencies.
type Deps = server.Deps

// NewHandler builds the gateway HTTP handler.
func NewHandler(d Deps) http.Handler { return server.New(d) }

// NewLimiter builds the Postgres rate limiter.
func NewLimiter(pool *pgxpool.Pool, perMinute int) server.Limiter {
	return ratelimit.New(pool, perMinute, time.Minute)
}

// NewRejectionLog builds the webhook rejection log.
func NewRejectionLog(pool *pgxpool.Pool) server.RejectionRecorder {
	return server.NewRejectionLog(pool, 5*time.Minute)
}
