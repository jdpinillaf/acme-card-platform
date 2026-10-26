// Package notify is the notifications service: event intake, delivery, and the API for
// preferences, inbox and partner subscriptions (Sprint 7, ADR-022).
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/pkg/secretbox"
	"github.com/acme/acme-card/services/notifications/internal/rules"
)

// Providers sends a customer notification on one channel (simulated in dev).
type Providers interface {
	Send(ctx context.Context, channel, recipient, text string) error
}

// Config tunes the service.
type Config struct {
	AllowedHTTPHosts []string      // dev only: partner hosts allowed over http and on private addresses
	WebhookTimeout   time.Duration // per attempt; 0 = 5s
}

// Service implements notificationsv1.NotificationsServiceServer plus intake and delivery.
type Service struct {
	notificationsv1.UnimplementedNotificationsServiceServer
	pool      *pgxpool.Pool
	accounts  accountsv1.AccountsServiceClient
	providers Providers
	box       *secretbox.Box
	cfg       Config
	client    *http.Client
	lookup    lookupFunc // partner host resolution (registration check)
	now       func() time.Time
}

// NewService builds the service; box seals partner secrets.
func NewService(pool *pgxpool.Pool, accounts accountsv1.AccountsServiceClient, providers Providers, box *secretbox.Box, cfg Config) *Service {
	if cfg.WebhookTimeout == 0 {
		cfg.WebhookTimeout = 5 * time.Second
	}
	return &Service{pool: pool, accounts: accounts, providers: providers, box: box, cfg: cfg,
		client: NewWebhookClient(cfg.AllowedHTTPHosts, cfg.WebhookTimeout), lookup: defaultLookup, now: func() time.Time { return time.Now().UTC() }}
}

// recipient is the simulated address of a customer on a channel.
func recipient(channel, customer string) string {
	switch channel {
	case "email":
		return customer + "@example.test"
	case "sms":
		sum := sha256.Sum256([]byte(customer))
		return "+1-555-" + hex.EncodeToString(sum[:2])
	}
	return "push:" + customer
}

// fraudEventID keys the fraud alert's deliveries apart from the decline's own notification.
func fraudEventID(eventID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("suspected_fraud:"+eventID)).String()
}

type delivery struct {
	eventID, notificationType, account, customer, target, recipient, text string
	payload                                                               []byte
}

// Process turns one event into its deliveries, once: the processed_events row is written in the
// same transaction, so an SQS redelivery (or a concurrent copy) creates nothing. An error leaves
// the message on the queue for a retry.
func (s *Service) Process(ctx context.Context, ev events.Event) error {
	var seen bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id=$1)`, ev.ID).Scan(&seen); err != nil || seen {
		return err
	}
	notice, isNotice := rules.Classify(ev)
	fraud := rules.CountsTowardFraud(ev)
	account := notice.AccountID
	if fraud {
		account = mustAccount(ev)
	}
	customer, cur := notice.CustomerID, ""
	if (isNotice || fraud) && account != "" && (customer == "" || ev.Type == "fee.charged") {
		acc, err := s.accounts.GetAccount(ctx, &accountsv1.GetAccountRequest{AccountId: account})
		if err != nil {
			return fmt.Errorf("notifications: account lookup: %w", err)
		}
		customer, cur = acc.Account.GetCustomerId(), acc.Account.GetCurrency()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, ev.ID)
		if err != nil || tag.RowsAffected() == 0 {
			return err // a concurrent copy won
		}
		var out []delivery
		if isNotice && customer != "" {
			d, err := s.customerDeliveries(ctx, tx, ev.ID, notice, customer, cur)
			if err != nil {
				return err
			}
			out = append(out, d...)
		}
		if fraud && customer != "" {
			alert, err := s.fraudAlert(ctx, tx, account, ev.OccurredAt)
			if err != nil {
				return err
			}
			if alert {
				d, err := s.customerDeliveries(ctx, tx, fraudEventID(ev.ID), rules.FraudNotice(account), customer, cur)
				if err != nil {
					return err
				}
				out = append(out, d...)
			}
		}
		if rules.PartnerEventAllowed(ev.Type) {
			rows, err := tx.Query(ctx, `SELECT id::text, url FROM subscriptions WHERE active AND $1 = ANY(event_types)`, ev.Type)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, url string
				if err := rows.Scan(&id, &url); err != nil {
					rows.Close()
					return err
				}
				out = append(out, delivery{eventID: ev.ID, account: account, customer: customer, target: "webhook:" + id, recipient: url, payload: body})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, d := range out {
			var payload any
			if d.payload != nil {
				payload = d.payload
			}
			if _, err := tx.Exec(ctx, `INSERT INTO deliveries (id, event_id, event_type, notification_type, account_id, customer_id, target, recipient, payload, text)
				VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), NULLIF($6,''), $7, $8, $9, NULLIF($10,'')) ON CONFLICT (event_id, target) DO NOTHING`,
				uuid.Must(uuid.NewV7()), d.eventID, ev.Type, d.notificationType, d.account, d.customer, d.target, d.recipient, payload, d.text); err != nil {
				return err
			}
		}
		return nil
	})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// mustAccount is the account an authorization event belongs to.
func mustAccount(ev events.Event) string {
	var p struct {
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(ev.After, &p)
	return p.AccountID
}

// customerDeliveries applies the customer's preferences (no row = all on; a mandatory type
// keeps push even if a stored row has every channel off).
func (s *Service) customerDeliveries(ctx context.Context, tx pgx.Tx, eventID string, n rules.Notice, customer, cur string) ([]delivery, error) {
	on := map[string]bool{"push": true, "sms": true, "email": true}
	var push, sms, email bool
	err := tx.QueryRow(ctx, `SELECT push, sms, email FROM preferences WHERE customer_id=$1 AND type=$2`, customer, n.Type).Scan(&push, &sms, &email)
	switch {
	case err == nil:
		on = map[string]bool{"push": push, "sms": sms, "email": email}
		if n.Mandatory && !push && !sms && !email {
			on["push"] = true
		}
	case err != pgx.ErrNoRows:
		return nil, err
	}
	text := n.Text(cur)
	var out []delivery
	for _, ch := range rules.Channels {
		if on[ch] {
			out = append(out, delivery{eventID: eventID, notificationType: n.Type, account: n.AccountID, customer: customer, target: ch,
				recipient: recipient(ch, customer), text: text})
		}
	}
	return out, nil
}

// fraudAlert records a counting decline and reports whether it is the one that raises the alert:
// the 3rd within the window (real time, occurred_at), at most once per cooldown per account.
func (s *Service) fraudAlert(ctx context.Context, tx pgx.Tx, account string, at time.Time) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('fraud:'||$1))`, account); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO declines (account_id, at) VALUES ($1, $2)`, account, at); err != nil {
		return false, err
	}
	var recent, alerted int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM declines WHERE account_id=$1 AND at > $2 AND at <= $3),
		(SELECT count(*) FROM fraud_alerts WHERE account_id=$1 AND at > $4)`, account, at.Add(-rules.FraudWindow), at, at.Add(-rules.FraudCooldown)).
		Scan(&recent, &alerted); err != nil {
		return false, err
	}
	if recent < rules.FraudDeclines || alerted > 0 {
		return false, nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO fraud_alerts (account_id, at) VALUES ($1, $2)`, account, at)
	return err == nil, err
}
