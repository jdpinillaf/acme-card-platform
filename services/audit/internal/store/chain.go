package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/acme/acme-card/pkg/events"
)

// GenesisHash is the prev_hash of chain_seq 1.
var GenesisHash = strings.Repeat("0", 64)

// redactedKeys are never stored in the trail (spec B-2): matched case-insensitively at any depth.
var redactedKeys = map[string]bool{
	"account_number": true, "holder_name": true, "destination_ciphertext": true,
	"token": true, "access_token": true, "secret": true, "password": true,
}

func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers stay exact
	var v any
	return v, dec.Decode(&v)
}

func redactValue(v any) (any, bool) {
	hit := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if redactedKeys[strings.ToLower(k)] {
				t[k], hit = "[redacted]", true
				continue
			}
			nv, h := redactValue(child)
			t[k], hit = nv, hit || h
		}
	case []any:
		for i, child := range t {
			nv, h := redactValue(child)
			t[i], hit = nv, hit || h
		}
	}
	return v, hit
}

// Redact replaces sensitive values with "[redacted]" and returns normalized JSON (sorted keys,
// exact numbers). Invalid JSON is returned unchanged.
func Redact(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	v, err := decode(raw)
	if err != nil {
		return raw, false
	}
	v, hit := redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return raw, false
	}
	return out, hit
}

// normalize re-encodes JSON with sorted keys so the same value hashes the same before and after a
// jsonb round trip; nil stays nil.
func normalize(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	v, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

type canonicalEvent struct {
	ID                 string          `json:"id"`
	Type               string          `json:"event_type"`
	AggregateType      string          `json:"aggregate_type"`
	AggregateID        string          `json:"aggregate_id"`
	Actor              string          `json:"actor"`
	IdempotencyKeyHash string          `json:"idempotency_key_hash"`
	HTTPStatus         int             `json:"http_status"`
	Before             json.RawMessage `json:"before"`
	After              json.RawMessage `json:"after"`
	OccurredAt         string          `json:"occurred_at"`
	Source             string          `json:"source"`
	SourceSeq          int64           `json:"source_seq"`
	ChainSeq           int64           `json:"chain_seq"`
}

// Canonical is the byte form of an event that the chain hashes (spec I-2): fixed field order,
// normalized before/after, occurred_at at the database's microsecond precision.
func Canonical(ev events.Event, chainSeq int64) ([]byte, error) {
	before, err := normalize(ev.Before)
	if err != nil {
		return nil, err
	}
	after, err := normalize(ev.After)
	if err != nil {
		return nil, err
	}
	if before == nil {
		before = json.RawMessage("null")
	}
	if after == nil {
		after = json.RawMessage("null")
	}
	return json.Marshal(canonicalEvent{ID: ev.ID, Type: ev.Type, AggregateType: ev.AggregateType, AggregateID: ev.AggregateID,
		Actor: ev.Actor, IdempotencyKeyHash: ev.IdempotencyKeyHash, HTTPStatus: ev.HTTPStatus, Before: before, After: after,
		OccurredAt: ev.OccurredAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano),
		Source:     ev.Source, SourceSeq: ev.SourceSeq, ChainSeq: chainSeq})
}

// Hash links an event to its predecessor: sha256(prev_hash ‖ "\n" ‖ canonical).
func Hash(prev string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte("\n"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}
