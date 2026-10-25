// Package billingtest exposes the billing server for cross-service tests.
package billingtest

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/services/billing/internal/billing"
)

// Store reads statement documents back (the in-memory S3 stand-in).
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// NewServer returns the billing gRPC implementation with USD pricing (22.80% APR, $25 floor and
// late fee) and an in-memory statement store.
func NewServer(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient, accounts accountsv1.AccountsServiceClient) (billingv1.BillingServiceServer, Store) {
	store := billing.NewMemStore()
	return billing.NewService(pool, ledger, accounts, store, billing.Config{APRBps: 2280, Fixed: map[string]int64{"USD": 2500}, Bucket: "test"}), store
}
