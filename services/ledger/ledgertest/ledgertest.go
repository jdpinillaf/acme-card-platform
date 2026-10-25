// Package ledgertest exposes an in-process ledger client for other services' tests,
// keeping the ledger implementation itself internal.
package ledgertest

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/services/ledger/internal/ledger"
)

type inproc struct{ s *ledger.Service }

// NewClient returns a LedgerServiceClient that calls the service directly on pool (ledger schema).
func NewClient(pool *pgxpool.Pool) ledgerv1.LedgerServiceClient { return inproc{ledger.NewService(pool)} }

// NewServer returns the ledger gRPC server implementation, for tests that need a real network hop.
func NewServer(pool *pgxpool.Pool) ledgerv1.LedgerServiceServer { return ledger.NewService(pool) }

func (c inproc) OpenLedger(ctx context.Context, r *ledgerv1.OpenLedgerRequest, _ ...grpc.CallOption) (*ledgerv1.OpenLedgerResponse, error) {
	return c.s.OpenLedger(ctx, r)
}

func (c inproc) PostJournal(ctx context.Context, r *ledgerv1.PostJournalRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostJournal(ctx, r)
}

func (c inproc) GetBalances(ctx context.Context, r *ledgerv1.GetBalancesRequest, _ ...grpc.CallOption) (*ledgerv1.GetBalancesResponse, error) {
	return c.s.GetBalances(ctx, r)
}
