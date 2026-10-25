package config

import (
	"testing"
	"time"
)

func TestLoadReadsEnvWithDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("BANK_WEBHOOK_SECRET", "s")
	t.Setenv("IDEMPOTENCY_TTL", "")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != "postgres://x" {
		t.Fatalf("DatabaseURL = %q", c.DatabaseURL)
	}
	if c.IdempotencyTTL != 24*time.Hour {
		t.Fatalf("default TTL = %v", c.IdempotencyTTL)
	}
	if c.RateLimitPerMinute != 60 {
		t.Fatalf("default rate = %d", c.RateLimitPerMinute)
	}
	if c.GatewayHTTPAddr != ":8080" {
		t.Fatalf("default addr = %q", c.GatewayHTTPAddr)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when DATABASE_URL missing")
	}
}

func TestLoadRejectsShortJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_HS256_SECRET", "short")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for JWT secret shorter than 32 bytes")
	}
}

func TestLoadParsesDurationAndInt(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("IDEMPOTENCY_TTL", "1h")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "5")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.IdempotencyTTL != time.Hour || c.RateLimitPerMinute != 5 {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadBuildsDSNFromRDSSecretWithTLS(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_HS256_SECRET", "")
	t.Setenv("DB_HOST", "acme.abc.us-east-1.rds.amazonaws.com:5432")
	t.Setenv("DB_CREDENTIALS", `{"username":"acme_admin","password":"p@ss/w:rd"}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://acme_admin:p%40ss%2Fw%3Ard@acme.abc.us-east-1.rds.amazonaws.com:5432/acmecard?sslmode=verify-full&sslrootcert=%2Fetc%2Fssl%2Frds%2Fglobal-bundle.pem"
	if c.DatabaseURL != want {
		t.Fatalf("got  %s\nwant %s", c.DatabaseURL, want)
	}
}

func TestLoadRejectsMalformedRDSSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "h:5432")
	t.Setenv("DB_CREDENTIALS", `not-json`)
	if _, err := Load(); err == nil {
		t.Fatal("malformed DB_CREDENTIALS must fail")
	}
}

func TestLoadParsesTrustedProxies(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 172.16.0.0/12")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 || c.TrustedProxies[1].String() != "172.16.0.0/12" {
		t.Fatalf("%v", c.TrustedProxies)
	}
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("bad CIDR must fail")
	}
}

func TestLoadReadsNetworkWebhookSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("NETWORK_WEBHOOK_SECRET", "net-secret")
	c, err := Load()
	if err != nil || c.NetworkWebhookSecret != "net-secret" {
		t.Fatalf("%v %q", err, c.NetworkWebhookSecret)
	}
}

func TestLoadBillingDefaultsAndDevClock(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("BILLING_APR_BPS", "")
	t.Setenv("DEV_CLOCK_ENABLED", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReturnWindow != 120*time.Hour || c.BillingAPRBps != 2280 || c.StatementsBucket != "acme-card-statements-dev" || c.BillingFixedAmounts == "" || c.DevClockEnabled {
		t.Fatalf("billing defaults: %+v", c)
	}
	t.Setenv("DEV_CLOCK_ENABLED", "1")
	t.Setenv("BILLING_APR_BPS", "1999")
	if c, _ = Load(); !c.DevClockEnabled || c.BillingAPRBps != 1999 {
		t.Fatalf("overrides: %+v", c)
	}
}
