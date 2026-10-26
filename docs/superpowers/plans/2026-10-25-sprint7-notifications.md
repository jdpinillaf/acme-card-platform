# Sprint 7 — Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Customer notifications (push, SMS and email, simulated) and HMAC-signed partner webhooks, fed by the existing domain events. Delivery is reliable, and each notification is delivered once per channel and per subscription.

**Architecture:**
- **New `notifications` service:**
  - Postgres schema `notifications`, gRPC on `:9006`.
  - Its own SQS queue on the domain SNS topic.
- **Intake:** turns each event into delivery rows once (`processed_events` dedupe in the same transaction).
- **Delivery:** two independent claim loops, one for customer channels and one for webhooks (`FOR UPDATE SKIP LOCKED`), with backoff. Results become audit events through the service's own outbox.
- **Gateway:** exposes customer preferences and inbox, plus support-only partner subscriptions.
- **Simulator:** polls the inbox and shows a popup.

**Tech Stack:** Go 1.26, gRPC/buf, pgx v5, aws-sdk-go-v2 SQS, `pkg/secretbox`, `pkg/webhook`, testcontainers (Postgres + LocalStack), k6, Next.js.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint7-notifications-design.md` (§3 decisions, §4 N-1…N-8, §4b builder-check rulings override §4).

## Global Constraints

**Notification types and their source events**

| Preference type | Source event | Account id from | Mandatory |
|---|---|---|---|
| `card_activated` | `card.activated` | aggregate_id | no |
| `purchase_approved` | `authorization.approved` | `after.account_id` | no |
| `purchase_declined` | `authorization.declined` | `after.account_id` | no |
| `card_freeze` | `card.frozen` and `card.unfrozen` | aggregate_id | no |
| `payment_received` | `payment.settled` | `after.account_id` | no |
| `late_fee` | `fee.charged` | `after.AccountID` (charge payload keys are Go field names: `AccountID`, `Amount`, `Reference`) | **yes** |
| `card_blocked` | `account.delinquency_changed` whose `after.delinquency` is `past_due` and `before.delinquency` is not; `account.charged_off`; `card.restricted` (text names `after.restricted_reason`) | aggregate_id | **yes** |
| `payout_update` | `payout.requested`, `payout.settled` and `payout.failed`, every payout kind and attempt | `after.account_id` | no |
| `suspected_fraud` | 3rd `authorization.declined` with `decline_reason` in {`insufficient_credit`, `over_limit`, `purchase_limit_exceeded`, `velocity_limit_exceeded`} of an account within 10 min of real time (`occurred_at`); then at most 1 per account per hour | `after.account_id` | **yes** |

**Texts**
- English, one line each.
- Amounts are formatted minor units plus the currency. The currency comes from accounts `GetAccount` when the payload has none (`fee.charged`).
- They include the merchant name when the event has one and `****<last4>` for payouts.
- They never contain an account number, a token or a ciphertext.

**Channels and recipients**
- Channels are `push`, `sms` and `email`.
- Simulated recipients:
  - push: `push:<customer_id>`;
  - email: `<customer_id>@example.test`;
  - SMS: `+1-555-<first 4 hex of sha256(customer_id)>`.
- The provider interface is `Send(ctx, channel, recipient, text string) error`.

**Preferences**
- Per customer and type. No row means every channel is on.
- A mandatory type with every channel off is refused with `422 mandatory_notification`.

**Deliveries**
- `target` is `push|sms|email` or `webhook:<subscription_id>`, unique per `(event_id, target)`.
- Backoff after failure n is `[1m, 5m, 30m, 2h, 6h]`. The 6th failure makes the delivery `failed`.
- Webhook timeout 5 s, no redirects followed. Only a 2xx counts as delivered.

**Webhooks**
- `POST url`. The body is the event JSON exactly as received.
- Headers:
  - `X-Acme-Event-Id`;
  - `X-Acme-Delivery-Id`;
  - `X-Acme-Timestamp` and `X-Acme-Signature: v1=<hex>`, where the hex is `webhook.Sign(secret, body, now)` (HMAC over `ts + "." + body`). Notifications adds the `v1=` prefix; `pkg/webhook` and the inbound webhooks stay unchanged.
- Secret: 32 random bytes, hex encoded. It is returned once and stored with `secretbox` under `NOTIFY_SECRET_KEY` (base64, 32 bytes; dev key in `.env.dev`).
- URLs must be `https`. `http` is allowed only for hosts in `NOTIFY_ALLOWED_HTTP_HOSTS`, which is dev only. Hosts that resolve to loopback, private, link-local or metadata addresses are refused unless they are in that list; the check runs at dial time.

**Partner-subscribable events**
- Allowed:
  - `account.*`, `card.*`, `authorization.*`, `payment.*`;
  - `payout.requested`, `payout.settled`, `payout.failed`;
  - `statement.generated`, `fee.charged`, `interest.charged`, `charge.waived`;
  - `collateral.confirmed`, `collateral.deposit_failed`.
- Anything else is `400 invalid_event_type`, including `payout.instruction_read`.

**Audit events**
- `notification.delivered` and `notification.failed`, aggregate `notification`, sent through the notifications outbox. The relay source is `"notifications"`, added to the audit `ProducerSchemas` and the e2e relay map.
- `after` = `{event_id, type, target, attempts, recipient_masked, last_error}`. `recipient_masked` keeps the last 4 characters.

**Secret never replayed or stored in clear**
- `pkg/idempotency` gains a per-route response redactor. The subscription-create route stores its 201 body with `secret` replaced by `"[shown once]"`.

**Lookups**
- `card.*` and `account.*` payloads carry `customer_id`. `GetAccount` is called only for authorization, payment, payout and charge events, and for currency on `fee.charged`.

**Config**
- `NOTIFICATIONS_SQS_QUEUE_URL`, `NOTIFICATIONS_GRPC_ADDR` (default `localhost:9006`), `NOTIFY_SECRET_KEY`.
- Dev only, refused unless `APP_ENV=dev`: `NOTIFY_ALLOWED_HTTP_HOSTS` and `NOTIFY_FAIL_CHANNELS`.

**Compatibility**
- Do not rename events or change any payload.
- `TestCustomerJourney` stays green.
- The HTTP audit middleware keeps skipping GETs.
- Every new gateway route gets an entry in `web/lib/ui-actions.json` and a control (coverage test).

**Process**
- TDD. Explicit `git add` paths.
- Commit trailer:
  - `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`
  - `Claude-Session: https://claude.ai/code/session_015vi3uVt9DP3bm1yjPQhtFS`
- Migrations: notifications `000001`+. ADR-022.

## Review Focus
1. **SQS redelivery during a crash between "deliveries created" and "message deleted".** Expect no second set of deliveries (`processed_events` in the same transaction). Test in Task 3.
2. **Two delivery workers racing on one due row.** Expect exactly one `Send` per attempt (`SKIP LOCKED`, attempt counter bumped in the claim). Test in Task 4.
3. **A partner URL that hangs or answers 500 forever.** Expect customer channels to deliver on time and the partner row to end `failed` after 6 attempts. Test in Task 4.
4. **A partner URL pointing at `169.254.169.254`, `localhost` or a host that resolves to `10.x`.** Expect it refused at registration and again at dial time (DNS rebinding). Test in Task 4.
5. **A customer turning every channel off for `late_fee`.** Expect 422 and the preference unchanged. Test in Task 5.

---

### Task 1: Contracts, schema, service skeleton

**Files:**
- `proto/acme/notifications/v1/notifications.proto`
- `services/notifications/{cmd/main.go, migrations/000001_init.{up,down}.sql, migrations/migrations.go}`
- `pkg/config`
- `deploy/docker-compose.yml`
- `deploy/localstack/init-aws.sh`
- `deploy/postgres-init.sql`
- `pkg/testutil/localstack.go` (`AddQueue`)
- `.env.dev` / `.env.example`
- Remove `services/notifications/README.md` placeholder text.

**Interfaces:**
- Produces `NotificationsService` RPCs:
  - `GetPreferences{customer_id}` → `{preferences[{type, push, sms, email, mandatory}]}`;
  - `SetPreference{customer_id, type, push, sms, email, meta}` → `Preference`;
  - `ListInbox{customer_id, after_id, limit}` → `{items[{id, channel, type, text, created_at}]}`;
  - `CreateSubscription{name, url, event_types[], meta}` → `{subscription, secret}`;
  - `ListSubscriptions{}`;
  - `DisableSubscription{id, meta}`;
  - `ListDeliveries{subscription_id, limit}` → `{deliveries[{id, event_id, event_type, status, attempts, last_error, next_attempt_at, updated_at}]}`.
- Produces the tables:
  - `processed_events(event_id pk, processed_at)`;
  - `preferences(customer_id, type, push, sms, email, updated_at; pk customer_id+type)`;
  - `subscriptions(id, name, url, event_types text[], secret_sealed, active, created_at, disabled_at)`;
  - `deliveries(id, event_id, event_type, notification_type, account_id, customer_id, target, recipient, payload jsonb, text, status, attempts, next_attempt_at, last_error, created_at, updated_at; unique(event_id,target); index (status,next_attempt_at) WHERE status='pending')`;
  - `inbox(id bigserial, account_id, customer_id, channel, type, text, delivery_id, created_at)`, read per account;
  - `declines(account_id, at)`;
  - `fraud_alerts(account_id, at)`;
  - `outbox` and `outbox_sequence` (`outbox.SequenceMigration`).
- Produces `testutil.(*AWSFixture).AddQueue(t, name) string`, which returns a queue URL subscribed to the topic with raw delivery.

- [ ] **Step 1: write the failing tests.**
  - `pkg/config`: `TestLoadNotificationsDefaults` (grpc addr default; dev-only knobs refused when `APP_ENV≠dev`).
  - `services/notifications`: `TestMigrationsApply` (integration, migrate up then down then up).
- [ ] **Step 2: run them.** Expected: FAIL (fields and package missing).
- [ ] **Step 3: implement.**
  - proto plus `buf generate`, migrations, config fields;
  - `main.go`: migrate, connect, gRPC server with an Unimplemented embed, outbox relay source `notifications`;
  - compose service `notifications` (`GRPC_ADDR: ":9006"`), `NOTIFICATIONS_GRPC_ADDR` and queue URL in `service-env`;
  - localstack queue `acme-card-notifications` + DLQ with a redrive policy (maxReceiveCount 5) + subscription; give `acme-card-audit` the same redrive policy;
  - `notifications` schema in `postgres-init.sql`.
- [ ] **Step 4: verify.** `go build ./... && go test ./pkg/config/ && go test -tags=integration ./services/notifications/...` → PASS. `make up` → the notifications container is up.
- [ ] **Step 5: commit.** `feat(notifications): contracts, schema and service skeleton`.

### Task 2: Rules — classify, render, backoff, preferences

**Files:** `services/notifications/internal/rules/{rules.go,rules_test.go}`.

**Interfaces:**
- Produces:
  - `type Notice struct{ Type, AccountID string; Mandatory bool; Text func(cur string) string }`;
  - `Classify(ev events.Event) (Notice, bool)`, which does not cover fraud;
  - `MandatoryTypes`;
  - `AllTypes []string`, in table order;
  - `Backoff(failures int) (time.Duration, bool)`, where false means give up;
  - `ValidatePreference(type string, push, sms, email bool) error`, returning `ErrUnknownType` / `ErrMandatory`;
  - `PartnerEventAllowed(eventType string) bool`;
  - `FormatMinor(amount int64, cur string) string`.

- [ ] **Step 1: failing unit tests.**
  - `TestClassifyEveryType`, table-driven over the Global Constraints table, with real payload shapes:
    - `fee.charged` with `AccountID`;
    - `delinquency_changed` from none to past_due → `card_blocked`, and from past_due to past_due → none.
  - `TestTextsCarryNoSecrets`: payout text shows `****6789`, never the full number even when the payload carried one.
  - `TestBackoffSchedule`: 1m, 5m, 30m, 2h, 6h, then false.
  - `TestMandatoryCannotBeSilenced`.
  - `TestPartnerAllowList`: `payout.instruction_read`, `http.request`, `audit.queried` and `snapshot.account` are false.
- [ ] **Step 2: run.** Expected: FAIL.
- [ ] **Step 3: implement.**
- [ ] **Step 4: run.** `go test ./services/notifications/internal/rules/` → PASS.
- [ ] **Step 5: commit.** `feat(notifications): notification rules`.

### Task 3: Intake — consumer and processor

**Files:** `services/notifications/internal/notify/{intake.go, consumer.go, harness_integration_test.go, intake_integration_test.go}`.

**Interfaces:**
- Consumes: Task 2 rules; accounts `GetAccount` (`customer_id`, `currency`).
- Produces:
  - `NewService(pool, accounts accountsv1.AccountsServiceClient, providers Providers, box *secretbox.Box, cfg Config) *Service`;
  - `(*Service).Process(ctx, ev events.Event) error`: one transaction that inserts into `processed_events` (on conflict nothing → return nil), records the decline window, then inserts deliveries;
  - `NewConsumer(sqs, queueURL, svc, log)`, the same contract as the audit consumer: delete only after `Process` succeeds.

- [ ] **Step 1: failing integration tests.** Accounts schema migrated, real accounts rows created via the accounts test server, or a fake `AccountsServiceClient`.
  - `TestProcessCreatesOneDeliveryPerChannel`: 3 rows.
  - `TestRedeliveredEventCreatesNothing`: `Process` twice, plus 10 concurrent calls → 3 rows.
  - `TestPreferencesFilterChannels`.
  - `TestMandatoryIgnoresAllOffRow`: a row written directly by SQL → still at least push.
  - `TestFraudAfterThreeDeclinesOncePerHour` (counting reasons only; three `card_frozen` declines raise nothing).
  - `TestCardBlockedOnPastDueChargeOffAndRestriction`.
  - `TestPartnerDeliveriesOnlyForSubscribedActiveAllowedTypes`.
  - `TestAccountLookupFailureLeavesMessage`: the error is returned and nothing is stored.
- [ ] **Step 2: run.** Expected: FAIL.
- [ ] **Step 3: implement.**
- [ ] **Step 4: run.** `go test -tags=integration ./services/notifications/...` → PASS.
- [ ] **Step 5: commit.** `feat(notifications): event intake with once-only deliveries`.

### Task 4: Delivery — providers, webhook sender, workers

**Files:** `services/notifications/internal/notify/{deliver.go, providers.go, webhook.go, deliver_integration_test.go, webhook_test.go}`.

**Interfaces:**
- Produces:
  - `type Providers interface{ Send(ctx, channel, recipient, text string) error }`;
  - `SimProviders{pool, fail map[string]bool}`, which writes `inbox`;
  - `(*Service).DeliverOnce(ctx, kind string /*"customer"|"webhook"*/, now time.Time) (int, error)`;
  - `(*Service).RunDelivery(ctx, every, log)`, which starts both loops;
  - `NewWebhookClient(allowedHTTPHosts []string) *http.Client`: dialer `Control` refuses private, loopback, link-local and metadata IPs unless the host is allowed; `CheckRedirect` returns `ErrUseLastResponse`;
  - `ValidateURL(raw string, allowed []string) error`.
- Each attempt:
  - claim with `FOR UPDATE SKIP LOCKED` and set `attempts+1`, `next_attempt_at = now + 1 min` as a lease;
  - send outside the transaction;
  - then mark `delivered`, or schedule `Backoff`, or `failed`;
  - `delivered` and `failed` enqueue `notification.delivered` / `notification.failed` in the same transaction as the status change.

- [ ] **Step 1: failing tests.**
  - Unit:
    - `TestSignatureVector`: a fixed secret, body and timestamp give a known hex;
    - `TestValidateURL` for `http://` (not allowed), `https://10.0.0.1`, `https://169.254.169.254`, `https://localhost` and an allowed dev host;
    - `TestDialRefusesPrivateAfterResolve`, via a custom resolver.
  - Integration:
    - `TestRetryThenDelivered`: fault for 2 attempts;
    - `TestSixFailuresThenFailedAudited`: advance `now` across the backoff;
    - `TestConcurrentWorkersSendOnce`: 4 workers × 50 rows → 50 sends;
    - `TestSlowPartnerDoesNotDelayCustomers`: an httptest server sleeping 10 s; customer rows deliver within 1 s;
    - `TestWebhookSignedAndVerifiable`: httptest verifies with `webhook.Verify`;
    - `TestDisabledSubscriptionStopsPending`.
- [ ] **Step 2: run.** Expected: FAIL.
- [ ] **Step 3: implement.**
- [ ] **Step 4: run.** `go test ./services/notifications/... && go test -tags=integration ./services/notifications/...` → PASS.
- [ ] **Step 5: commit.** `feat(notifications): reliable delivery with retries and signed webhooks`.

### Task 5: API — preferences, inbox, subscriptions (gRPC and gateway)

**Files:**
- `services/notifications/internal/notify/api.go` + test;
- `services/gateway/internal/server/notifications.go` + test;
- `server.go` Deps;
- `services/gateway/cmd/main.go`;
- notifications `main.go` (consumer + delivery + gRPC wiring).

**Interfaces:**
- Gateway routes:
  - Customer (JWT, `authorized_accounts`). The customer id comes from `GetAccount`, which also enforces ownership.
    - `GET /v1/accounts/{accountID}/notification-preferences`;
    - `PUT /v1/accounts/{accountID}/notification-preferences` with `{type, push, sms, email}`, strict JSON;
    - `GET /v1/accounts/{accountID}/notifications?after=`.
  - Support (role `support`):
    - `POST /v1/support/webhook-subscriptions` with `{name, url, event_types}` → 201 with the secret;
    - `GET /v1/support/webhook-subscriptions`;
    - `DELETE /v1/support/webhook-subscriptions/{subscriptionID}`;
    - `GET /v1/support/webhook-subscriptions/{subscriptionID}/deliveries`.
- Gateway `Deps.Notifications notificationsv1.NotificationsServiceClient`.

- [ ] **Step 1: failing tests.**
  - gRPC integration:
    - `TestSetPreferenceMandatory422`: maps to `FailedPrecondition` + reason `mandatory_notification`;
    - `TestCreateSubscriptionSecretOnceAndSealed`: the DB holds no plain secret; List never returns it;
    - gateway `TestSubscriptionSecretNotReplayed`: the same Idempotency-Key replays the 201 without the secret, and `gateway.idempotency_keys` holds no secret;
    - `TestSubscriptionValidation`: bad URL or event type → `InvalidArgument`.
  - Gateway unit:
    - route matrix (customer on someone else's account → 403; customer on a support route → 403);
    - strict JSON;
    - 422 code passthrough;
    - mutating routes audited by the existing middleware.
- [ ] **Step 2: run.** Expected: FAIL.
- [ ] **Step 3: implement and wire the mains.**
  - Gateway dials `NOTIFICATIONS_GRPC_ADDR`.
  - Notifications runs the consumer, both delivery loops and gRPC.
  - Audit `ProducerSchemas` gains `notifications` in all three lists (audit main, auditctl, audittest).
- [ ] **Step 4: run.** `go test ./services/gateway/... -skip TestEveryRouteHasASimulatorAction && go test -tags=integration ./services/notifications/...` → PASS.
- [ ] **Step 5: commit.** `feat: notification preferences, inbox and partner subscriptions API`.

### Task 6: Simulator — popup and panel 10

**Files:**
- `web/lib/notifications.ts` + test;
- `web/app/page.tsx`;
- `web/app/api/partner/webhook/route.ts`;
- `web/lib/ui-actions.json`;
- compose `web` env: `NOTIFY_ALLOWED_HTTP_HOSTS=web` for notifications.

**Interfaces:**
- `web/lib/notifications.ts`:
  - `newItems(prev, incoming)` dedupes by id;
  - `channelIcon(channel)`;
  - `verifyPartnerSignature(secret, body, ts, sig, now)`, mirroring `pkg/webhook`.
- Partner route:
  - keeps an in-memory `Map<subscriptionId, secret>`, set by the page after registration through `POST /api/partner/secret`;
  - stores the last 20 verified events, shown in panel 10;
  - answers 401 on a bad signature.

- [ ] **Step 1: failing web tests.**
  - `newItems` dedupes and orders the inbox;
  - `verifyPartnerSignature` checks the same vector as Go `TestSignatureVector`, and rejects a stale or tampered request.
- [ ] **Step 2: run.** `npm test` → FAIL.
- [ ] **Step 3: implement.**
  - Popup: fixed bottom-right stack, 6 s per item, polling every 3 s while an account is loaded.
  - Panel 10:
    - preference toggles (mandatory types show a lock and refuse the last channel off);
    - register demo partner;
    - list partners and deliveries;
    - disable;
    - received partner events.
- [ ] **Step 4: verify.**
  - `make web-test && go test ./services/gateway/...`: coverage green.
  - `make up`, then a `/browse` flow: activate a card → popups for activation; a purchase → popup; a partner registered → a delivery `delivered` and the event shown as verified; `NOTIFY_FAIL_CHANNELS=sms` → an SMS retrying. No console errors.
- [ ] **Step 5: commit.** `feat(web): notifications popup and panel`.

### Task 7: End to end, load, docs

**Files:**
- `tests/integration/sprint1_test.go` (the stack gains notifications: second queue via `AddQueue`, `notifytest` server, gateway Deps, relay source);
- `tests/integration/sprint7_test.go`;
- `tests/load/flows.js` / `tests/load/verify.sql`, plus `notify_gate.sh` if needed;
- docs: ADR-022, SPRINTS, README, `docs/testing/sprint7.md`, PROMPTS.

- [ ] **Step 1: failing e2e test** `TestSprint7NotificationsEndToEnd`:
  - open, activate, buy, decline ×3, pay, past due (dev clock);
  - inbox holds exactly the expected types per channel;
  - `suspected_fraud` once;
  - a partner on httptest receives the subscribed events, signatures verified, no duplicates;
  - the audit trail has `notification.delivered`.
- [ ] **Step 2: run.** Expected: FAIL until the stack is wired.
- [ ] **Step 3: wire the e2e stack and implement any gaps.**
- [ ] **Step 4: full suites.** Unit, integration (incl. `TestCustomerJourney`), web.
- [ ] **Step 5: isolated load test** (`-p acme-card-s7`, fresh volumes; stop the dev stack and restart it after).
  - Gate SQL, 0 rows each:
    - duplicate `(event_id,target)` rows;
    - pending rows older than 10 min after the drain;
    - inbox rows without a delivered delivery.
  - The audit gate stays green.
  - p95 ≤ 1.5 s, or recorded with the reason; every delivery delivered or failed within 10 minutes after the load.
- [ ] **Step 6: docs.**
- [ ] **Step 7: commit.** `test: Sprint 7 e2e and load gate; docs (ADR-022)`.

## Unresolved questions
- None. The builder check is folded in (spec §4b; the user decided fraud reasons and card-blocked sources).
