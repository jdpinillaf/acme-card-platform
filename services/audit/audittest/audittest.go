// Package audittest exposes the audit consumer for cross-service tests.
package audittest

import (
	"context"
	"crypto/ed25519"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/services/audit/internal/audit"
	"github.com/acme/acme-card/services/audit/internal/store"
)

// Consumer polls SQS into audit_events.
type Consumer interface {
	PollOnce(ctx context.Context) (int, error)
	Run(ctx context.Context)
}

// NewConsumer builds a consumer writing into pool (audit schema).
func NewConsumer(client *sqs.Client, queueURL string, pool *pgxpool.Pool) Consumer {
	return store.NewConsumer(client, queueURL, store.New(pool), nil)
}

// NewService builds the audit gRPC service over pool with an in-memory object store (no S3).
func NewService(pool *pgxpool.Pool, key ed25519.PrivateKey) auditv1.AuditServiceServer {
	return audit.NewService(pool, audit.NewMemStore(), key, audit.Config{ProducerSchemas: []string{"accounts", "ledger", "billing", "gateway", "audit"}})
}
