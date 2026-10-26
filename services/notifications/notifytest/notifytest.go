// Package notifytest runs the notifications service in-process for cross-service tests.
package notifytest

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/services/notifications/internal/notify"
)

// Start builds the service with simulated providers, runs its SQS consumer and delivery loops
// until ctx ends, and returns its gRPC implementation. allowedHTTPHosts lets test partners
// (httptest on 127.0.0.1) receive webhooks.
func Start(ctx context.Context, pool *pgxpool.Pool, client *sqs.Client, queueURL string, accounts accountsv1.AccountsServiceClient,
	secretKey []byte, allowedHTTPHosts []string) (notificationsv1.NotificationsServiceServer, error) {
	box, err := secretbox.New(secretKey)
	if err != nil {
		return nil, err
	}
	svc := notify.NewService(pool, accounts, notify.SimProviders{}, box, notify.Config{AllowedHTTPHosts: allowedHTTPHosts})
	go notify.NewConsumer(client, queueURL, svc, nil).Run(ctx)
	go svc.RunDelivery(ctx, 100*time.Millisecond, nil)
	return svc, nil
}
