// Command gateway serves the public REST API.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	billingv1 "github.com/acme/acme-card/gen/go/acme/billing/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/pkg/idempotency"
	"github.com/acme/acme-card/pkg/logging"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/ratelimit"
	"github.com/acme/acme-card/services/gateway/internal/server"
	"github.com/acme/acme-card/services/gateway/migrations"
)

func main() {
	cfg, err := config.Load()
	log := logging.New(cfg.LogLevel, "gateway")
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	if cfg.JWTHS256Secret == "" || cfg.BankWebhookSecret == "" || cfg.NetworkWebhookSecret == "" {
		log.Error("JWT_HS256_SECRET, BANK_WEBHOOK_SECRET and NETWORK_WEBHOOK_SECRET are required")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL, "gateway", migrations.FS, "."); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "gateway")
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	// Readiness gets its own tiny pool so a saturated request pool does not fail /readyz on every
	// replica at once (which would make the load balancer drop all targets).
	probePool, err := db.ConnectN(ctx, cfg.DatabaseURL, "gateway", 2)
	if err != nil {
		log.Error("db probe pool", "err", err)
		os.Exit(1)
	}
	defer probePool.Close()
	clients, err := awsx.New(ctx, cfg.AWSRegion, cfg.AWSEndpointURL)
	if err != nil {
		log.Error("aws", "err", err)
		os.Exit(1)
	}
	go outbox.NewRelay(pool, events.NewSNSPublisher(clients.SNS, cfg.SNSTopicARN), 100).Run(ctx, 250*time.Millisecond, log)

	conn, err := grpcx.Dial(cfg.AccountsGRPCAddr)
	if err != nil {
		log.Error("dial accounts", "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	billingConn, err := grpcx.Dial(cfg.BillingGRPCAddr)
	if err != nil {
		log.Error("dial billing", "err", err)
		os.Exit(1)
	}
	defer billingConn.Close()
	if cfg.DevClockEnabled {
		if cfg.AppEnv != "dev" { // allow-list: only the local dev environment
			log.Error("DEV_CLOCK_ENABLED is only allowed with APP_ENV=dev", "app_env", cfg.AppEnv)
			os.Exit(1)
		}
		log.Warn("simulated account clock route enabled (dev only)")
	}

	idem := idempotency.NewStore(pool, cfg.IdempotencyTTL)
	limiter := ratelimit.New(pool, cfg.RateLimitPerMinute, time.Minute)
	go sweeper(ctx, log, idem, limiter)

	handler := server.New(server.Deps{
		Accounts:         accountsv1.NewAccountsServiceClient(conn),
		Billing:          billingv1.NewBillingServiceClient(billingConn),
		DevClock:         cfg.DevClockEnabled,
		JWTSecret:        []byte(cfg.JWTHS256Secret),
		BankSecret:       []byte(cfg.BankWebhookSecret),
		NetworkSecret:    []byte(cfg.NetworkWebhookSecret),
		WebhookTolerance: cfg.BankWebhookTolerance,
		Rejections:       server.NewRejectionLog(pool, 5*time.Minute),
		RequestTimeout:   10 * time.Second,
		Idempotency:      idem,
		Limiter:          limiter,
		AuditSink:        pool,
		TrustedProxies:   cfg.TrustedProxies,
		Ready: []func(context.Context) error{
			probePool.Ping,
			func(ctx context.Context) error {
				res, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
				if err != nil {
					return err
				}
				if res.Status != grpc_health_v1.HealthCheckResponse_SERVING {
					return errors.New("accounts not serving")
				}
				return nil
			},
		},
	})
	srv := &http.Server{Addr: cfg.GatewayHTTPAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("gateway listening", "addr", cfg.GatewayHTTPAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// sweeper removes expired idempotency keys (24h TTL) and stale rate-limit windows every hour.
func sweeper(ctx context.Context, log interface {
	Info(string, ...any)
	Error(string, ...any)
}, idem *idempotency.Store, lim *ratelimit.Limiter) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := idem.Sweep(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			log.Error("idempotency sweep", "err", err)
		} else if n > 0 {
			log.Info("idempotency keys swept", "count", n)
		}
		if err := lim.Sweep(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Error("ratelimit sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
