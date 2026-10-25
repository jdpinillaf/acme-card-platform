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

// A 5xx means the effect did not (observably) happen: the key must be released so the client's
// retry with the same key runs the handler again instead of replaying the failure for 24 hours.
func TestMiddlewareDoesNotStore5xxSoRetryRuns(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			SetOutcome(r.Context(), OutcomeRolledBack)
			w.WriteHeader(500)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
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
	if r1.StatusCode != 500 || b1 != "boom" {
		t.Fatalf("first call: %d %s", r1.StatusCode, b1)
	}
	var rows int
	if err := s.pool.QueryRow(r1.Request.Context(), `SELECT count(*) FROM idempotency_keys WHERE key_hash=$1`, KeyHash("sub", "k")).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a 5xx must not leave a stored key (rows=%d)", rows)
	}
	r2, b2 := do(`{"a":1}`)
	if r2.StatusCode != 201 || b2 != `{"ok":true}` || r2.Header.Get(HeaderReplayed) == "true" {
		t.Fatalf("retry must run the handler again: %d %s replayed=%q", r2.StatusCode, b2, r2.Header.Get(HeaderReplayed))
	}
	if calls.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", calls.Load())
	}
	r3, _ := do(`{"a":1}`)
	if r3.Header.Get(HeaderReplayed) != "true" || r3.StatusCode != 201 {
		t.Fatalf("the stored 201 must replay: %d", r3.StatusCode)
	}
	r4, b4 := do(`{"a":2}`)
	if r4.StatusCode != 422 || !strings.Contains(b4, "idempotency_key_reuse") {
		t.Fatalf("mismatch must be 422: %d %s", r4.StatusCode, b4)
	}
}

// 4xx are the client's own doing and stay replayable, so a retry cannot turn a rejected request
// into an accepted one without changing the key.
func TestMiddlewareStoresAndReplays4xx(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"conflict"}`))
	})
	srv := httptest.NewServer(Middleware(s, func(*http.Request) string { return "sub" })(handler))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/x", strings.NewReader(`{}`))
		req.Header.Set(HeaderKey, "k4")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 409 || (i == 1 && resp.Header.Get(HeaderReplayed) != "true") {
			t.Fatalf("call %d: %d replayed=%q", i, resp.StatusCode, resp.Header.Get(HeaderReplayed))
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", calls.Load())
	}
}

func TestMiddlewareStoresResultWhenClientDisconnects(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond) // effect committed downstream while the client gives up
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(Middleware(s, func(*http.Request) string { return "sub" })(handler))
	defer srv.Close()

	impatient := &http.Client{Timeout: 50 * time.Millisecond}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/x", strings.NewReader(`{"a":1}`))
	req.Header.Set(HeaderKey, "k-disconnect")
	if _, err := impatient.Do(req); err == nil {
		t.Fatal("expected client timeout")
	}
	time.Sleep(500 * time.Millisecond) // let the handler finish

	req, _ = http.NewRequest("POST", srv.URL+"/v1/x", strings.NewReader(`{"a":1}`))
	req.Header.Set(HeaderKey, "k-disconnect")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("handler ran %d times after client disconnect; retry must replay", calls.Load())
	}
	if resp.Header.Get(HeaderReplayed) != "true" || resp.StatusCode != 201 {
		t.Fatalf("retry must replay stored 201, got %d replayed=%q", resp.StatusCode, resp.Header.Get(HeaderReplayed))
	}
}
