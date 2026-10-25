// Package audittest exposes the audit consumer for cross-service tests.
package audittest

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

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
