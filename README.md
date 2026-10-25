# acme-card

Secured credit card platform. A customer's collateral deposit backs their credit limit: the card activates only once the bank confirms the deposit, and the limit equals the confirmed collateral (no overdraft in the MVP).

Go microservices, PostgreSQL, gRPC inside, REST at the edge, SNS→SQS events, append-only audit. Runs fully locally in Docker; LocalStack emulates AWS. Terraform describes the AWS target but is never applied from this repo.

## Architecture

```
browser ── web (Next.js simulator :3010) ──┐
                                           ▼
client ─────────────────────────────▶ lb (nginx :8088) ──▶ gateway ×2 ──gRPC──▶ accounts ──gRPC──▶ ledger
bank ── HMAC webhook ──────────────────────┘                  │                    │                │
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
| billing, notifications | Placeholders (Sprints 4 and 2) | — |

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

## Docs
- `docs/DECISIONS.md` — architecture decisions (ADRs)
- `docs/SPRINTS.md` — sprint plan
- `docs/TESTING.md` — test gates and latest results
- `PROMPTS.md` — verbatim prompt log
