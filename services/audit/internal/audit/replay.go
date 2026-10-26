package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

// replayFields are the account fields replay compares (spec B-1).
var replayFields = []string{"state", "frozen", "collateral_minor", "credit_limit_minor", "purchase_limit_minor", "restricted_reason",
	"delinquency", "close_stage", "clock_offset_seconds"}

// creditNormal ledger sub-accounts are shown negated in events (natural sign).
var creditNormal = map[string]bool{"collateral_held": true, "interest_income": true, "fee_income": true}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case json.Number:
		return t.String()
	case string:
		return t
	case bool:
		return fmt.Sprint(t)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Replay rebuilds an account from the trail — its latest account.changed or snapshot.account, then
// the ledger balances from that snapshot plus later ledger.journal_posted events — and compares it
// with the live rows (read-only reporting access to the accounts and ledger schemas).
func (s *Service) Replay(ctx context.Context, req *auditv1.ReplayRequest) (*auditv1.ReplayReport, error) {
	if _, err := uuid.Parse(req.AccountId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "account_id must be a uuid")
	}
	report := &auditv1.ReplayReport{AccountId: req.AccountId}
	var evID, evType string
	var after []byte
	err := s.pool.QueryRow(ctx, `SELECT id::text, event_type, after::text FROM audit_events
		WHERE aggregate_id=$1 AND event_type IN ('account.changed','snapshot.account') ORDER BY coalesce(source_seq, 0) DESC, chain_seq DESC LIMIT 1`, req.AccountId).
		Scan(&evID, &evType, &after)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "no account state in the audit trail")
	}
	if err != nil {
		return nil, internal(err)
	}
	report.BasedOnEvent = evID
	doc, err := decodeObject(after)
	if err != nil {
		return nil, internal(err)
	}
	account := doc
	balances := map[string]string{}
	if evType == "snapshot.account" {
		account, _ = doc["account"].(map[string]any)
	}
	// Balances: the latest snapshot is the base for sub-accounts no journal touched; every
	// journal carries the exact post-state of the sub-accounts it touched, so journals replayed
	// in ledger order (source_seq, not arrival order) override it.
	var snap []byte
	err = s.pool.QueryRow(ctx, `SELECT after::text FROM audit_events WHERE aggregate_id=$1 AND event_type='snapshot.account'
		ORDER BY coalesce(source_seq, 0) DESC, chain_seq DESC LIMIT 1`, req.AccountId).Scan(&snap)
	if err == nil {
		if d, err := decodeObject(snap); err == nil {
			if b, ok := d["balances"].(map[string]any); ok {
				for k, v := range b {
					balances[k] = valueString(v)
				}
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT after::text FROM audit_events WHERE event_type='ledger.journal_posted' AND after->>'account_id'=$1
		ORDER BY coalesce(source_seq, 0), chain_seq`, req.AccountId)
	if err != nil {
		return nil, internal(err)
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		if d, err := decodeObject(raw); err == nil {
			if b, ok := d["balances"].(map[string]any); ok {
				for k, v := range b {
					balances[k] = valueString(v)
				}
			}
		}
	}
	rows.Close()

	live, err := s.liveAccount(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	for _, f := range replayFields {
		if a, l := valueString(account[f]), live[f]; a != l {
			report.Differences = append(report.Differences, &auditv1.Difference{Field: f, Audit: a, Live: l})
		}
	}
	liveBal, err := s.liveBalances(ctx, req.AccountId)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if balances[k] != liveBal[k] {
			report.Differences = append(report.Differences, &auditv1.Difference{Field: "balance." + k, Audit: balances[k], Live: liveBal[k]})
		}
	}
	return report, nil
}

func (s *Service) liveAccount(ctx context.Context, id string) (map[string]string, error) {
	var state, delinquency string
	var frozen bool
	var coll, limit, plimit, offset int64
	var restricted, stage *string
	err := s.pool.QueryRow(ctx, `SELECT state, frozen, collateral_minor, credit_limit_minor, purchase_limit_minor, restricted_reason, delinquency,
		close_stage, clock_offset_seconds FROM accounts.accounts WHERE id=$1`, id).Scan(&state, &frozen, &coll, &limit, &plimit, &restricted, &delinquency, &stage, &offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "account not found")
	}
	if err != nil {
		return nil, internal(err)
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return map[string]string{"state": state, "frozen": fmt.Sprint(frozen), "collateral_minor": fmt.Sprint(coll), "credit_limit_minor": fmt.Sprint(limit),
		"purchase_limit_minor": fmt.Sprint(plimit), "restricted_reason": str(restricted), "delinquency": delinquency, "close_stage": str(stage),
		"clock_offset_seconds": fmt.Sprint(offset)}, nil
}

func (s *Service) liveBalances(ctx context.Context, id string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT sub_account, raw_balance_minor FROM ledger.balances WHERE account_id=$1`, id)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sa string
		var raw int64
		if err := rows.Scan(&sa, &raw); err != nil {
			return nil, internal(err)
		}
		if creditNormal[sa] {
			raw = -raw
		}
		out[sa] = fmt.Sprint(raw)
	}
	return out, internal(rows.Err())
}
