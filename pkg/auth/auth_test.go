package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestSignParseRoundTrip(t *testing.T) {
	tok, err := Sign(secret, "cust-1", []string{"acc-1", "acc-2"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(secret, tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "cust-1" || len(c.AuthorizedAccounts) != 2 || c.AuthorizedAccounts[1] != "acc-2" {
		t.Fatalf("%+v", c)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	tok, _ := Sign(secret, "s", nil, time.Minute)
	if _, err := Parse([]byte("another-secret-another-secret-00"), tok); err == nil {
		t.Fatal("want signature error")
	}
}

func TestParseRejectsExpired(t *testing.T) {
	tok, _ := Sign(secret, "s", nil, -time.Minute)
	if _, err := Parse(secret, tok); err == nil {
		t.Fatal("want expiry error")
	}
}

func TestParseRejectsNonHS256(t *testing.T) {
	// alg=none and alg confusion must be rejected even if the payload is well-formed.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "s", "exp": time.Now().Add(time.Hour).Unix()})
	tok, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := Parse(secret, tok); err == nil {
		t.Fatal("alg=none must be rejected")
	}
	hs512 := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{"sub": "s", "exp": time.Now().Add(time.Hour).Unix()})
	tok2, _ := hs512.SignedString(secret)
	if _, err := Parse(secret, tok2); err == nil {
		t.Fatal("HS512 must be rejected; only HS256 allowed")
	}
}

func TestParseRequiresExp(t *testing.T) {
	noExp := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "s"})
	tok, _ := noExp.SignedString(secret)
	if _, err := Parse(secret, tok); err == nil {
		t.Fatal("exp is mandatory")
	}
}

func TestMiddlewareAndRequireAccount(t *testing.T) {
	var seen *Claims
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(204)
	})
	accountFromQuery := func(r *http.Request) string { return r.URL.Query().Get("account") }
	h := Middleware(secret)(RequireAccount(accountFromQuery)(inner))

	// no token
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/?account=acc-1", nil))
	if w.Code != 401 {
		t.Fatalf("missing token: %d", w.Code)
	}

	tok, _ := Sign(secret, "cust-1", []string{"acc-1"}, time.Minute)

	// authorized account
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/?account=acc-1", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w, r)
	if w.Code != 204 || seen == nil || seen.Subject != "cust-1" {
		t.Fatalf("authorized: code=%d claims=%+v", w.Code, seen)
	}

	// foreign account
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/?account=acc-999", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("foreign account must be 403, got %d", w.Code)
	}

	// malformed token
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/?account=acc-1", nil)
	r.Header.Set("Authorization", "Bearer garbage")
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("garbage token must be 401, got %d", w.Code)
	}
}

func TestRequireRole(t *testing.T) {
	h := Middleware(secret)(RequireRole(RoleSupport)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })))
	call := func(tok string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		h.ServeHTTP(w, r)
		return w.Code
	}
	customer, _ := Sign(secret, "cust-1", []string{"acc-1"}, time.Minute)
	if c := call(customer); c != 403 {
		t.Fatalf("customer token: %d want 403", c)
	}
	support, err := SignWithRole(secret, "agent-7", nil, RoleSupport, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c := call(support); c != 204 {
		t.Fatalf("support token: %d want 204", c)
	}
	claims, err := Parse(secret, support)
	if err != nil || claims.Role != RoleSupport || claims.Subject != "agent-7" {
		t.Fatalf("claims %+v %v", claims, err)
	}
}
