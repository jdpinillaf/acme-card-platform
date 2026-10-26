// Command audit consumes domain events from SQS into the append-only audit log.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"

	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/audit/internal/audit"
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
	st := store.New(pool)
	// Chain rows written before Sprint 6 once, before consuming new events (ADR-021).
	if n, err := st.BackfillChain(ctx); err != nil {
		log.Error("chain backfill", "err", err)
		os.Exit(1)
	} else if n > 0 {
		log.Info("chained pre-existing audit events", "count", n)
	}
	rawKey, err := base64.StdEncoding.DecodeString(cfg.AuditSigningKey)
	if err != nil || len(rawKey) != ed25519.PrivateKeySize {
		log.Error("AUDIT_SIGNING_KEY must be a base64 Ed25519 private key (64 bytes)")
		os.Exit(1)
	}
	svc := audit.NewService(pool, audit.NewS3Store(clients.S3, cfg.AuditBucket), ed25519.PrivateKey(rawKey),
		audit.Config{ProducerSchemas: []string{"accounts", "ledger", "billing", "gateway", "audit"}})
	go svc.RunDaily(ctx, time.Hour, log)
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100, "audit").Run(ctx, 250*time.Millisecond, log)
	go store.NewConsumer(clients.SQS, cfg.AuditSQSQueueURL, st, log).Run(ctx)
	log.Info("audit api listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { auditv1.RegisterAuditServiceServer(s, svc) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
