package idempotency

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
}

func TestMiddlewareRequiresKeyOnMutations(t *testing.T) {
	h := Middleware(nil, func(*http.Request) string { return "sub" })(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/x", strings.NewReader("{}")))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "idempotency_key_required") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMiddlewareRejectsOverlongKey(t *testing.T) {
	h := Middleware(nil, func(*http.Request) string { return "sub" })(okHandler())
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/x", strings.NewReader("{}"))
	r.Header.Set(HeaderKey, strings.Repeat("k", 256))
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "idempotency_key_invalid") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMiddlewareSkipsSafeMethods(t *testing.T) {
	h := Middleware(nil, func(*http.Request) string { return "sub" })(okHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/x", nil))
	if w.Code != 204 {
		t.Fatalf("GET must bypass idempotency, code=%d", w.Code)
	}
}
