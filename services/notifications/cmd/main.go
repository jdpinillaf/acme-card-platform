// Command notifications turns domain events into customer notifications (push, SMS, email;
// simulated) and signed partner webhooks (Sprint 7, ADR-022).
package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/services/notifications/internal/notify"
	"github.com/acme/acme-card/services/notifications/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "notifications")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "notifications", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "notifications")
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	clients, err := awsx.New(ctx, cfg.AWSRegion, cfg.AWSEndpointURL)
	if err != nil {
		log.Error("aws", "err", err)
		os.Exit(1)
	}
	rawKey, err := base64.StdEncoding.DecodeString(cfg.NotifySecretKey)
	if err != nil {
		log.Error("NOTIFY_SECRET_KEY must be base64", "err", err)
		os.Exit(1)
	}
	box, err := secretbox.New(rawKey)
	if err != nil {
		log.Error("NOTIFY_SECRET_KEY", "err", err)
		os.Exit(1)
	}
	accountsConn, err := grpcx.Dial(cfg.AccountsGRPCAddr)
	if err != nil {
		log.Error("dial accounts", "err", err)
		os.Exit(1)
	}
	defer accountsConn.Close()
	if len(cfg.NotifyFailChannels) > 0 {
		log.Warn("simulated provider failures enabled (dev only)", "channels", cfg.NotifyFailChannels)
	}
	svc := notify.NewService(pool, accountsv1.NewAccountsServiceClient(accountsConn), notify.SimProviders{Fail: cfg.NotifyFailChannels}, box,
		notify.Config{AllowedHTTPHosts: cfg.NotifyAllowedHTTPHosts})
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100, "notifications").Run(ctx, 250*time.Millisecond, log)
	go notify.NewConsumer(clients.SQS, cfg.NotificationsSQSQueueURL, svc, log).Run(ctx)
	go svc.RunDelivery(ctx, 500*time.Millisecond, log)
	log.Info("notifications api listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { notificationsv1.RegisterNotificationsServiceServer(s, svc) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
