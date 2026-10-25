// Command audit consumes domain events from SQS into the append-only audit log.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/services/audit/internal/store"
	"github.com/acme/acme-card/services/audit/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "audit")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "audit", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "audit")
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
	log.Info("audit consumer started", "queue", cfg.AuditSQSQueueURL)
	store.NewConsumer(clients.SQS, cfg.AuditSQSQueueURL, store.New(pool), log).Run(ctx)
}
