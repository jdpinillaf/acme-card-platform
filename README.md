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
| audit | SQS consumer → `audit.audit_events` (UPDATE/DELETE/TRUNCATE blocked) | — |
| web | Flow simulator; signs JWT/HMAC server-side | 3010 |
| billing | Statements (S3), interest, late fees, delinquency, charge-off, support waivers | 9004 gRPC |
| notifications | Placeholder (Sprint 7) | — |

## Quick start

Requirements: Docker, Go ≥ 1.26 (or `GOTOOLCHAIN=auto`), Node 24 for `web/` development, Terraform ≥ 1.9 for validation.

```bash
make up                  # postgres, localstack, ledger, accounts, audit, gateway ×2, lb, web
open http://localhost:3010   # flow simulator
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

## Docs
- `docs/DECISIONS.md` — architecture decisions (ADRs)
- `docs/SPRINTS.md` — sprint plan
- `docs/TESTING.md` — test gates (Sprint 3 and 4 results in `docs/testing/sprint3.md`, `docs/testing/sprint4.md`) and latest results
- `PROMPTS.md` — verbatim prompt log
