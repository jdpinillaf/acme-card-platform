//go:build integration

package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/acme/acme-card/pkg/webhook"
)

func (f *fixture) insertDelivery(t *testing.T, target, recipient string, payload []byte) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	var p any
	if payload != nil {
		p = payload
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO deliveries (id, event_id, event_type, notification_type, account_id, customer_id, target, recipient, payload, text, next_attempt_at)
		VALUES ($1, $2, 'authorization.approved', CASE WHEN $3 LIKE 'webhook:%' THEN NULL ELSE 'purchase_approved' END, 'acc-1', 'cust-acc-1', $3, $4, $5,
		CASE WHEN $3 LIKE 'webhook:%' THEN NULL ELSE 'Purchase approved: 12.34 USD at Corner Bookstore.' END, $6)`,
		id, uuid.NewString(), target, recipient, p, f.now); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) status(t *testing.T, id string) (string, int) {
	t.Helper()
	var st string
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT status, attempts FROM deliveries WHERE id=$1`, id).Scan(&st, &n); err != nil {
		t.Fatal(err)
	}
	return st, n
}

func TestRetryThenDelivered(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.providers.fail["sms"] = 2
	id := f.insertDelivery(t, "sms", "+1-555-abcd", nil)
	at := f.now
	for _, step := range []time.Duration{0, time.Minute, 5 * time.Minute} {
		at = at.Add(step)
		if _, err := f.svc.DeliverOnce(ctx, "customer", at); err != nil {
			t.Fatal(err)
		}
	}
	if st, n := f.status(t, id); st != "delivered" || n != 3 {
		t.Fatalf("status %s attempts %d", st, n)
	}
	if f.count(t, `SELECT count(*) FROM inbox WHERE delivery_id=$1 AND account_id='acc-1' AND channel='sms'`, id) != 1 {
		t.Fatal("no inbox row")
	}
	if f.count(t, `SELECT count(*) FROM outbox WHERE payload->>'type'='notification.delivered' AND (payload->'after'->>'attempts')::int = 3
		AND payload->'after'->>'recipient_masked' = '****abcd'`) != 1 {
		t.Fatal("notification.delivered not audited")
	}
}

func TestEarlyAttemptWaitsForBackoff(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.providers.fail["push"] = 1
	id := f.insertDelivery(t, "push", "push:cust-acc-1", nil)
	_, _ = f.svc.DeliverOnce(ctx, "customer", f.now)
	_, _ = f.svc.DeliverOnce(ctx, "customer", f.now.Add(30*time.Second)) // before the 1-minute backoff
	if st, n := f.status(t, id); st != "pending" || n != 1 {
		t.Fatalf("retried early: %s %d", st, n)
	}
}

func TestSixFailuresThenFailedAudited(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.providers.fail["email"] = 99
	id := f.insertDelivery(t, "email", "cust-acc-1@example.test", nil)
	at := f.now
	for _, step := range []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour} {
		at = at.Add(step)
		if _, err := f.svc.DeliverOnce(ctx, "customer", at); err != nil {
			t.Fatal(err)
		}
	}
	if st, n := f.status(t, id); st != "failed" || n != 6 {
		t.Fatalf("status %s attempts %d", st, n)
	}
	if f.count(t, `SELECT count(*) FROM outbox WHERE payload->>'type'='notification.failed' AND payload->'after'->>'last_error' LIKE '%email provider down%'`) != 1 {
		t.Fatal("notification.failed not audited")
	}
	if f.count(t, `SELECT count(*) FROM inbox`) != 0 {
		t.Fatal("failed delivery reached the inbox")
	}
}

func TestConcurrentWorkersSendOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		f.insertDelivery(t, "push", "push:cust-acc-1", nil)
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f.count(t, `SELECT count(*) FROM deliveries WHERE status='pending'`) > 0 {
				if _, err := f.svc.DeliverOnce(ctx, "customer", f.now); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(f.providers.sent) != 50 || f.count(t, `SELECT count(*) FROM inbox`) != 50 {
		t.Fatalf("sends=%d", len(f.providers.sent))
	}
}

func (f *fixture) subscription(t *testing.T, url, secret string, active bool) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	sealed, err := f.svc.box.Seal([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO subscriptions (id, name, url, event_types, secret_sealed, active) VALUES ($1,'partner',$2,'{authorization.approved}',$3,$4)`,
		id, url, sealed, active); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWebhookSignedAndVerifiable(t *testing.T) {
	f := setup(t)
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig := strings.TrimPrefix(r.Header.Get("X-Acme-Signature"), "v1=")
		err := webhook.Verify([]byte("s3cret"), body, r.Header.Get("X-Acme-Timestamp"), sig, time.Now(), 5*time.Minute)
		got.Store([]string{r.Header.Get("X-Acme-Event-Id"), r.Header.Get("X-Acme-Delivery-Id"), string(body), errString(err)})
		if err != nil {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	sub := f.subscription(t, srv.URL+"/hook", "s3cret", true)
	id := f.insertDelivery(t, "webhook:"+sub, srv.URL+"/hook", []byte(`{"id":"e1","type":"authorization.approved"}`))
	if _, err := f.svc.DeliverOnce(context.Background(), "webhook", f.now); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.status(t, id); st != "delivered" {
		t.Fatalf("status %s, partner saw %v", st, got.Load())
	}
	seen := got.Load().([]string)
	if seen[1] != id || seen[0] == "" || !strings.Contains(seen[2], `"e1"`) || seen[3] != "" {
		t.Fatalf("partner saw %v", seen)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestDisabledSubscriptionStopsPending(t *testing.T) {
	f := setup(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer srv.Close()
	sub := f.subscription(t, srv.URL, "s", false)
	id := f.insertDelivery(t, "webhook:"+sub, srv.URL, []byte(`{}`))
	if _, err := f.svc.DeliverOnce(context.Background(), "webhook", f.now); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.status(t, id); st != "failed" || hits.Load() != 0 {
		t.Fatalf("disabled partner: %s hits=%d", st, hits.Load())
	}
}

func TestSlowPartnerDoesNotDelayCustomers(t *testing.T) {
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	sub := f.subscription(t, srv.URL, "s", true)
	for i := 0; i < 20; i++ {
		f.insertDelivery(t, "webhook:"+sub, srv.URL, []byte(`{}`))
	}
	push := f.insertDelivery(t, "push", "push:cust-acc-1", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.svc.RunDelivery(ctx, 50*time.Millisecond, nil)
	deadline := time.Now().Add(time.Second)
	for {
		if st, _ := f.status(t, push); st == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("customer push delayed by a slow partner")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
