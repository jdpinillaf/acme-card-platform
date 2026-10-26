# Sprint 7 — Notifications

Status: builder-checked · 2026-10-25 · builds on Sprints 1–6 (ADR-001…021)

## 1. Intent

Customers need to know what happens to their card. Partner systems need a feed of what happens across all accounts. Today every state change already becomes an event (outbox → SNS → SQS), but it only reaches the audit log.

This sprint turns the events into:
- **customer notifications**, by push, SMS and email (all simulated);
- **partner webhooks**, signed with HMAC.

Both get reliable delivery: retries with growing waits, then a final `failed`.

## 2. Goals, non-goals, anti-goals

**Goals**
- Nine customer notification types, delivered on three channels. Customers choose channels per type, and every channel is on by default.
- Three types are mandatory and cannot be fully silenced: past due, block, suspected fraud.
- Partner webhooks. An internal operator registers a partner with a URL and the event types it subscribes to. The partner receives those events for every account, signed with a per-partner secret.
- Delivery retries with growing waits. After the last attempt the delivery is `failed`.
- The simulator shows each customer notification as a simple popup at the bottom of the page.
- Every delivered or failed notification is itself an audit event.

**Non-goals**
- Real providers: no APNs/FCM, Twilio or SES. Providers are simulated adapters behind an interface.
- Manual resend by support. Automatic pausing of failing partners.
- Customer-registered webhooks. A partner self-service portal.
- Templates, i18n and rich email. Texts are short and in English, like the whole project.
- Real AWS deployment (Terraform described only).

**Anti-goals**
- A notification sent twice for one event and channel (SQS redelivers).
- A mandatory notification that a customer could turn off completely.
- A webhook that a partner cannot authenticate, or that it can replay later.
- Secrets in clear: partner secrets stored unencrypted, or full account numbers / tokens in any payload.
- A slow or dead partner URL that delays customer notifications or other partners.
- A partner URL that lets the service call internal hosts (SSRF).

## 3. Decisions made by the user

| # | Topic | Decision |
|---|---|---|
| 1 | Channels | Push, SMS and email, all three; simulated for now |
| 2 | Customer events | Card activated; purchase approved; purchase declined; card frozen/unfrozen; payment received; past due + late fee; card blocked for past due; close payout sent/settled/failed; suspected fraud (several declines in a row) |
| 3 | Preferences | The customer chooses channels per notification type; everything is on by default |
| 4 | Mandatory | Past due, block and fraud cannot be fully disabled (at least one channel stays on) |
| 5 | Partner webhooks | Yes, in this sprint, HMAC-signed |
| 6 | Who registers | An internal operator registers partners (URL, secret, event types); events of every account |
| 7 | Failures | Incremental retries; after the last one the delivery is `failed` |
| 8 | Simulator | A simple popup at the bottom of the app |
| 9 | Assumptions accepted | The service consumes the same SNS→SQS events; English texts; fraud = 3 declines in 10 minutes; every delivery and failure audited |
| 10 | Fraud declines (builder check) | Only declines that suggest someone else is using the card count: `insufficient_credit`, `over_limit`, `purchase_limit_exceeded`, `velocity_limit_exceeded`. The 10-minute window is real time (`occurred_at`), not the account's dev clock |
| 11 | Card blocked (builder check) | `card_blocked` (mandatory) fires on: past due at 15 days (`account.delinquency_changed` → `past_due`), charge-off at 60 days (`account.charged_off`), and a restriction (`card.restricted`, e.g. `payment_returned`). The text names the reason |

## 4. Design

### N-1 Service and intake
- `services/notifications` becomes a real service:
  - own Postgres schema `notifications`;
  - gRPC on `:9006`;
  - its own SQS queue `acme-card-notifications` (plus DLQ), subscribed to the domain topic with raw delivery, exactly like the audit queue.
- Each event is processed once:
  - a row in `processed_events(event_id)` is inserted in the same transaction that creates its deliveries;
  - a redelivered event inserts nothing.
- The service looks up the account's `customer_id` through accounts gRPC `GetAccount`.
  - If that lookup fails, the SQS message is not deleted, so it is retried.

### N-2 Notification types (customer)

| Type | Source event | Mandatory |
|---|---|---|
| `card_activated` | `card.activated` | no |
| `purchase_approved` | `authorization.approved` | no |
| `purchase_declined` | `authorization.declined` (text names the decline reason) | no |
| `card_frozen` / `card_unfrozen` (one preference: `card_freeze`) | `card.frozen` / `card.unfrozen` | no |
| `payment_received` | `payment.settled` | no |
| `late_fee` | `fee.charged` (late fee reference) | **yes** |
| `card_blocked` | `account.delinquency_changed` with after stage `past_due` (from another stage); `account.charged_off`; `card.restricted` | **yes** |
| `payout_update` | `payout.requested` / `payout.settled` / `payout.failed` | no |
| `suspected_fraud` | 3 `authorization.declined` with reason `insufficient_credit`, `over_limit`, `purchase_limit_exceeded` or `velocity_limit_exceeded` on one account within 10 minutes of real time; then at most one per account per hour | **yes** |

- **Text.** One English line per type with the amount (formatted minor units + currency), the merchant name when there is one, and the last 4 digits of the payout destination when relevant.
- **What text never contains:** a full account number, a token or a ciphertext.

### N-3 Preferences
- Table `preferences(customer_id, type, push, sms, email)`. A missing row means all three channels are on.
- Routes (customer, the account must be in the token's `authorized_accounts`):
  - `GET /v1/accounts/{id}/notification-preferences` lists every type with its channels and a `mandatory` flag.
  - `PUT /v1/accounts/{id}/notification-preferences` with `{type, push, sms, email}` is strict JSON.
  - All three off on a mandatory type → `422 mandatory_notification`.
- Preferences belong to the customer, so they are shared by all of that customer's accounts. They are read at delivery-creation time.

### N-4 Deliveries and retries
- One `deliveries` row per (event, channel), or per (event, partner subscription).
  - Columns: `status pending|delivered|failed`, `attempts`, `next_attempt_at`, `last_error`.
  - Unique key `(event_id, target)` makes duplicates impossible.
- Workers claim due rows with `FOR UPDATE SKIP LOCKED`, so several replicas never work the same row.
- Each attempt has a timeout: 5 s for webhooks.
- **Backoff after the 1st failure:** 1 min, 5 min, 30 min, 2 h, 6 h. That is 6 attempts in total, about 8.6 h. After the 6th failure the delivery is `failed`.
- **Isolation.** Each delivery is independent. A dead partner only fills its own rows. Workers process webhooks and customer channels from separate claim loops, so slow partners cannot starve customer notifications.
- **Audit events** go through the notifications outbox (source `notifications`, added to the audit producers):
  - `notification.delivered` and `notification.failed`;
  - payload: type, channel or partner id, attempts, and the masked recipient;
  - never the message body with personal data beyond what the text holds.

### N-5 Simulated providers
- A `Provider` interface: `Send(ctx, channel, recipient, text) error`.
- The dev adapter records each message in `inbox(customer_id, channel, text, created_at)`. This table is what the simulator reads.
- Recipients are simulated from the customer id: a `push:<customer>` device, `<customer>@example.test` and `+1-555-<hash4>`.
- Fault injection for tests and the simulator: `NOTIFY_FAIL_CHANNELS=sms`, dev only, makes that channel's provider fail.
- Customer route `GET /v1/accounts/{id}/notifications?after=<id>` returns the newest inbox items of **that account** (the inbox has `account_id`), newest first, at most 50. The simulator polls it.

### N-6 Partner webhooks
- Operator routes. Role `support` stands in for "internal operator"; there is no new role.
  - `POST /v1/support/webhook-subscriptions` with `{name, url, event_types[]}` returns 201 with the id and the secret. The secret is shown once.
  - `GET /v1/support/webhook-subscriptions` lists subscriptions without secrets.
  - `DELETE /v1/support/webhook-subscriptions/{id}` disables the subscription. It is not deleted, for audit.
  - `GET /v1/support/webhook-subscriptions/{id}/deliveries` lists the latest 50 deliveries with status and attempts.
- **Event types a partner may subscribe to:** the public domain events.
  - `account.*`, `card.*`, `authorization.*`, `payment.*`;
  - `payout.requested|settled|failed`, `statement.generated`, `fee.charged`, `interest.charged`, `charge.waived`;
  - `collateral.confirmed`, `collateral.deposit_failed`.
  - Never `http.request`, `audit.*`, `snapshot.*`, `ledger.*` or `payout.instruction_read`.
- **Request.** `POST <url>`, body = the event JSON, already masked by its producer.
  - Headers:
    - `X-Acme-Event-Id`;
    - `X-Acme-Delivery-Id`;
    - `X-Acme-Timestamp` (Unix seconds);
    - `X-Acme-Signature: v1=<hex hmac-sha256(secret, timestamp + "." + body)>`.
  - Partners reject requests older than 5 minutes and dedupe on the event id. This is documented in the README.
  - A 2xx is delivered; anything else, or a timeout, is retried.
- **Secrets.** Generated server-side (32 random bytes). Stored AES-256-GCM encrypted with `NOTIFY_SECRET_KEY` (`pkg/secretbox`, KMS in AWS).
- **URL rules (SSRF):**
  - `https` only, except hosts listed in `NOTIFY_ALLOWED_HTTP_HOSTS`, which is dev only (for example `web` for the simulator's partner endpoint);
  - after DNS resolution, refuse loopback, link-local, private and metadata addresses unless the host is in that dev list;
  - no redirects followed.

### N-7 Simulator
- **Popup.** While an account is loaded, the page polls `GET /v1/accounts/{id}/notifications` every 3 seconds. Each new item shows as a small popup at the bottom (channel icon + text) for 6 seconds.
- **Coverage.** Every new route needs a control, as the coverage test requires.
  - Panel 10:
    - preference toggles;
    - a "register demo partner" button (support token), pointing at the simulator's own `/api/partner/webhook`, which verifies the HMAC and keeps the last 20 received events;
    - a partner deliveries list;
    - disable partner.
  - Minimal UI; the popup is the main feature.

### N-8 Testing gates
- **Unit tests:**
  - text rendering per type, with no secrets;
  - backoff schedule;
  - HMAC signing against a fixed vector;
  - URL validation;
  - the mandatory rule;
  - the fraud window.
- **Integration tests:**
  - once per event and channel under SQS redelivery and concurrent workers;
  - retry → delivered;
  - 6 failures → `failed` plus a `notification.failed` audit event;
  - a mandatory type with all channels off is refused;
  - a disabled partner receives nothing new;
  - a slow partner does not delay customer channels.
- **End to end:** a customer journey produces the expected inbox items and partner deliveries, verified with HMAC. `TestCustomerJourney` stays green.
- **Load test** (1000 VUs, isolated): no duplicate deliveries; every pending delivery drains or reaches `failed`; p95 stays within the gate.

## 4b. Builder-check rulings (my calls; the user decided #10 and #11)
- **Secret shown once, never stored in clear.** The gateway's idempotency store keeps response bodies in clear. For `POST /v1/support/webhook-subscriptions` it stores the response with `secret` replaced by `"[shown once]"`: the first response carries the secret, a replay does not. This is a per-route option in `pkg/idempotency`.
- **Charge payloads unchanged.** `fee.charged` stays the raw `chargeRow` (`AccountID`, `Amount`, `Reference`; no currency). Notifications recognises a late fee by `Reference` starting `late_fee:` and takes the currency from `GetAccount`. Partners receive it as is (documented).
- **Payouts.** Every `payout.requested` / `settled` / `failed` notifies, for every payout kind: retries and the extra payouts after a late refund or credit balance are real money movements.
- **`GetAccount` only when needed.** `card.*` and `account.*` snapshots carry `customer_id`.
- **Signature header** `X-Acme-Signature: v1=<hex>` wraps `pkg/webhook.Sign`; the shared package and the inbound webhooks stay as they are. `X-Acme-Event-Id` and `X-Acme-Delivery-Id` are new.
- **The DLQ really receives.** The notifications queue gets a redrive policy (5 receives → DLQ); the audit queue is given the same fix.
- **Audit producers.** `notifications` is added to the three hard-coded producer lists (audit main, auditctl, audittest).
- **Load gate.** p95 ≤ 1.5 s as in earlier sprints, plus every delivery resolved (delivered or failed) within 10 minutes after the load.

## 5. Left to the builder
- Exact message wording.
- Table indexes.
- Polling interval of the workers (≤ 1 s).
- Panel layout.
