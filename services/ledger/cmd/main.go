// Command ledger serves the double-entry ledger over gRPC and relays its outbox to SNS.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/services/ledger/internal/ledger"
	"github.com/acme/acme-card/services/ledger/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "ledger")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "ledger", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "ledger")
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
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100, "ledger").Run(ctx, 250*time.Millisecond, log)

	svc := ledger.NewService(pool)
	// Safety net: accounts expires holds at 7 days; anything still active at 8 days is an orphan.
	go svc.RunStaleHoldSweeper(ctx, 10*time.Minute, 8*24*time.Hour, log)
	log.Info("ledger listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { ledgerv1.RegisterLedgerServiceServer(s, svc) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
