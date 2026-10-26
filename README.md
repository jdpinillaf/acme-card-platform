# acme-card

Secured credit card platform. A customer's collateral deposit backs their credit limit: the card activates only once the bank confirms the deposit, and the limit equals the confirmed collateral (no overdraft in the MVP).

Go microservices, PostgreSQL, gRPC inside, REST at the edge, SNS→SQS events, append-only audit. Runs fully locally in Docker; LocalStack emulates AWS. Terraform describes the AWS target but is never applied from this repo.

## Architecture

```
browser ── web (Next.js simulator :3010) ──┐
                                           ▼
client ─────────────────────────────▶ lb (nginx :8088) ──▶ gateway ×2 ──gRPC──▶ accounts ──gRPC──▶ ledger
bank ── HMAC webhook ──────────────────────┘                  │  └──gRPC──▶ billing ──▶ S3 statements ▲
                                                              │                    │                │
                                                              └──── outbox ────────┴──── outbox ────┘
                                                                          │ SNS (acme-card-events)
                                                                          ▼
                                                                    SQS ──▶ audit (append-only)
```

| Service | Role | Port |
|---|---|---|
| gateway | REST, JWT HS256 + `authorized_accounts`, rate limit, idempotency, audit, HMAC webhook | 8080 (behind lb) |
| accounts | Account/card lifecycle, collateral deposits, bank settlement | 9001 gRPC |
| ledger | Double-entry append-only journal, balances | 9002 gRPC |
| audit | SQS consumer → hash-chained `audit.audit_events` (append-only), verify, checkpoints, replay, reconciliation, retention | 9005 gRPC |
| web | Flow simulator (`/`) and cardholder portal (`/portal`); signs JWT/HMAC server-side | 3010 |
| billing | Statements (S3), interest, late fees, delinquency, charge-off, support waivers | 9004 gRPC |
| notifications | Placeholder (Sprint 7) | — |

## Quick start

Requirements: Docker, Go ≥ 1.26 (or `GOTOOLCHAIN=auto`), Node 24 for `web/` development, Terraform ≥ 1.9 for validation.

```bash
make up                  # postgres, localstack, ledger, accounts, audit, gateway ×2, lb, web
open http://localhost:3010   # flow simulator
open http://localhost:3010/portal   # cardholder portal (Spanish): buy, refunds, freeze, pay, statements, close
make test                # unit tests
make test-integration    # integration + end-to-end (needs Docker)
make web-test            # simulator tests + typecheck
make load                # k6: 1000 VUs, then SQL invariant checks
STRESS=1 make load       # same, without think time (saturation)
make tf-validate         # terraform validate dev + prod
make down                # stop and wipe volumes
```

Host ports are configurable in `.env.dev` (`GATEWAY_PORT`, `WEB_PORT`).

### API by hand

```bash
export JWT_HS256_SECRET='dev-only-change-me-32-bytes-minimum!!' BANK_WEBHOOK_SECRET=bank-dev-secret
T=$(go run ./cmd/devtoken --sub cust-1)
curl -XPOST localhost:8088/v1/accounts -H "Authorization: Bearer $T" -H "Idempotency-Key: $(uuidgen)" -d '{"currency":"USD"}'
T=$(go run ./cmd/devtoken --sub cust-1 --accounts <account_id>)
curl -XPOST localhost:8088/v1/accounts/<account_id>/collateral/deposits -H "Authorization: Bearer $T" -H "Idempotency-Key: $(uuidgen)" -d '{"amount_minor":50000}'
go run ./cmd/banksim --deposit <deposit_id> --amount 50000 --repeat 10   # 1 applied, 9 replays
curl localhost:8088/v1/accounts/<account_id> -H "Authorization: Bearer $T"
```

### Card network (simulated, HMAC with `NETWORK_WEBHOOK_SECRET`)

| Route | Result |
|---|---|
| `POST /v1/network/authorizations` | 201 `approved` or 201 `declined` + `decline_reason` |
| `POST /v1/network/authorizations/{id}/capture` | 201, at most the authorized amount, once |
| `POST /v1/network/authorizations/{id}/reversal` | 200, uncaptured and within 3 days |
| `POST /v1/network/authorizations/{id}/refunds` | 201, partials up to the captured amount |

Customer controls (JWT): `POST /v1/accounts/{id}/freeze|unfreeze`, `PUT /v1/accounts/{id}/limits`, `GET /v1/accounts/{id}/transactions`.

### Repayments (Sprint 3)

| Route | Who | Result |
|---|---|---|
| `POST /v1/accounts/{id}/payments` `{amount_minor}` | customer (JWT + Idempotency-Key) | 201 pending; min 100, at most the outstanding debt |
| `GET /v1/accounts/{id}/payments` | customer | newest first |
| `POST /v1/webhooks/bank/payments` | bank (HMAC bound to method + path) | settled restores credit; failed changes nothing |
| `POST /v1/webhooks/bank/payments/{id}/return` | bank | within 5 days; over the limit the card is restricted (`payment_returned`) |

Account JSON shows `outstanding_minor`, `credit_balance_minor` and `restricted_reason`. A credit balance is spent on purchases first.

### Billing (Sprint 4)

| Route | Who | Result |
|---|---|---|
| `GET /v1/accounts/{id}/statements` | customer | statements, oldest first |
| `GET /v1/accounts/{id}/statements/{statementId}` | customer | statement + its S3 JSON document |
| `GET /v1/accounts/{id}/billing` | customer | account time, cycle, minimum, due date, past due, DPD, grace, stage |
| `GET /v1/accounts/{id}/charges` | customer | interest and late-fee charges with waived amounts |
| `POST /v1/support/accounts/{id}/charges/{chargeId}/waivers` `{amount_minor, reason}` | support (`role: support` JWT + Idempotency-Key) | 201; Σ waived ≤ charged |
| `POST /v1/dev/accounts/{id}/clock/advance` `{days}` | customer, **dev only** (`DEV_CLOCK_ENABLED=1`, `APP_ENV=dev`) | 1–400 days; billing catches up |

Support token: `JWT_HS256_SECRET=... go run ./cmd/devtoken --sub agent-1 --role support`. Account JSON adds `account_time`, `activated_at` and `delinquency` (`none` / `past_due` / `charged_off`); declines add `past_due`, `charged_off` and `over_limit`.

### Close, cancel and payouts (Sprint 5)

| Route | Who | Result |
|---|---|---|
| `POST /v1/accounts/{id}/close` `{destination:{bank_code, account_number, holder_name}}` | customer | 202 `closing`; new authorizations decline `account_closing` |
| `POST /v1/support/accounts/{id}/close` `{destination, reason}` | support | 202 |
| `POST /v1/accounts/{id}/cancel` | customer | 200 `cancelled` (only before activation); pending deposits voided |
| `GET /v1/accounts/{id}/payouts` | customer | payouts with attempt ids, destination `****1234` |
| `POST /v1/support/accounts/{id}/payouts/{payoutId}/retry` | support | 201, new attempt for a failed payout |
| `GET /v1/bank/payout-instructions/{attemptId}` | bank (HMAC bound to method + path) | the destination in clear, for the bank only |
| `POST /v1/webhooks/bank/payouts` `{attempt_id, amount_minor, currency, destination_last4, status}` | bank | settled releases the money; failed keeps it held |

The close completes after in-flight work, the final statement's due date (debt is taken from the collateral) and the 5-day payment return window. `DESTINATION_KEY` (base64, 32 bytes) encrypts destinations; `RETURN_WINDOW` may only be shortened with `APP_ENV=dev`.

### Audit (Sprint 6)

An auditor token: `JWT_HS256_SECRET=... go run ./cmd/devtoken --sub auditor-1 --role auditor`. Customer and support tokens get 403 (audited as `audit.access_denied`).

| Route | Result |
|---|---|
| `GET /v1/audit/events?account_id=&event_type=&actor=&from=&to=&page_size=&page_token=` | chained events (`chain_seq`, `hash`), redacted; dates RFC 3339 |
| `GET /v1/audit/verify` | chain (from the last full walk), checkpoints, per-source gaps, stuck outboxes |
| `POST /v1/audit/checkpoints` | 201, signed head written to the Object-Lock bucket |
| `POST` / `GET /v1/audit/reconciliation` | run / last run of the 36 invariants |
| `GET /v1/audit/replay/{accountId}` | `differences: []` when the trail matches live state |
| `POST /v1/audit/exports` `{filter}` → `GET /v1/audit/exports/{id}` | NDJSON + sha256 (≤ 100,000 events) |
| `POST /v1/audit/snapshots` | 202, every account snapshotted in the background |

Every call is itself audited (`audit.queried` / `audit.exported`). `AUDIT_SIGNING_KEY` (base64 Ed25519 private key, 64 bytes; the one in `.env.dev` is dev only) signs checkpoints and archive manifests; `AUDIT_BUCKET` is created with Object Lock. Operators: `make auditctl ARGS="verify-chain|checkpoint|archive|verify|purge"`.

## Docs
- `docs/DECISIONS.md` — architecture decisions (ADRs)
- `docs/SPRINTS.md` — sprint plan
- `docs/TESTING.md` — test gates (Sprint 3–6 results in `docs/testing/sprint3.md` … `sprint6.md`; customer journey in `docs/testing/journey.md`) and latest results
- `PROMPTS.md` — verbatim prompt log

### Cardholder portal (`/portal`)

Customer-side test environment in Spanish: open an account and deposit collateral, buy from demo merchants (declines explained), freeze/unfreeze, per-purchase limit, pay (minimum/total/other), statements and charges, close with payout. A simulation rail plays the bank (confirm/reject deposits and payments, return a payment, settle/fail the payout), the merchants (capture, reverse, partial refunds; auto-capture on by default), support (waive charges, retry payouts) and the account clock. The session lives in `localStorage`, so a reload keeps the customer.

Gotchas: the gateway rate limit (`RATE_LIMIT_PER_MINUTE`, 60 in dev) is per customer, and each portal action costs about 5 requests. The payment return window (5 days) runs on wall-clock time, not the account clock, so a customer who made payments stays in the `awaiting_returns` close stage for 5 real days. To test the full close, use a customer without payments.
