package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/outbox"
	"github.com/acme/acme-card/pkg/webhook"
	"github.com/acme/acme-card/services/notifications/internal/rules"
)

const claimBatch = 20

type claimed struct {
	id, eventID, eventType, notificationType, account, customer, target, recipient, text string
	payload                                                                              []byte
	attempts                                                                             int
}

// DeliverOnce claims the due deliveries of one kind ("customer" channels or "webhook"), sends them
// concurrently and records the outcome. The claim bumps attempts and leases the row past the
// attempt timeout, so concurrent workers never send the same attempt twice.
func (s *Service) DeliverOnce(ctx context.Context, kind string, now time.Time) (int, error) {
	webhooks := kind == "webhook"
	lease := now.Add(s.cfg.WebhookTimeout + time.Minute)
	var batch []claimed
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE deliveries d SET attempts = attempts + 1, next_attempt_at = $3, updated_at = $1
			FROM (SELECT id FROM deliveries WHERE status = 'pending' AND next_attempt_at <= $1 AND (target LIKE 'webhook:%') = $2
			      ORDER BY next_attempt_at LIMIT $4 FOR UPDATE SKIP LOCKED) due
			WHERE d.id = due.id
			RETURNING d.id::text, d.event_id::text, d.event_type, coalesce(d.notification_type,''), coalesce(d.account_id,''), coalesce(d.customer_id,''),
				d.target, d.recipient, coalesce(d.text,''), d.payload::text, d.attempts`, now, webhooks, lease, claimBatch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c claimed
			var payload *string
			if err := rows.Scan(&c.id, &c.eventID, &c.eventType, &c.notificationType, &c.account, &c.customer, &c.target, &c.recipient, &c.text, &payload, &c.attempts); err != nil {
				return err
			}
			if payload != nil {
				c.payload = []byte(*payload)
			}
			batch = append(batch, c)
		}
		return rows.Err()
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(batch))
	for i := range batch {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := batch[i]
			var sendErr error
			final := false
			if strings.HasPrefix(c.target, "webhook:") {
				final, sendErr = s.sendWebhook(ctx, c, now)
			} else {
				sendErr = s.providers.Send(ctx, c.target, c.recipient, c.text)
			}
			errs[i] = s.finish(ctx, c, sendErr, final, now)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return len(batch), err
		}
	}
	return len(batch), nil
}

// finish records an attempt: delivered (plus the inbox row for customer channels), a retry after
// the backoff, or failed after the last attempt (or at once when final). Delivered and failed are
// audited in the same transaction.
func (s *Service) finish(ctx context.Context, c claimed, sendErr error, final bool, now time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if sendErr == nil {
			tag, err := tx.Exec(ctx, `UPDATE deliveries SET status='delivered', last_error=NULL, updated_at=$3 WHERE id=$1 AND attempts=$2 AND status='pending'`,
				c.id, c.attempts, now)
			if err != nil || tag.RowsAffected() == 0 {
				return err // the lease expired and another worker took the row
			}
			if !strings.HasPrefix(c.target, "webhook:") {
				if _, err := tx.Exec(ctx, `INSERT INTO inbox (account_id, customer_id, channel, type, text, delivery_id, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
					c.account, c.customer, c.target, c.notificationType, c.text, c.id, now); err != nil {
					return err
				}
			}
			return outbox.Enqueue(ctx, tx, s.auditEvent("notification.delivered", c, ""))
		}
		msg := sendErr.Error()
		if wait, retry := rules.Backoff(c.attempts); retry && !final {
			_, err := tx.Exec(ctx, `UPDATE deliveries SET next_attempt_at=$3, last_error=$4, updated_at=$5 WHERE id=$1 AND attempts=$2 AND status='pending'`,
				c.id, c.attempts, now.Add(wait), msg, now)
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE deliveries SET status='failed', last_error=$3, updated_at=$4 WHERE id=$1 AND attempts=$2 AND status='pending'`,
			c.id, c.attempts, msg, now)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return outbox.Enqueue(ctx, tx, s.auditEvent("notification.failed", c, msg))
	})
}

func mask(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

func (s *Service) auditEvent(typ string, c claimed, lastErr string) events.Event {
	ev := events.New(typ, "notification", c.id)
	ev.Actor = "notifications"
	after := map[string]any{"event_id": c.eventID, "type": c.notificationType, "target": c.target, "attempts": c.attempts,
		"recipient_masked": mask(c.recipient), "account_id": c.account}
	if strings.HasPrefix(c.target, "webhook:") {
		after["type"] = c.eventType
	}
	if lastErr != "" {
		after["last_error"] = lastErr
	}
	ev.After = mustJSON(after)
	return ev
}

// sendWebhook posts the event to the partner, signed with its secret. final reports an error that
// retrying cannot fix (the subscription was disabled).
func (s *Service) sendWebhook(ctx context.Context, c claimed, now time.Time) (final bool, err error) {
	var url, sealed string
	var active bool
	if err := s.pool.QueryRow(ctx, `SELECT url, secret_sealed, active FROM subscriptions WHERE id::text = $1`, strings.TrimPrefix(c.target, "webhook:")).
		Scan(&url, &sealed, &active); err != nil {
		return err == pgx.ErrNoRows, fmt.Errorf("subscription: %w", err)
	}
	if !active {
		return true, fmt.Errorf("subscription disabled")
	}
	secret, err := s.box.Open(sealed)
	if err != nil {
		return true, fmt.Errorf("subscription secret: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(c.payload))
	if err != nil {
		return true, err
	}
	ts := now
	if real := time.Now(); real.After(ts) {
		ts = real // the signature timestamp is when it is sent (partners reject stale ones)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Acme-Event-Id", c.eventID)
	req.Header.Set("X-Acme-Delivery-Id", c.id)
	req.Header.Set(webhook.HeaderTimestamp, fmt.Sprint(ts.Unix()))
	req.Header.Set(webhook.HeaderSignature, signatureHeader(secret, c.payload, ts))
	res, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode/100 != 2 {
		return false, fmt.Errorf("partner answered %d", res.StatusCode)
	}
	return false, nil
}

// RunDelivery runs the customer and webhook loops independently (a slow partner never holds up
// customer channels) until ctx is cancelled.
func (s *Service) RunDelivery(ctx context.Context, every time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	loop := func(kind string) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			for {
				n, err := s.DeliverOnce(ctx, kind, s.now())
				if err != nil && ctx.Err() == nil {
					log.Error("notifications: delivery failed", "kind", kind, "err", err)
				}
				if n < claimBatch || err != nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}
	go loop("webhook")
	loop("customer")
}
