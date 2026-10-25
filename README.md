# acme-card

Secured credit card platform: a customer's deposit backs their credit limit. Go microservices, PostgreSQL, gRPC inside, REST at the edge, SNS/SQS events, append-only audit. Runs fully locally in Docker (LocalStack emulates AWS). Terraform describes the AWS target but is never applied from this repo.

## Services
| Service | Role | Port |
|---|---|---|
| gateway | Public REST, JWT, idempotency, rate limit, audit middleware | 8080 |
| accounts | Account/card lifecycle, collateral intents | 9001 (gRPC) |
| ledger | Double-entry journal, balances, limits | 9002 (gRPC) |
| audit | SQS consumer → append-only audit log | — |
| billing | Statements, interest, fees (Sprint 4) | 9003 |
| notifications | Outbound webhooks (Sprint 2) | 9004 |

## Quick start
```bash
make up            # postgres + localstack + services
make test          # unit
make test-integration
make load          # k6, 1000 VUs
make devtoken ACCOUNTS=<uuid>   # mint a dev JWT
```

## Docs
- `docs/DECISIONS.md` — ADRs
- `docs/SPRINTS.md` — sprint plan
- `docs/TESTING.md` — test gates
- `PROMPTS.md` — verbatim prompt log
