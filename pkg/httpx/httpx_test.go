package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type payload struct {
	CustomerID string `json:"customer_id"`
	Currency   string `json:"currency"`
}

func TestDecodeStrictAcceptsExactShape(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"customer_id":"c1","currency":"USD"}`))
	var p payload
	if err := DecodeStrict(r, &p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if p.CustomerID != "c1" || p.Currency != "USD" {
		t.Fatalf("%+v", p)
	}
}

func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"customer_id":"c1","currency":"USD","extra":1}`))
	var p payload
	if err := DecodeStrict(r, &p, 1<<20); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}

func TestDecodeStrictRejectsTrailingData(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"customer_id":"c1"} {"again":true}`))
	var p payload
	if err := DecodeStrict(r, &p, 1<<20); err == nil {
		t.Fatal("want error for multiple JSON values")
	}
}

func TestDecodeStrictRejectsOversizedBody(t *testing.T) {
	big := `{"customer_id":"` + strings.Repeat("x", 100) + `"}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(big))
	var p payload
	if err := DecodeStrict(r, &p, 16); err == nil {
		t.Fatal("want error for oversized body")
	}
}

func TestWriteErrorEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusConflict, "account_closed", "account is closed")
	if w.Code != 409 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code=%d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "account_closed" || env.Error.Message != "account is closed" {
		t.Fatalf("%+v", env)
	}
}
