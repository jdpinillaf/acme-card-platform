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
func NewClient(pool *pgxpool.Pool) ledgerv1.LedgerServiceClient {
	return inproc{ledger.NewService(pool)}
}

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

func (c inproc) PlaceHold(ctx context.Context, r *ledgerv1.PlaceHoldRequest, _ ...grpc.CallOption) (*ledgerv1.PlaceHoldResponse, error) {
	return c.s.PlaceHold(ctx, r)
}

func (c inproc) CaptureHold(ctx context.Context, r *ledgerv1.CaptureHoldRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.CaptureHold(ctx, r)
}

func (c inproc) ReleaseHold(ctx context.Context, r *ledgerv1.ReleaseHoldRequest, _ ...grpc.CallOption) (*ledgerv1.ReleaseHoldResponse, error) {
	return c.s.ReleaseHold(ctx, r)
}

func (c inproc) PostRefund(ctx context.Context, r *ledgerv1.PostRefundRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostRefund(ctx, r)
}

func (c inproc) PostPayment(ctx context.Context, r *ledgerv1.PostPaymentRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostPayment(ctx, r)
}

func (c inproc) PostPaymentReturn(ctx context.Context, r *ledgerv1.PostPaymentReturnRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostPaymentReturn(ctx, r)
}

func (c inproc) PostCharge(ctx context.Context, r *ledgerv1.PostChargeRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostCharge(ctx, r)
}

func (c inproc) PostCollateralApplication(ctx context.Context, r *ledgerv1.PostCollateralApplicationRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostCollateralApplication(ctx, r)
}

func (c inproc) SealPeriod(ctx context.Context, r *ledgerv1.SealPeriodRequest, _ ...grpc.CallOption) (*ledgerv1.SealPeriodResponse, error) {
	return c.s.SealPeriod(ctx, r)
}

func (c inproc) ListEntries(ctx context.Context, r *ledgerv1.ListEntriesRequest, _ ...grpc.CallOption) (*ledgerv1.ListEntriesResponse, error) {
	return c.s.ListEntries(ctx, r)
}

func (c inproc) PostPayout(ctx context.Context, r *ledgerv1.PostPayoutRequest, _ ...grpc.CallOption) (*ledgerv1.PostJournalResponse, error) {
	return c.s.PostPayout(ctx, r)
}
