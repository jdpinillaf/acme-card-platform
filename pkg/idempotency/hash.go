// Package idempotency implements insert-first request idempotency on Postgres.
//
// The first request for a key inserts a row and keeps the inserting transaction (and therefore the
// row lock) open while the handler runs. Concurrent duplicates block on the primary key, then see
// the completed row and replay its stored response. Keys expire after a TTL and are swept.
package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
)

// Outcome classifies how a request ended; stored alongside the HTTP status for auditing.
type Outcome string

const (
	OutcomeSuccess     Outcome = "success"
	OutcomeClientError Outcome = "client_error"
	OutcomeServerError Outcome = "server_error"
	OutcomeTimeout     Outcome = "timeout"
	OutcomeRolledBack  Outcome = "rolled_back"
)

// KeyHash scopes a client idempotency key to a subject (JWT sub / webhook source) and hashes it.
func KeyHash(subject, key string) string {
	h := sha256.New()
	h.Write([]byte(subject))
	h.Write([]byte{0})
	h.Write([]byte(key))
	return hex.EncodeToString(h.Sum(nil))
}

// RequestHash fingerprints the request so a reused key with a different payload can be rejected.
func RequestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// OutcomeForStatus maps an HTTP status to an Outcome. 504 is treated as a timeout.
func OutcomeForStatus(code int) Outcome {
	switch {
	case code == 504:
		return OutcomeTimeout
	case code >= 500:
		return OutcomeServerError
	case code >= 400:
		return OutcomeClientError
	default:
		return OutcomeSuccess
	}
}
