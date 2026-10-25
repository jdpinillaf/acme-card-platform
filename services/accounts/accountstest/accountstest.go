// Package accountstest exposes the accounts server for cross-service tests.
package accountstest

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/services/accounts/internal/accounts"
)

// Option configures the test server.
type Option func(*[]accounts.Option)

// WithDestinationKey enables closes with a 32-byte destination key.
func WithDestinationKey(key []byte) Option {
	return func(o *[]accounts.Option) {
		box, err := secretbox.New(key)
		if err != nil {
			panic(err)
		}
		*o = append(*o, accounts.WithDestinationBox(box))
	}
}

// WithBilling lets the close saga cut final statements.
func WithBilling(b billingv1.BillingServiceClient) Option {
	return func(o *[]accounts.Option) { *o = append(*o, accounts.WithBilling(b)) }
}

// WithReturnWindow shortens the payment return window (load/e2e tests).
func WithReturnWindow(d time.Duration) Option {
	return func(o *[]accounts.Option) { *o = append(*o, accounts.WithReturnWindow(d)) }
}

// WithVelocity overrides the per-card authorization velocity limit (default 10 per minute).
func WithVelocity(limit int, window time.Duration) Option {
	return func(o *[]accounts.Option) { *o = append(*o, accounts.WithVelocity(limit, window)) }
}

// NewServer returns the accounts gRPC implementation on pool (accounts schema).
func NewServer(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient, opts ...Option) accountsv1.AccountsServiceServer {
	var os []accounts.Option
	for _, o := range opts {
		o(&os)
	}
	return accounts.NewService(pool, ledger, os...)
}
