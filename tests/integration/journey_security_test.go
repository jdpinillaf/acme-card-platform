//go:build integration

package integration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/webhook"
)

func TestLeakScan(t *testing.T) {
	clean := `{"id":"3f2e","customer_id":"cust-1","state":"active","credit_limit_minor":100000,"available_credit_minor":75000,"restricted_reason":"payment_returned","statements":[{"sha256":"ab12","document":{"lines":[]}}]}`
	if got := findLeak(clean); got != "" {
		t.Fatalf("clean body flagged by %q", got)
	}
	for _, leaky := range []string{
		`{"error":{"code":"internal","message":"rpc error: code = Internal desc = boom"}}`,
		`{"error":{"message":"ERROR: duplicate key (SQLSTATE 23505)"}}`,
		`{"id":"x","pending_op":"capture"}`,
		`goroutine 12 [running]:\nmain.main()\n\t/app/main.go:42`,
		`{"dsn":"postgres://acme:pw@db/acmecard"}`,
	} {
		if findLeak(leaky) == "" {
			t.Fatalf("leak not detected: %s", leaky)
		}
	}
}

func TestNoneAlgTokenShape(t *testing.T) {
	tok := noneAlgToken("cust-1", []string{"acc-1"})
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[2] != "" {
		t.Fatalf("want header.payload. with empty signature, got %q", tok)
	}
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]string
	if json.Unmarshal(hdr, &h) != nil || h["alg"] != "none" {
		t.Fatalf("header %s", hdr)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	if json.Unmarshal(payload, &c) != nil || c["sub"] != "cust-1" || c["exp"] == nil {
		t.Fatalf("claims %s", payload)
	}
}

// ---------- intruder helpers ----------

// errCode returns error.code from the standard envelope, or "".
func errCode(r resp) string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// noneAlgToken forges an unsigned JWT ("alg":"none") with the gateway's claim shape.
func noneAlgToken(sub string, accounts []string) string {
	now := time.Now()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := enc(map[string]string{"alg": "none", "typ": "JWT"})
	claims := enc(map[string]any{"sub": sub, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "iss": "acme-card", "authorized_accounts": accounts})
	return header + "." + claims + "."
}

// expiredToken is a correctly signed token whose exp is beyond the 30 s leeway.
func expiredToken(t *testing.T, sub string, accounts ...string) string {
	t.Helper()
	tok, err := auth.Sign(jwtSecret, sub, accounts, -2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// foreignSecretToken is signed with a secret the gateway does not know.
func foreignSecretToken(t *testing.T, sub string, accounts ...string) string {
	t.Helper()
	tok, err := auth.Sign([]byte("not-the-gateway-secret-not-the-gateway"), sub, accounts, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// signedAt posts body to path signed with secret as of time at (deposits use body-only signing,
// every other webhook binds method and path), then lets mutate tamper with the request.
func (s *stack) signedAt(t *testing.T, path string, body map[string]any, secret []byte, at time.Time, mutate func(*http.Request)) resp {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.url+path, bytes.NewReader(b))
	var ts, sig string
	if path == "/v1/webhooks/bank/deposits" {
		ts, sig = webhook.Sign(secret, b, at)
	} else {
		ts, sig = webhook.SignRequest(secret, "POST", path, b, at)
	}
	req.Header.Set(webhook.HeaderTimestamp, ts)
	req.Header.Set(webhook.HeaderSignature, sig)
	if mutate != nil {
		mutate(req)
	}
	return do(t, req)
}

// leakPatterns are things no public response may contain: database and RPC internals, Go
// runtime traces, internal column names, secrets and host paths.
var leakPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)pgx|postgres|sqlstate|\bpq:`),
	regexp.MustCompile(`rpc error|grpc|codes\.`),
	regexp.MustCompile(`goroutine \d+|panic|runtime error|\.go:\d+`),
	regexp.MustCompile(`pending_op|lease_until|"owner"|key_hash|request_hash|hold_id|journal_id|exposure_epoch`),
	regexp.MustCompile(`(?i)secret|password|bearer `),
	regexp.MustCompile(`/Users/|/app/|/home/`),
}

// findLeak returns the first pattern raw matches, or "".
func findLeak(raw string) string {
	for _, p := range leakPatterns {
		if p.MatchString(raw) {
			return p.String()
		}
	}
	return ""
}

// assertNoLeak fails the test when a response body leaks internals.
func assertNoLeak(t *testing.T, where string, r resp) {
	t.Helper()
	if p := findLeak(r.raw); p != "" {
		t.Fatalf("%s: response leaks internals (matched %s): %s", where, p, r.raw)
	}
}
