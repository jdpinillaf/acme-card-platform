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
	want := "postgres://acme_admin:p%40ss%2Fw%3Ard@acme.abc.us-east-1.rds.amazonaws.com:5432/acmecard?sslmode=verify-full"
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
