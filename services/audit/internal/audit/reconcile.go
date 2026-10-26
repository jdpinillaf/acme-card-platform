package audit

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/outbox"
)

// verifySQL is tests/load/verify.sql (kept identical by a test): every row must report 0.
//
//go:embed verify.sql
var verifySQL string

type reconciliationDoc struct {
	RunID           string           `json:"run_id"`
	RanAt           time.Time        `json:"ran_at"`
	TotalViolations int64            `json:"total_violations"`
	Checks          []reconcileCheck `json:"checks"`
}

type reconcileCheck struct {
	Name       string `json:"name"`
	Violations int64  `json:"violations"`
}

// RunReconciliation runs every integrity check, stores a WORM report and emits
// reconciliation.completed plus one reconciliation.violation (the alert) per failing check.
func (s *Service) RunReconciliation(ctx context.Context, _ *auditv1.RunReconciliationRequest) (*auditv1.ReconciliationReport, error) {
	rows, err := s.pool.Query(ctx, verifySQL)
	if err != nil {
		return nil, internal(err)
	}
	doc := reconciliationDoc{RunID: uuid.Must(uuid.NewV7()).String(), RanAt: s.now()}
	for rows.Next() {
		var c reconcileCheck
		if err := rows.Scan(&c.Name, &c.Violations); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		doc.Checks = append(doc.Checks, c)
		doc.TotalViolations += c.Violations
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, internal(err)
	}
	key := fmt.Sprintf("reconciliation/%s/%s.json", doc.RanAt.Format("2006-01-02"), doc.RunID)
	if err := s.objects.PutLocked(ctx, key, body, doc.RanAt.Add(s.cfg.ArchiveLock)); err != nil {
		return nil, internal(err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO audit_reconciliations (run_id, ran_at, total_violations, report, s3_key) VALUES ($1,$2,$3,$4,$5)`,
			doc.RunID, doc.RanAt, doc.TotalViolations, body, key); err != nil {
			return err
		}
		evs := []events.Event{auditEvent("reconciliation.completed", doc.RunID, map[string]any{"total_violations": doc.TotalViolations, "s3_key": key})}
		for _, c := range doc.Checks {
			if c.Violations > 0 {
				evs = append(evs, auditEvent("reconciliation.violation", doc.RunID, map[string]any{"check": c.Name, "violations": c.Violations, "s3_key": key}))
			}
		}
		return outbox.Enqueue(ctx, tx, evs...)
	})
	if err != nil {
		return nil, internal(err)
	}
	return reconciliationProto(doc, key), nil
}

// GetReconciliation returns the latest run.
func (s *Service) GetReconciliation(ctx context.Context, _ *auditv1.GetReconciliationRequest) (*auditv1.ReconciliationReport, error) {
	var body []byte
	var key string
	err := s.pool.QueryRow(ctx, `SELECT report, s3_key FROM audit_reconciliations ORDER BY ran_at DESC LIMIT 1`).Scan(&body, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "no reconciliation has run yet")
	}
	if err != nil {
		return nil, internal(err)
	}
	var doc reconciliationDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, internal(err)
	}
	return reconciliationProto(doc, key), nil
}

func reconciliationProto(d reconciliationDoc, key string) *auditv1.ReconciliationReport {
	r := &auditv1.ReconciliationReport{RunId: d.RunID, RanAt: timestamppb.New(d.RanAt), TotalViolations: d.TotalViolations, S3Key: key}
	for _, c := range d.Checks {
		r.Checks = append(r.Checks, &auditv1.ReconciliationCheck{Name: c.Name, Violations: c.Violations})
	}
	return r
}

func auditEvent(typ, id string, after map[string]any) events.Event {
	ev := events.New(typ, "audit", id)
	ev.Actor = "audit"
	ev.After, _ = json.Marshal(after)
	return ev
}
