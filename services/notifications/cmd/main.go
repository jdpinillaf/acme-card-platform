// Command notifications turns domain events into customer notifications (push, SMS, email;
// simulated) and signed partner webhooks (Sprint 7, ADR-022).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/notifications/migrations"
)

type unimplemented struct {
	notificationsv1.UnimplementedNotificationsServiceServer
}

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
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100, "notifications").Run(ctx, 250*time.Millisecond, log)
	log.Info("notifications api listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { notificationsv1.RegisterNotificationsServiceServer(s, unimplemented{}) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
