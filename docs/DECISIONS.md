# Architecture Decision Records

Append-only. One entry per decision. Format: context → decision → consequences.

## ADR-001 — Go monorepo, six services (2026-10-25)
**Context.** Fintech flow with strict concurrency guarantees; team wants microservices from day one.
**Decision.** Single Go module (`github.com/acme/acme-card`) holding `services/{gateway,accounts,ledger,billing,audit,notifications}` plus shared `pkg/`. Each service has its own `cmd/` and Dockerfile target. One go.mod (not go.work) so `go test ./...` and Docker builds stay trivial.
**Consequences.** Shared code changes ripple instantly; service boundaries enforced by package layout and gRPC contracts, not by repo boundaries.

## ADR-002 — gRPC internally, REST at the gateway (2026-10-25)
**Decision.** Public API is REST/JSON on `gateway`. Service-to-service is gRPC + protobuf (`proto/acme/*/v1`, generated with `buf`). Async events go to SNS → SQS.

## ADR-003 — Double-entry, append-only ledger in integer minor units (2026-10-25)
**Decision.** `ledger` owns money. `journal_entries` + `postings` are immutable; every entry must balance (Σ debit = Σ credit, enforced in a single transaction). Amounts are `int64` minor units + ISO-4217 currency. No floats anywhere. Balances are derived; a `balance_snapshots` table caches them.
**Consequences.** Audit trail is intrinsic; corrections are new reversing entries, never updates.

## ADR-004 — Idempotency via insert-first in Postgres (2026-10-25)
**Decision.** Mutating REST endpoints require `Idempotency-Key`. Gateway stores `sha256(subject || key)` as PK with `sha256(method|path|body)`. `INSERT … ON CONFLICT DO NOTHING` first; on conflict `SELECT … FOR UPDATE` waits for the in-flight request, then replays the stored status + body. Mismatched body with same key → 422. Outcome (`success|client_error|server_error|timeout|rolled_back`) and HTTP status are always stored. TTL 24h, swept hourly.

## ADR-005 — Append-only audit via outbox → SNS → SQS (2026-10-25)
**Decision.** Services write domain events to an `outbox` table in the same transaction as the state change. A publisher worker ships them to SNS. `audit` service consumes SQS into `audit.audit_events`, whose role lacks UPDATE/DELETE and whose triggers reject both. Audit middleware exists from Sprint 1.

## ADR-006 — Security baseline (2026-10-25)
**Decision.** JWT HS256 with `authorized_accounts` claim; gateway denies any `account_id` not in the claim. Strict JSON (`DisallowUnknownFields`). Webhooks signed with HMAC-SHA256 over `timestamp.body`, ±5m tolerance; rejections deduplicated to one row per (source, 5-minute window). Rate/velocity limiting via Postgres sliding window.

## ADR-007 — No overdraft in MVP (2026-10-25)
**Decision.** Credit limit = total confirmed collateral. Purchases never exceed it. The 10% overdraft is a future, opt-in feature.

## ADR-008 — LocalStack for AWS locally; Terraform never applied (2026-10-25)
**Decision.** docker-compose runs Postgres + LocalStack (SNS, SQS, S3, KMS). Terraform under `infra/terraform` describes prod-grade AWS (TLS 1.3, KMS, RDS Multi-AZ, S3) with `envs/dev` and `envs/prod`; only `terraform validate` runs.

## ADR-009 — Collateral confirmed by simulated bank webhook (2026-10-25)
**Decision.** `POST /v1/webhooks/bank/deposits` (HMAC-signed) settles a deposit intent. `cmd/banksim` signs and sends one. Dedup by `external_ref`.

## ADR-010 — Multi-currency = one account per (customer, currency) (2026-10-25)
**Decision.** A customer may hold several accounts, one per currency. All postings inside an account share its currency. Top-ups after activation raise the limit.

## ADR-011 — Next.js flow simulator (2026-10-25)
**Context.** Manual and exploratory testing of the flows needs a UI; curl alone is slow for demos.
**Decision.** `web/` is a single-page Next.js (App Router, TypeScript) app. Browser talks only to its own route handlers (`/api/*`), which hold `JWT_HS256_SECRET` and `BANK_WEBHOOK_SECRET`, mint tokens, sign webhooks and forward to the gateway. It ships in docker-compose as `web` on port 3000. It is a dev tool, not a customer product.
