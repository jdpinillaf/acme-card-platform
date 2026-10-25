// Package config loads service configuration from the environment.
// Every service reads the same struct; unused fields are simply empty.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const minJWTSecretBytes = 32

// rdsCABundle is baked into the service image (deploy/Dockerfile) so verify-full can validate RDS.
const rdsCABundle = "/etc/ssl/rds/global-bundle.pem"

// Config is the process configuration shared by all services.
type Config struct {
	AppEnv   string
	LogLevel string

	DatabaseURL string

	GatewayHTTPAddr    string
	GRPCAddr           string
	JWTHS256Secret     string
	IdempotencyTTL     time.Duration
	RateLimitPerMinute int

	BankWebhookSecret    string
	NetworkWebhookSecret string
	BankWebhookTolerance time.Duration

	AccountsGRPCAddr string
	LedgerGRPCAddr   string
	BillingGRPCAddr  string

	// Billing (Sprint 4).
	StatementsBucket    string
	BillingAPRBps       int
	BillingFixedAmounts string
	// DevClockEnabled exposes the simulated-clock route at the gateway (dev only).
	DevClockEnabled bool

	AWSRegion        string
	AWSEndpointURL   string
	SNSTopicARN      string
	AuditSQSQueueURL string

	TrustedProxies []netip.Prefix
}

// Load reads and validates configuration from the environment.
func Load() (Config, error) {
	c := Config{
		AppEnv:               getenv("APP_ENV", "dev"),
		LogLevel:             getenv("LOG_LEVEL", "info"),
		DatabaseURL:          os.Getenv("DATABASE_URL"),
		GatewayHTTPAddr:      getenv("GATEWAY_HTTP_ADDR", ":8080"),
		GRPCAddr:             getenv("GRPC_ADDR", ":9000"),
		JWTHS256Secret:       os.Getenv("JWT_HS256_SECRET"),
		BankWebhookSecret:    os.Getenv("BANK_WEBHOOK_SECRET"),
		NetworkWebhookSecret: os.Getenv("NETWORK_WEBHOOK_SECRET"),
		AccountsGRPCAddr:     getenv("ACCOUNTS_GRPC_ADDR", "localhost:9001"),
		LedgerGRPCAddr:       getenv("LEDGER_GRPC_ADDR", "localhost:9002"),
		BillingGRPCAddr:      getenv("BILLING_GRPC_ADDR", "localhost:9004"),
		StatementsBucket:     getenv("STATEMENTS_BUCKET", "acme-card-statements-dev"),
		BillingFixedAmounts:  getenv("BILLING_FIXED_AMOUNTS", "USD:2500,EUR:2500,MXN:45000,COP:10000000"),
		DevClockEnabled:      os.Getenv("DEV_CLOCK_ENABLED") == "1",
		AWSRegion:            getenv("AWS_REGION", "us-east-1"),
		AWSEndpointURL:       os.Getenv("AWS_ENDPOINT_URL"),
		SNSTopicARN:          os.Getenv("SNS_TOPIC_ARN"),
		AuditSQSQueueURL:     os.Getenv("AUDIT_SQS_QUEUE_URL"),
	}
	if c.DatabaseURL == "" && os.Getenv("DB_HOST") != "" {
		dsn, err := dsnFromRDSSecret(os.Getenv("DB_HOST"), os.Getenv("DB_CREDENTIALS"))
		if err != nil {
			return Config{}, err
		}
		c.DatabaseURL = dsn
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL (or DB_HOST + DB_CREDENTIALS) is required")
	}
	if c.JWTHS256Secret != "" && len(c.JWTHS256Secret) < minJWTSecretBytes {
		return Config{}, fmt.Errorf("config: JWT_HS256_SECRET must be at least %d bytes", minJWTSecretBytes)
	}
	var err error
	if c.IdempotencyTTL, err = getDuration("IDEMPOTENCY_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if c.BankWebhookTolerance, err = getDuration("BANK_WEBHOOK_TOLERANCE", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if c.RateLimitPerMinute, err = getInt("RATE_LIMIT_PER_MINUTE", 60); err != nil {
		return Config{}, err
	}
	if c.BillingAPRBps, err = getInt("BILLING_APR_BPS", 2280); err != nil {
		return Config{}, err
	}
	for _, raw := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return Config{}, fmt.Errorf("config: TRUSTED_PROXY_CIDRS: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	return c, nil
}

// dsnFromRDSSecret builds a TLS-verified DSN from the RDS-managed secret JSON injected by ECS.
func dsnFromRDSSecret(host, secretJSON string) (string, error) {
	var cred struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(secretJSON), &cred); err != nil || cred.Username == "" {
		return "", errors.New("config: DB_CREDENTIALS must be the RDS secret JSON with username and password")
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(cred.Username, cred.Password), Host: host, Path: "/acmecard", RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {rdsCABundle}}.Encode()}
	return u.String(), nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return d, nil
}

func getInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return n, nil
}
