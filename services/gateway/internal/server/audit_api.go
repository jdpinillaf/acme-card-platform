package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/httpx"
	"github.com/acme/acme-card/pkg/outbox"
)

// emitAuditEvent writes an audit-API meta event (audit.queried / audit.exported /
// audit.access_denied) to the gateway outbox, so it is chained like every other event.
func (s *server) emitAuditEvent(r *http.Request, typ string, after map[string]any) {
	ev := events.New(typ, "audit", r.URL.Path)
	ev.Actor = auth.Subject(r)
	after["method"], after["route"] = r.Method, routePattern(r)
	ev.After, _ = json.Marshal(after)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	var err error
	switch {
	case s.auditEmit != nil:
		err = s.auditEmit(ctx, ev)
	case s.AuditSink != nil:
		err = outbox.Enqueue(ctx, s.AuditSink, ev)
	}
	if err != nil {
		slog.ErrorContext(ctx, "audit-api meta event lost", "type", typ, "err", err)
	}
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return r.URL.Path
}

// requireAuditor is RequireRole(auditor) that audits refusals.
func (s *server) requireAuditor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.FromContext(r.Context())
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		if claims.Role != auth.RoleAuditor {
			s.emitAuditEvent(r, "audit.access_denied", map[string]any{"role": claims.Role})
			httpx.WriteError(w, http.StatusForbidden, "forbidden", "token lacks the auditor role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) queried(r *http.Request, detail map[string]any) {
	s.emitAuditEvent(r, "audit.queried", detail)
}

func parseTime(v string) (*timestamppb.Timestamp, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, err
	}
	return timestamppb.New(t), nil
}

func auditEventJSON(e *auditv1.AuditEvent) map[string]any {
	out := map[string]any{"id": e.Id, "event_type": e.EventType, "aggregate_type": e.AggregateType, "aggregate_id": e.AggregateId, "actor": e.Actor,
		"http_status": e.HttpStatus, "occurred_at": ts(e.OccurredAt), "recorded_at": ts(e.RecordedAt), "source": e.Source, "source_seq": e.SourceSeq,
		"chain_seq": e.ChainSeq, "hash": e.Hash, "redacted": e.Redacted}
	if len(e.BeforeJson) > 0 {
		out["before"] = json.RawMessage(e.BeforeJson)
	}
	if len(e.AfterJson) > 0 {
		out["after"] = json.RawMessage(e.AfterJson)
	}
	return out
}

func (s *server) auditEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := parseTime(q.Get("from"))
	to, err2 := parseTime(q.Get("to"))
	if err1 != nil || err2 != nil {
		httpx.WriteError(w, 400, "invalid_request", "from/to must be RFC 3339")
		return
	}
	size, _ := strconv.Atoi(q.Get("page_size"))
	filter := &auditv1.EventFilter{AccountId: q.Get("account_id"), EventType: q.Get("event_type"), Actor: q.Get("actor"), From: from, To: to}
	res, err := s.Audit.QueryEvents(r.Context(), &auditv1.QueryEventsRequest{Filter: filter, PageToken: q.Get("page_token"), PageSize: int32(min(max(size, 0), 500))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(res.Events))
	for _, e := range res.Events {
		out = append(out, auditEventJSON(e))
	}
	s.queried(r, map[string]any{"filter": map[string]any{"account_id": filter.AccountId, "event_type": filter.EventType, "actor": filter.Actor,
		"from": q.Get("from"), "to": q.Get("to")}, "results": len(out)})
	httpx.WriteJSON(w, 200, map[string]any{"events": out, "next_page_token": res.NextPageToken})
}

func (s *server) auditVerify(w http.ResponseWriter, r *http.Request) {
	res, err := s.Audit.Verify(r.Context(), &auditv1.VerifyRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	gaps := make([]map[string]any, 0, len(res.Gaps))
	for _, g := range res.Gaps {
		gaps = append(gaps, map[string]any{"source": g.Source, "from_seq": g.FromSeq, "to_seq": g.ToSeq})
	}
	stuck := make([]map[string]any, 0, len(res.StuckOutbox))
	for _, st := range res.StuckOutbox {
		stuck = append(stuck, map[string]any{"source": st.Source, "count": st.Count, "oldest": ts(st.Oldest)})
	}
	s.queried(r, map[string]any{"chain_valid": res.ChainValid})
	httpx.WriteJSON(w, 200, map[string]any{"chain_valid": res.ChainValid, "first_bad_chain_seq": res.FirstBadChainSeq, "checked": res.Checked,
		"checkpoints_valid": res.CheckpointsValid, "checkpoints": res.Checkpoints, "gaps": gaps, "stuck_outbox": stuck, "detail": res.Detail})
}

func (s *server) auditCheckpoint(w http.ResponseWriter, r *http.Request) {
	res, err := s.Audit.CreateCheckpoint(r.Context(), &auditv1.CreateCheckpointRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.queried(r, map[string]any{"chain_seq": res.ChainSeq})
	httpx.WriteJSON(w, 201, map[string]any{"chain_seq": res.ChainSeq, "hash": res.Hash, "created_at": ts(res.CreatedAt),
		"prev_checkpoint_sha256": res.PrevCheckpointSha256, "s3_key": res.S3Key})
}

func reconciliationJSON(r *auditv1.ReconciliationReport) map[string]any {
	checks := make([]map[string]any, 0, len(r.Checks))
	for _, c := range r.Checks {
		checks = append(checks, map[string]any{"name": c.Name, "violations": c.Violations})
	}
	return map[string]any{"run_id": r.RunId, "ran_at": ts(r.RanAt), "total_violations": r.TotalViolations, "checks": checks, "s3_key": r.S3Key}
}

func (s *server) auditRunReconciliation(w http.ResponseWriter, r *http.Request) {
	res, err := s.Audit.RunReconciliation(r.Context(), &auditv1.RunReconciliationRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.queried(r, map[string]any{"run_id": res.RunId, "total_violations": res.TotalViolations})
	httpx.WriteJSON(w, 201, reconciliationJSON(res))
}

func (s *server) auditGetReconciliation(w http.ResponseWriter, r *http.Request) {
	res, err := s.Audit.GetReconciliation(r.Context(), &auditv1.GetReconciliationRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.queried(r, map[string]any{"run_id": res.RunId})
	httpx.WriteJSON(w, 200, reconciliationJSON(res))
}

func (s *server) auditReplay(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "accountID")
	res, err := s.Audit.Replay(r.Context(), &auditv1.ReplayRequest{AccountId: id})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	diffs := make([]map[string]any, 0, len(res.Differences))
	for _, d := range res.Differences {
		diffs = append(diffs, map[string]any{"field": d.Field, "audit": d.Audit, "live": d.Live})
	}
	s.queried(r, map[string]any{"account_id": id, "differences": len(diffs)})
	httpx.WriteJSON(w, 200, map[string]any{"account_id": res.AccountId, "based_on_event": res.BasedOnEvent, "differences": diffs})
}

func (s *server) auditCreateExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filter struct {
			AccountID string `json:"account_id"`
			EventType string `json:"event_type"`
			Actor     string `json:"actor"`
			From      string `json:"from"`
			To        string `json:"to"`
		} `json:"filter"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	from, err1 := parseTime(body.Filter.From)
	to, err2 := parseTime(body.Filter.To)
	if err1 != nil || err2 != nil {
		httpx.WriteError(w, 400, "invalid_request", "from/to must be RFC 3339")
		return
	}
	res, err := s.Audit.CreateExport(r.Context(), &auditv1.CreateExportRequest{Filter: &auditv1.EventFilter{AccountId: body.Filter.AccountID,
		EventType: body.Filter.EventType, Actor: body.Filter.Actor, From: from, To: to}, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.emitAuditEvent(r, "audit.exported", map[string]any{"export_id": res.Id, "count": res.Count, "sha256": res.Sha256,
		"filter": body.Filter})
	httpx.WriteJSON(w, 201, map[string]any{"id": res.Id, "count": res.Count, "sha256": res.Sha256, "s3_key": res.S3Key, "created_at": ts(res.CreatedAt)})
}

func (s *server) auditGetExport(w http.ResponseWriter, r *http.Request) {
	res, err := s.Audit.GetExport(r.Context(), &auditv1.GetExportRequest{Id: chi.URLParam(r, "exportID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.queried(r, map[string]any{"export_id": res.Export.GetId(), "downloaded": true})
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(res.Body)
}

func (s *server) auditSnapshots(w http.ResponseWriter, r *http.Request) {
	res, err := s.Accounts.SnapshotAccounts(r.Context(), &accountsv1.SnapshotAccountsRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	s.queried(r, map[string]any{"snapshots": res.Count})
	httpx.WriteJSON(w, 201, map[string]any{"count": res.Count})
}
