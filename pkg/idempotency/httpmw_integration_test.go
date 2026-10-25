//go:build integration

package idempotency

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiddlewareConcurrentSameKeyRunsHandlerOnce(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		time.Sleep(150 * time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"call":%d,"echo":%s}`, n, body)
	})
	srv := httptest.NewServer(Middleware(s, func(*http.Request) string { return "sub" })(handler))
	defer srv.Close()

	const n = 50
	var wg sync.WaitGroup
	results := make([]string, n)
	codes := make([]int, n)
	replayed := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", srv.URL+"/v1/deposits", strings.NewReader(`{"amount":100}`))
			req.Header.Set(HeaderKey, "same-key")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			results[i], codes[i] = string(b), resp.StatusCode
			replayed[i] = resp.Header.Get(HeaderReplayed) == "true"
		}(i)
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", calls.Load())
	}
	replays := 0
	for i := range results {
		if codes[i] != 201 || results[i] != `{"call":1,"echo":{"amount":100}}` {
			t.Fatalf("response %d differs: %d %s", i, codes[i], results[i])
		}
		if replayed[i] {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("replayed=%d want %d", replays, n-1)
	}
}

func TestMiddlewareStoresErrorOutcomesAndRejectsPayloadMismatch(t *testing.T) {
	s := newStore(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetOutcome(r.Context(), OutcomeRolledBack)
		w.WriteHeader(500)
		_, _ = w.Write([]byte("boom"))
	})
	srv := httptest.NewServer(Middleware(s, func(*http.Request) string { return "sub" })(handler))
	defer srv.Close()

	do := func(body string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/x", strings.NewReader(body))
		req.Header.Set(HeaderKey, "k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	r1, b1 := do(`{"a":1}`)
	r2, b2 := do(`{"a":1}`)
	if r1.StatusCode != 500 || r2.StatusCode != 500 || b1 != "boom" || b2 != "boom" {
		t.Fatalf("5xx must be stored and replayed: %d %s / %d %s", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if r2.Header.Get(HeaderReplayed) != "true" {
		t.Fatal("second call must be a replay")
	}
	var outcome string
	if err := s.pool.QueryRow(r1.Request.Context(), `SELECT outcome FROM idempotency_keys WHERE key_hash=$1`, KeyHash("sub", "k")).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != string(OutcomeRolledBack) {
		t.Fatalf("outcome=%s want rolled_back", outcome)
	}
	r3, b3 := do(`{"a":2}`)
	if r3.StatusCode != 422 || !strings.Contains(b3, "idempotency_key_reuse") {
		t.Fatalf("mismatch must be 422: %d %s", r3.StatusCode, b3)
	}
}
