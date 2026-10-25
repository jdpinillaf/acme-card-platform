package idempotency

import "testing"

func TestKeyHashIsSHA256HexAndScopedBySubject(t *testing.T) {
	h1 := KeyHash("customer-a", "key-1")
	h2 := KeyHash("customer-b", "key-1")
	if len(h1) != 64 || h1 == h2 {
		t.Fatalf("expected 64-hex, subject-scoped hashes; got %q %q", h1, h2)
	}
	if KeyHash("customer-a", "key-1") != h1 {
		t.Fatal("hash must be deterministic")
	}
}

func TestRequestHashCoversMethodPathBody(t *testing.T) {
	a := RequestHash("POST", "/v1/accounts", []byte(`{"x":1}`))
	if a != RequestHash("POST", "/v1/accounts", []byte(`{"x":1}`)) {
		t.Fatal("deterministic")
	}
	if a == RequestHash("PUT", "/v1/accounts", []byte(`{"x":1}`)) {
		t.Fatal("method must matter")
	}
	if a == RequestHash("POST", "/v1/accountz", []byte(`{"x":1}`)) {
		t.Fatal("path must matter")
	}
	if a == RequestHash("POST", "/v1/accounts", []byte(`{"x":2}`)) {
		t.Fatal("body must matter")
	}
}

func TestOutcomeForStatus(t *testing.T) {
	cases := map[int]Outcome{200: OutcomeSuccess, 201: OutcomeSuccess, 400: OutcomeClientError, 409: OutcomeClientError, 500: OutcomeServerError, 504: OutcomeTimeout}
	for code, want := range cases {
		if got := OutcomeForStatus(code); got != want {
			t.Fatalf("%d: want %s got %s", code, want, got)
		}
	}
}
