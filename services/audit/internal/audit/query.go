package audit

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

const maxPage = 500

// filterSQL builds the shared WHERE clause of queries and exports. An account matches its own
// events (aggregate id) and any event that names it in `after.account_id`.
func filterSQL(f *auditv1.EventFilter, args []any) (string, []any) {
	where := "TRUE"
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + cond + "$" + strconv.Itoa(len(args))
	}
	if f != nil {
		if f.AccountId != "" {
			args = append(args, f.AccountId)
			n := strconv.Itoa(len(args))
			where += " AND (aggregate_id = $" + n + " OR after->>'account_id' = $" + n + ")"
		}
		if f.EventType != "" {
			add("event_type = ", f.EventType)
		}
		if f.Actor != "" {
			add("actor = ", f.Actor)
		}
		if f.From != nil {
			add("occurred_at >= ", f.From.AsTime())
		}
		if f.To != nil {
			add("occurred_at < ", f.To.AsTime())
		}
	}
	return where, args
}

const eventCols = `id::text, event_type, aggregate_type, aggregate_id, coalesce(actor,''), coalesce(http_status,0), coalesce(before::text,''),
	coalesce(after::text,''), occurred_at, recorded_at, coalesce(source,''), coalesce(source_seq,0), chain_seq, hash, redacted`

func scanAuditEvent(row interface{ Scan(...any) error }) (*auditv1.AuditEvent, error) {
	var e auditv1.AuditEvent
	var before, after string
	var occurred, recorded time.Time
	if err := row.Scan(&e.Id, &e.EventType, &e.AggregateType, &e.AggregateId, &e.Actor, &e.HttpStatus, &before, &after, &occurred, &recorded,
		&e.Source, &e.SourceSeq, &e.ChainSeq, &e.Hash, &e.Redacted); err != nil {
		return nil, err
	}
	e.BeforeJson, e.AfterJson = []byte(before), []byte(after)
	e.OccurredAt, e.RecordedAt = timestamppb.New(occurred), timestamppb.New(recorded)
	return &e, nil
}

// QueryEvents pages the trail in chain order (page token = last chain_seq seen).
func (s *Service) QueryEvents(ctx context.Context, req *auditv1.QueryEventsRequest) (*auditv1.QueryEventsResponse, error) {
	size := int(req.PageSize)
	if size <= 0 || size > maxPage {
		size = 100
	}
	after := int64(0)
	if req.PageToken != "" {
		v, err := strconv.ParseInt(req.PageToken, 10, 64)
		if err != nil || v < 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid page token")
		}
		after = v
	}
	where, args := filterSQL(req.Filter, []any{after})
	args = append(args, size+1)
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM audit_events WHERE chain_seq > $1 AND `+where+
		` ORDER BY chain_seq LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	resp := &auditv1.QueryEventsResponse{}
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, internal(err)
		}
		resp.Events = append(resp.Events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	if len(resp.Events) > size {
		resp.Events = resp.Events[:size]
		resp.NextPageToken = strconv.FormatInt(resp.Events[size-1].ChainSeq, 10)
	}
	return resp, nil
}
