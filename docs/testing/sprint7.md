# Sprint 7 — test results (2026-10-25)

Branch `sprint-7`. Design: `docs/superpowers/specs/2026-10-25-sprint7-notifications-design.md`; decisions: ADR-022.

## Automated suites
- **Unit:**
  - rules: every type from real payload shapes, skips, no secrets in texts, backoff schedule, mandatory rule, partner allow-list, fraud reasons;
  - webhook: signature vector (shared with the web test), URL validation (http, private, metadata, loopback, credentials), dial-time refusal after re-resolution (DNS rebinding), no redirects;
  - config: dev-only knobs refused outside `APP_ENV=dev`;
  - gateway: preference, inbox and partner routes, auth matrix, strict JSON, `422 mandatory_notification`;
  - web: popup feed, partner signature check, partner store.
- **Integration (testcontainers):**
  - intake: one delivery per channel; redelivery and 10 concurrent copies create nothing more; preferences filter channels; a mandatory all-off row still delivers by push; suspected fraud on the 3rd counting decline within 10 min, once per hour, never for `card_frozen` or spread declines; card blocked on past due, charge-off and restriction; partners only for active subscriptions and allowed types; an account lookup failure stores nothing; irrelevant events cost no work; account owners looked up once;
  - delivery: retry then delivered (inbox row, `notification.delivered`); no early retry; 6 failures → `failed` + `notification.failed`; 4 workers × 50 rows → 50 sends; a slow partner does not delay customer channels; webhooks verifiable with `pkg/webhook`; disabled partner gets nothing;
  - API: defaults and mandatory refusal; secret 64-hex, sealed, never listed; subscription validation; inbox per account, newest first;
  - idempotency: a route can redact what is stored, so the partner secret is never replayed nor stored in clear.
- **End to end** (`tests/integration/sprint7_test.go`) through the gateway: partner registered by support (secret once; the replay carries no secret; not stored in clear); activation, purchase, 3 declines (one suspected-fraud alert), payment → every type on push, SMS and email, nothing twice; the partner receives its subscribed events once each with valid signatures; late fee and card blocked after 68 simulated days; mandatory refusal; another customer cannot read the inbox; every delivery audited; no duplicate deliveries. `TestCustomerJourney` and all earlier sprint suites green (the Sprint 1 audit-completeness check now sums every producer outbox).

## Browser (simulator, gstack headless browser, dev stack)
- Partner registered from panel 10; activation → 3 popups; purchase → 3 popups; the partner's delivery `delivered` and shown as "signature ok".
- `late_fee`: turning the third channel off → 422, the box stays on.
- `NOTIFY_FAIL_CHANNELS=sms` → the SMS delivery stays `pending` with "sms provider down (simulated)" while push and email are delivered. No console errors.

## Load — 1000 VUs, isolated compose project `acme-card-s7` (fresh volumes, `RETURN_WINDOW=1m`)

| Run | p95 | Failed requests | Checks | Wrong close state |
|---|---|---|---|---|
| Sprint 7, first | 2.7 s | 79 (504: 61 dev clock advance, 18 close) | 99.97% | 2 |
| A/B: `main` (Sprint 6), same host | **1.1 s** | 0 | 100% | 0 |
| Sprint 7 after the fix | **2.0 s** (gate 1.5 s: **not met**) | **0** | **100%** | **0** |

- **Cause found and fixed:** the notifications consumer opened a transaction for every event in the system and called accounts (which also queries the ledger) for every notifying event, competing with the money path. Now events that cannot notify cost no work, and each account's customer and currency are learned once (`d116002`).
- **Still open:** p95 2.0 s vs 1.1 s on `main`. Likely the doubled SNS → SQS fan-out in LocalStack (two queues per event) and the sequential notifications consumer (~200 events/s, slower than the event rate under load, so a backlog builds up). Recorded as a known regression to work on (consumer concurrency, batching).
- **Notifications gate after this run** (`tests/load/notify_gate.sql`): not completed at merge time; the queue still held ~62k messages minutes after the load (see the consumer throughput above). The first run's gate, after its ~16 min drain: 0 duplicate deliveries, 0 pending, inbox consistent, 0 duplicate events, fraud alerts at most hourly; `verify.sql` 0 violations.
