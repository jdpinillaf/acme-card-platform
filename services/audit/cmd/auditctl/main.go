// auditctl is the operators' tool for the audit trail's integrity and retention (ADR-021). It talks
// to Postgres and the Object-Lock bucket directly, with the audit service's own configuration.
//
//	go run ./services/audit/cmd/auditctl verify-chain
//	go run ./services/audit/cmd/auditctl checkpoint
//	go run ./services/audit/cmd/auditctl archive [--now 2027-12-01T00:00:00Z]
//	go run ./services/audit/cmd/auditctl verify
//	go run ./services/audit/cmd/auditctl purge
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/awsx"
	"github.com/acme/acme-card/pkg/config"
	"github.com/acme/acme-card/pkg/db"
	"github.com/acme/acme-card/services/audit/internal/audit"
)

const usage = "usage: auditctl archive [--now RFC3339] | verify | purge | checkpoint | verify-chain"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	nowFlag := fs.String("now", "", "archive cutoff reference time (default: now)")
	_ = fs.Parse(os.Args[2:])
	now := time.Now().UTC()
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fail(fmt.Errorf("--now: %w", err))
		}
		now = t
	}

	ctx := context.Background()
	svc, closeFn, err := build(ctx)
	if err != nil {
		fail(err)
	}
	defer closeFn()

	var out any
	switch cmd {
	case "archive":
		n, key, err := svc.Archive(ctx, now)
		if err != nil {
			fail(err)
		}
		out = map[string]any{"archived": n, "manifest_key": key}
	case "verify":
		if err := svc.VerifyArchives(ctx); err != nil {
			fail(err)
		}
		out = map[string]any{"archives_verified": true}
	case "purge":
		n, err := svc.Purge(ctx)
		if err != nil {
			fail(err)
		}
		out = map[string]any{"purged": n}
	case "checkpoint":
		if out, err = svc.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{}); err != nil {
			fail(err)
		}
	case "verify-chain":
		r, err := svc.Verify(ctx, &auditv1.VerifyRequest{Full: true})
		if err != nil {
			fail(err)
		}
		out = r
		if !r.ChainValid || !r.CheckpointsValid {
			emit(out)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	emit(out)
}

func build(ctx context.Context) (*audit.Service, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	rawKey, err := base64.StdEncoding.DecodeString(cfg.AuditSigningKey)
	if err != nil || len(rawKey) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("AUDIT_SIGNING_KEY must be a base64 Ed25519 private key (64 bytes)")
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL, "audit")
	if err != nil {
		return nil, nil, err
	}
	clients, err := awsx.New(ctx, cfg.AWSRegion, cfg.AWSEndpointURL)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	svc := audit.NewService(pool, audit.NewS3Store(clients.S3, cfg.AuditBucket), ed25519.PrivateKey(rawKey),
		audit.Config{ProducerSchemas: []string{"accounts", "ledger", "billing", "gateway", "audit"}})
	return svc, pool.Close, nil
}

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "auditctl:", err)
	os.Exit(1)
}
