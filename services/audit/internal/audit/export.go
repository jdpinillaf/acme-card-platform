package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/services/audit/internal/store"
)

// maxExport bounds one export; a wider one must be narrowed by filter.
const maxExport = 100_000

// jwtLike catches bearer tokens in free text that no redacted key names (e.g. rows written
// before redaction existed).
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

type exportLine struct {
	ID            string          `json:"id"`
	EventType     string          `json:"event_type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Actor         string          `json:"actor,omitempty"`
	HTTPStatus    int32           `json:"http_status,omitempty"`
	Before        json.RawMessage `json:"before,omitempty"`
	After         json.RawMessage `json:"after,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
	RecordedAt    time.Time       `json:"recorded_at"`
	Source        string          `json:"source,omitempty"`
	SourceSeq     int64           `json:"source_seq,omitempty"`
	ChainSeq      int64           `json:"chain_seq"`
	Hash          string          `json:"hash"`
	Redacted      bool            `json:"redacted"`
}

func exportJSON(e *auditv1.AuditEvent) ([]byte, error) {
	before, rb := store.Redact(e.BeforeJson)
	after, ra := store.Redact(e.AfterJson)
	b, err := json.Marshal(exportLine{ID: e.Id, EventType: e.EventType, AggregateType: e.AggregateType, AggregateID: e.AggregateId, Actor: e.Actor,
		HTTPStatus: e.HttpStatus, Before: before, After: after, OccurredAt: e.OccurredAt.AsTime(), RecordedAt: e.RecordedAt.AsTime(),
		Source: e.Source, SourceSeq: e.SourceSeq, ChainSeq: e.ChainSeq, Hash: e.Hash, Redacted: e.Redacted || rb || ra})
	if err != nil {
		return nil, err
	}
	return jwtLike.ReplaceAll(b, []byte("[redacted]")), nil
}

// CreateExport writes the filtered trail as NDJSON (redacted again on the way out) to
// exports/<id>.ndjson and records who exported what (spec I-9).
func (s *Service) CreateExport(ctx context.Context, req *auditv1.CreateExportRequest) (*auditv1.Export, error) {
	actor := req.GetMeta().GetActor()
	if actor == "" {
		return nil, status.Error(codes.InvalidArgument, "meta.actor is required")
	}
	var buf bytes.Buffer
	var n int64
	token := ""
	for {
		page, err := s.QueryEvents(ctx, &auditv1.QueryEventsRequest{Filter: req.Filter, PageToken: token, PageSize: maxPage})
		if err != nil {
			return nil, err
		}
		for _, e := range page.Events {
			line, err := exportJSON(e)
			if err != nil {
				return nil, internal(err)
			}
			buf.Write(line)
			buf.WriteByte('\n')
			if n++; n > maxExport {
				return nil, status.Errorf(codes.FailedPrecondition, "export exceeds %d events; narrow the filter", maxExport)
			}
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	sum := sha256.Sum256(buf.Bytes())
	ex := &auditv1.Export{Id: uuid.NewString(), Count: n, Sha256: hex.EncodeToString(sum[:]), CreatedAt: timestamppb.New(s.now())}
	ex.S3Key = "exports/" + ex.Id + ".ndjson"
	if err := s.objects.Put(ctx, ex.S3Key, buf.Bytes()); err != nil {
		return nil, internal(err)
	}
	filter, err := json.Marshal(filterDoc(req.Filter))
	if err != nil {
		return nil, internal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_exports (id, actor, filter, count, sha256, s3_key, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ex.Id, actor, filter, ex.Count, ex.Sha256, ex.S3Key, ex.CreatedAt.AsTime()); err != nil {
		return nil, internal(err)
	}
	return ex, nil
}

func filterDoc(f *auditv1.EventFilter) map[string]any {
	d := map[string]any{"account_id": f.GetAccountId(), "event_type": f.GetEventType(), "actor": f.GetActor()}
	if f.GetFrom() != nil {
		d["from"] = f.GetFrom().AsTime()
	}
	if f.GetTo() != nil {
		d["to"] = f.GetTo().AsTime()
	}
	return d
}

// GetExport returns an export and its body, refusing one whose body no longer matches its sha256.
func (s *Service) GetExport(ctx context.Context, req *auditv1.GetExportRequest) (*auditv1.GetExportResponse, error) {
	if _, err := uuid.Parse(req.Id); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid export id")
	}
	ex := &auditv1.Export{}
	var created time.Time
	err := s.pool.QueryRow(ctx, `SELECT id::text, s3_key, count, sha256, created_at FROM audit_exports WHERE id=$1`, req.Id).
		Scan(&ex.Id, &ex.S3Key, &ex.Count, &ex.Sha256, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "export not found")
	}
	if err != nil {
		return nil, internal(err)
	}
	ex.CreatedAt = timestamppb.New(created)
	body, err := s.objects.Get(ctx, ex.S3Key)
	if err != nil {
		return nil, internal(err)
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != ex.Sha256 {
		return nil, status.Error(codes.DataLoss, "export body does not match its sha256")
	}
	return &auditv1.GetExportResponse{Export: ex, Body: body}, nil
}
