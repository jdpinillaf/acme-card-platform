package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/events"
)

type fakeAudit struct {
	auditv1.AuditServiceClient
	lastQuery *auditv1.QueryEventsRequest
	calls     []string
}

func (f *fakeAudit) QueryEvents(_ context.Context, r *auditv1.QueryEventsRequest, _ ...grpc.CallOption) (*auditv1.QueryEventsResponse, error) {
	f.lastQuery, f.calls = r, append(f.calls, "query")
	return &auditv1.QueryEventsResponse{Events: []*auditv1.AuditEvent{{Id: "e1", EventType: "account.opened", ChainSeq: 1, Hash: strings.Repeat("a", 64)}}}, nil
}
func (f *fakeAudit) Verify(context.Context, *auditv1.VerifyRequest, ...grpc.CallOption) (*auditv1.VerifyReport, error) {
	f.calls = append(f.calls, "verify")
	return &auditv1.VerifyReport{ChainValid: true, Checked: 10, CheckpointsValid: true}, nil
}
func (f *fakeAudit) CreateCheckpoint(context.Context, *auditv1.CreateCheckpointRequest, ...grpc.CallOption) (*auditv1.Checkpoint, error) {
	f.calls = append(f.calls, "checkpoint")
	return &auditv1.Checkpoint{ChainSeq: 10, Hash: strings.Repeat("b", 64)}, nil
}
func (f *fakeAudit) RunReconciliation(context.Context, *auditv1.RunReconciliationRequest, ...grpc.CallOption) (*auditv1.ReconciliationReport, error) {
	f.calls = append(f.calls, "reconcile")
	return &auditv1.ReconciliationReport{RunId: "r1", Checks: []*auditv1.ReconciliationCheck{{Name: "x", Violations: 0}}}, nil
}
func (f *fakeAudit) GetReconciliation(context.Context, *auditv1.GetReconciliationRequest, ...grpc.CallOption) (*auditv1.ReconciliationReport, error) {
	return &auditv1.ReconciliationReport{RunId: "r1"}, nil
}
func (f *fakeAudit) Replay(_ context.Context, r *auditv1.ReplayRequest, _ ...grpc.CallOption) (*auditv1.ReplayReport, error) {
	return &auditv1.ReplayReport{AccountId: r.AccountId}, nil
}
func (f *fakeAudit) CreateExport(context.Context, *auditv1.CreateExportRequest, ...grpc.CallOption) (*auditv1.Export, error) {
	f.calls = append(f.calls, "export")
	return &auditv1.Export{Id: "x1", Count: 3, Sha256: strings.Repeat("c", 64)}, nil
}
func (f *fakeAudit) GetExport(_ context.Context, r *auditv1.GetExportRequest, _ ...grpc.CallOption) (*auditv1.GetExportResponse, error) {
	return &auditv1.GetExportResponse{Export: &auditv1.Export{Id: r.Id}, Body: []byte("{\"id\":\"e1\"}\n")}, nil
}

type fakeSnapAccounts struct {
	fakeAccounts
	snapped bool
}

func (f *fakeSnapAccounts) SnapshotAccounts(_ context.Context, r *accountsv1.SnapshotAccountsRequest, _ ...grpc.CallOption) (*accountsv1.SnapshotAccountsResponse, error) {
	f.snapped = r.Background // the API never snapshots synchronously: a large book outlasts a request
	return &accountsv1.SnapshotAccountsResponse{Count: 4, Background: r.Background}, nil
}

func auditorToken(t *testing.T) string {
	tok, err := auth.SignWithRole(jwtSecret, "auditor-1", nil, auth.RoleAuditor, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + tok
}

func auditServer(a *fakeAudit, acc *fakeSnapAccounts, emitted *[]events.Event) *server {
	return &server{Deps{Accounts: acc, Audit: a, JWTSecret: jwtSecret, Rejections: &nopRejections{}, RequestTimeout: time.Second, DisableStateful: true,
		auditEmit: func(_ context.Context, ev events.Event) error { *emitted = append(*emitted, ev); return nil }}}
}

func TestAuditAPIRequiresAuditorRole(t *testing.T) {
	var emitted []events.Event
	h := New(auditServer(&fakeAudit{}, &fakeSnapAccounts{}, &emitted).Deps)
	for _, tok := range []string{token(t, accID), supportToken(t)} {
		if w := do(h, "GET", "/v1/audit/verify", "", map[string]string{"Authorization": tok}); w.Code != 403 {
			t.Fatalf("non-auditor: %d", w.Code)
		}
	}
	if len(emitted) != 2 || emitted[0].Type != "audit.access_denied" || emitted[0].Actor != "cust-1" {
		t.Fatalf("denials must be audited: %+v", emitted)
	}
	if w := do(h, "GET", "/v1/audit/verify", "", nil); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
}

func TestAuditAPIRoutesAndMetaAudit(t *testing.T) {
	var emitted []events.Event
	a, acc := &fakeAudit{}, &fakeSnapAccounts{}
	h := New(auditServer(a, acc, &emitted).Deps)
	hdr := map[string]string{"Authorization": auditorToken(t)}
	w := do(h, "GET", "/v1/audit/events?account_id="+accID+"&event_type=account.opened&actor=cust-1&from=2027-01-01T00:00:00Z&to=2027-02-01T00:00:00Z&page_size=50", "", hdr)
	if w.Code != 200 || a.lastQuery.Filter.AccountId != accID || a.lastQuery.Filter.EventType != "account.opened" || a.lastQuery.PageSize != 50 ||
		a.lastQuery.Filter.From.AsTime().Month() != 1 || !strings.Contains(w.Body.String(), `"chain_seq":1`) {
		t.Fatalf("events: %d %s %+v", w.Code, w.Body.String(), a.lastQuery)
	}
	if w := do(h, "GET", "/v1/audit/events?from=not-a-date", "", hdr); w.Code != 400 {
		t.Fatalf("bad date: %d", w.Code)
	}
	for _, c := range []struct{ method, path, body, want string }{
		{"GET", "/v1/audit/verify", "", `"chain_valid":true`},
		{"POST", "/v1/audit/checkpoints", "", `"chain_seq":10`},
		{"POST", "/v1/audit/reconciliation", "", `"run_id":"r1"`},
		{"GET", "/v1/audit/reconciliation", "", `"run_id":"r1"`},
		{"GET", "/v1/audit/replay/" + accID, "", `"differences":[]`},
		{"POST", "/v1/audit/exports", `{"filter":{"account_id":"` + accID + `"}}`, `"count":3`},
		{"GET", "/v1/audit/exports/x1", "", `{"id":"e1"}`},
		{"POST", "/v1/audit/snapshots", "", `"background":true`},
	} {
		w := do(h, c.method, c.path, c.body, hdr)
		if w.Code/100 != 2 || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if w := do(h, "POST", "/v1/audit/exports", `{"filter":{},"x":1}`, hdr); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	queried, exported := 0, 0
	for _, e := range emitted {
		switch e.Type {
		case "audit.queried":
			queried++
		case "audit.exported":
			exported++
		}
		if e.Actor != "auditor-1" {
			t.Fatalf("meta-audit actor: %+v", e)
		}
	}
	if queried < 8 || exported != 1 || !acc.snapped {
		t.Fatalf("queried=%d exported=%d snapped=%v", queried, exported, acc.snapped)
	}
}

type denyAll struct{}

func (denyAll) Allow(context.Context, string, time.Time) (bool, time.Duration, error) {
	return false, time.Second, nil
}

// A rate-limited caller cannot flood the trail with audit.access_denied events.
func TestAuditDenialsAreRateLimited(t *testing.T) {
	var emitted []events.Event
	h := New(Deps{Accounts: &fakeAccounts{}, Audit: &fakeAudit{}, JWTSecret: jwtSecret, Rejections: &nopRejections{}, Limiter: denyAll{}, AuditSink: &countingSink{},
		auditEmit: func(_ context.Context, ev events.Event) error { emitted = append(emitted, ev); return nil }})
	hdr := map[string]string{"Authorization": token(t, "acc-1")}
	for i := 0; i < 5; i++ {
		if w := do(h, "GET", "/v1/audit/verify", "", hdr); w.Code != 429 {
			t.Fatalf("limited denial: %d", w.Code)
		}
	}
	if len(emitted) != 0 {
		t.Fatalf("rate-limited requests emitted %d access_denied events", len(emitted))
	}
}
