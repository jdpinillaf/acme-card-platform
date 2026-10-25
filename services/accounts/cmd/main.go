// Command accounts serves account/card lifecycle and collateral deposits over gRPC.
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
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	ledgerv1 "github.com/acme/acme-card/gen/go/acme/ledger/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/services/accounts/internal/accounts"
	"github.com/acme/acme-card/services/accounts/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "accounts")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "accounts", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "accounts")
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

	conn, err := grpcx.Dial(cfg.LedgerGRPCAddr)
	if err != nil {
		log.Error("dial ledger", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	billingConn, err := grpcx.Dial(cfg.BillingGRPCAddr)
	if err != nil {
		log.Error("dial billing", "err", err)
		os.Exit(1)
	}
	defer billingConn.Close()

	opts := []accounts.Option{accounts.WithBilling(billingv1.NewBillingServiceClient(billingConn))}
	if cfg.DestinationKey != "" {
		key, err := base64.StdEncoding.DecodeString(cfg.DestinationKey)
		if err != nil {
			log.Error("DESTINATION_KEY must be base64", "err", err)
			os.Exit(1)
		}
		box, err := secretbox.New(key)
		if err != nil {
			log.Error("DESTINATION_KEY", "err", err)
			os.Exit(1)
		}
		opts = append(opts, accounts.WithDestinationBox(box))
	} else {
		log.Warn("DESTINATION_KEY not set: account closes are refused")
	}
	if cfg.ReturnWindow != accounts.ReturnWindow {
		if cfg.AppEnv != "dev" { // only the dev load test shortens it
			log.Error("RETURN_WINDOW may only be changed with APP_ENV=dev", "return_window", cfg.ReturnWindow)
			os.Exit(1)
		}
		opts = append(opts, accounts.WithReturnWindow(cfg.ReturnWindow))
	}
	svc := accounts.NewService(pool, ledgerv1.NewLedgerServiceClient(conn), opts...)
	go svc.RunExpiry(ctx, time.Minute, log)
	log.Info("accounts listening", "addr", cfg.GRPCAddr)
	if err := grpcx.Serve(ctx, cfg.GRPCAddr, func(s *grpc.Server) { accountsv1.RegisterAccountsServiceServer(s, svc) }); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
