// Package accountstest exposes the accounts server for cross-service tests.
package accountstest

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/services/accounts/internal/accounts"
)

// Option configures the accounts service under test by appending service options.
type Option func(*[]accounts.Option)

// WithVelocity overrides the per-card authorization velocity limit (default 10 per minute).
func WithVelocity(limit int, window time.Duration) Option {
	return func(o *[]accounts.Option) { *o = append(*o, accounts.WithVelocity(limit, window)) }
}

// NewServer returns the accounts gRPC implementation on pool (accounts schema).
func NewServer(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient, opts ...Option) accountsv1.AccountsServiceServer {
	var o []accounts.Option
	for _, f := range opts {
		f(&o)
	}
	return accounts.NewService(pool, ledger, o...)
}
