// Package accountstest exposes the accounts server for cross-service tests.
package accountstest

import (
	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/services/accounts/internal/accounts"
)

// NewServer returns the accounts gRPC implementation on pool (accounts schema).
func NewServer(pool *pgxpool.Pool, ledger ledgerv1.LedgerServiceClient) accountsv1.AccountsServiceServer {
	return accounts.NewService(pool, ledger)
}
