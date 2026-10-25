package secretbox

import (
	"bytes"
	"encoding/base64"
	"testing"
)

var key = bytes.Repeat([]byte{7}, 32)

func TestSecretboxRoundTripAndTamper(t *testing.T) {
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := b.Seal([]byte("0012345678"))
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := b.Seal([]byte("0012345678"))
	if s1 == s2 {
		t.Fatal("two seals of the same input must differ (random nonce)")
	}
	got, err := b.Open(s1)
	if err != nil || string(got) != "0012345678" {
		t.Fatalf("open: %q %v", got, err)
	}
	raw, _ := base64.StdEncoding.DecodeString(s1)
	raw[len(raw)-1] ^= 1 // flip a ciphertext byte (not a base64 padding bit)
	if _, err := b.Open(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}
	other, _ := New(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open(s1); err == nil {
		t.Fatal("wrong key must not open")
	}
	if _, err := New(key[:31]); err == nil {
		t.Fatal("31-byte key accepted")
	}
	if Last4("0012345678") != "5678" || Last4("12") != "12" {
		t.Fatal("Last4")
	}
}
