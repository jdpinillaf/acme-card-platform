// Package config loads service configuration from the environment.
// Every service reads the same struct; unused fields are simply empty.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const minJWTSecretBytes = 32

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
	BankWebhookTolerance time.Duration

	AccountsGRPCAddr string
	LedgerGRPCAddr   string

	AWSRegion        string
	AWSEndpointURL   string
	SNSTopicARN      string
	AuditSQSQueueURL string
}

// Load reads and validates configuration from the environment.
func Load() (Config, error) {
	c := Config{
		AppEnv:            getenv("APP_ENV", "dev"),
		LogLevel:          getenv("LOG_LEVEL", "info"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		GatewayHTTPAddr:   getenv("GATEWAY_HTTP_ADDR", ":8080"),
		GRPCAddr:          getenv("GRPC_ADDR", ":9000"),
		JWTHS256Secret:    os.Getenv("JWT_HS256_SECRET"),
		BankWebhookSecret: os.Getenv("BANK_WEBHOOK_SECRET"),
		AccountsGRPCAddr:  getenv("ACCOUNTS_GRPC_ADDR", "localhost:9001"),
		LedgerGRPCAddr:    getenv("LEDGER_GRPC_ADDR", "localhost:9002"),
		AWSRegion:         getenv("AWS_REGION", "us-east-1"),
		AWSEndpointURL:    os.Getenv("AWS_ENDPOINT_URL"),
		SNSTopicARN:       os.Getenv("SNS_TOPIC_ARN"),
		AuditSQSQueueURL:  os.Getenv("AUDIT_SQS_QUEUE_URL"),
	}
	if c.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL is required")
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
	return c, nil
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
