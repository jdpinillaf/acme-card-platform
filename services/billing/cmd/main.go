// Command billing generates statements, interest, late fees and delinquency over gRPC (Sprint 4).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/billing/internal/billing"
	"github.com/acme/acme-card/services/billing/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "billing")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	fixed, err := billing.ParseFixedAmounts(cfg.BillingFixedAmounts)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "billing", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	// Each catch-up holds one connection for its account lock and uses others for writes.
	pool, err := db.ConnectN(ctx, cfg.DatabaseURL, "billing", 40)
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
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100).Run(ctx, 250*time.Millisecond, log)

	ledgerConn, err := grpcx.Dial(cfg.LedgerGRPCAddr)
	if err != nil {
		log.Error("dial ledger", "err", err)
		os.Exit(1)
	}
	defer ledgerConn.Close()
	accountsConn, err := grpcx.Dial(cfg.AccountsGRPCAddr)
	if err != nil {
		log.Error("dial accounts", "err", err)
		os.Exit(1)
	}
	defer accountsConn.Close()

	svc := billing.NewService(pool, ledgerv1.NewLedgerServiceClient(ledgerConn), accountsv1.NewAccountsServiceClient(accountsConn),
		billing.NewS3Store(clients.S3, cfg.StatementsBucket), billing.Config{APRBps: int64(cfg.BillingAPRBps), Fixed: fixed, Bucket: cfg.StatementsBucket})
	go svc.RunSweep(ctx, time.Minute, log)
	log.Info("billing listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { billingv1.RegisterBillingServiceServer(s, svc) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
