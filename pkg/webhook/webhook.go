// Package webhook signs and verifies HMAC-SHA256 webhooks.
//
// signature = hex(HMAC-SHA256(secret, timestamp + "." + body)), sent with the unix timestamp.
// Binding the timestamp into the MAC prevents replaying an old signature with a fresh timestamp.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-Acme-Timestamp"
	HeaderSignature = "X-Acme-Signature"
)

var (
	ErrMissingHeaders = errors.New("missing_signature_headers")
	ErrStaleTimestamp = errors.New("stale_timestamp")
	ErrBadSignature   = errors.New("bad_signature")
)

func mac(secret, body []byte, ts string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// Sign returns the timestamp and signature headers for body at time now.
func Sign(secret, body []byte, now time.Time) (ts, sig string) {
	ts = strconv.FormatInt(now.Unix(), 10)
	return ts, hex.EncodeToString(mac(secret, body, ts))
}

// Verify checks headers against body. tolerance bounds clock skew in both directions.
func Verify(secret, body []byte, ts, sig string, now time.Time, tolerance time.Duration) error {
	if ts == "" || sig == "" {
		return ErrMissingHeaders
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrStaleTimestamp
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return ErrStaleTimestamp
	}
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(secret, body, ts)) {
		return ErrBadSignature
	}
	return nil
}

// requestPayload binds the HTTP method and path into the signed bytes, so a signed message cannot
// be replayed against another endpoint or another resource id (used by card network routes).
func requestPayload(method, path string, body []byte) []byte {
	out := make([]byte, 0, len(method)+len(path)+len(body)+2)
	out = append(out, method...)
	out = append(out, ' ')
	out = append(out, path...)
	out = append(out, '.')
	return append(out, body...)
}

// SignRequest signs timestamp.METHOD path.body.
func SignRequest(secret []byte, method, path string, body []byte, now time.Time) (ts, sig string) {
	return Sign(secret, requestPayload(method, path, body), now)
}

// VerifyRequest verifies a SignRequest signature.
func VerifyRequest(secret []byte, method, path string, body []byte, ts, sig string, now time.Time, tolerance time.Duration) error {
	return Verify(secret, requestPayload(method, path, body), ts, sig, now, tolerance)
}
