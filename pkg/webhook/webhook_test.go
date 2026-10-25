package webhook

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

var secret = []byte("bank-dev-secret")

func TestSignVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"deposit_id":"x"}`)
	ts, sig := Sign(secret, body, now)
	if ts != strconv.FormatInt(now.Unix(), 10) || len(sig) != 64 {
		t.Fatalf("ts=%s sig=%s", ts, sig)
	}
	if err := Verify(secret, body, ts, sig, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"a":1}`)
	ts, sig := Sign(secret, body, now)
	cases := []struct {
		name         string
		secret, body []byte
		ts, sig      string
		at           time.Time
		want         error
	}{
		{"tampered body", secret, []byte(`{"a":2}`), ts, sig, now, ErrBadSignature},
		{"wrong secret", []byte("other"), body, ts, sig, now, ErrBadSignature},
		{"garbage sig", secret, body, ts, "zz", now, ErrBadSignature},
		{"stale", secret, body, ts, sig, now.Add(6 * time.Minute), ErrStaleTimestamp},
		{"future", secret, body, ts, sig, now.Add(-6 * time.Minute), ErrStaleTimestamp},
		{"missing ts", secret, body, "", sig, now, ErrMissingHeaders},
		{"missing sig", secret, body, ts, "", now, ErrMissingHeaders},
		{"non-numeric ts", secret, body, "abc", sig, now, ErrStaleTimestamp},
	}
	for _, c := range cases {
		if err := Verify(c.secret, c.body, c.ts, c.sig, c.at, 5*time.Minute); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v got %v", c.name, c.want, err)
		}
	}
}

func TestSignatureBindsTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{}`)
	_, sig := Sign(secret, body, now)
	otherTS := strconv.FormatInt(now.Unix()+1, 10)
	if err := Verify(secret, body, otherTS, sig, now, 5*time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replaying signature with a new timestamp must fail: %v", err)
	}
}
