// Package audit is the audit service's read and integrity side (Sprint 6, ADR-021): query,
// hash-chain verification, gap and outbox-lag detection, signed checkpoints, replay,
// reconciliation, exports and retention.
package audit

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/services/audit/internal/store"
)

// Config tunes the integrity jobs.
type Config struct {
	ProducerSchemas []string      // schemas whose outbox is checked for stuck rows (= source names)
	GapGrace        time.Duration // a gap is reported once the event after it is this old
	StuckAfter      time.Duration // an unpublished outbox row older than this is stuck
	Retention       time.Duration // hot retention before archiving (13 months)
	ArchiveLock     time.Duration // object-lock retention of checkpoints and archives (7 years)
}

func (c Config) withDefaults() Config {
	if c.GapGrace == 0 {
		c.GapGrace = 2 * time.Minute
	}
	if c.StuckAfter == 0 {
		c.StuckAfter = 5 * time.Minute
	}
	if c.Retention == 0 {
		c.Retention = 13 * 30 * 24 * time.Hour
	}
	if c.ArchiveLock == 0 {
		c.ArchiveLock = 7 * 365 * 24 * time.Hour
	}
	return c
}

// Service implements auditv1.AuditServiceServer.
type Service struct {
	auditv1.UnimplementedAuditServiceServer
	pool    *pgxpool.Pool
	store   *store.Store
	objects ObjectStore
	key     ed25519.PrivateKey
	cfg     Config
	now     func() time.Time
	// dailyJobs run after the daily checkpoint (reconciliation, retention).
	dailyJobs []func(context.Context) error
}

// NewService builds the service; key signs checkpoints and archive manifests.
func NewService(pool *pgxpool.Pool, objects ObjectStore, key ed25519.PrivateKey, cfg Config) *Service {
	return &Service{pool: pool, store: store.New(pool), objects: objects, key: key, cfg: cfg.withDefaults(), now: func() time.Time { return time.Now().UTC() }}
}
