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

## ADR-012 — Sprint 1 implementation rulings (2026-10-25)
- `POST /v1/accounts` takes `customer_id` from the JWT `sub`; the body only carries `currency`.
- Bank webhook duplicates are deduplicated by `external_ref`; the route does not require `Idempotency-Key`.
- Webhook amount or currency mismatch → 400, deposit stays `pending` so the bank can resend.
- `credit_available` is never posted; available credit = `collateral_held − receivable`, derived from the ledger.
- Settlement saga: ledger `PostJournal` (idempotent on `deposit:<id>`) first, then the accounts transaction. A crash in between heals on the next webhook retry.
- The gateway has its own outbox. Every mutating request emits `http.request` with status and outcome (`success`, `client_error`, `server_error`, `timeout`, `rolled_back`).
- Rate limit is a Postgres sliding-window counter per JWT subject.
- Each `migrations/` directory is a Go package exporting `embed.FS`. Cross-service tests use exported `*test` packages, so `internal/` stays private.
- Go 1.26 toolchain, required by transitive dependencies.
- Local host ports: lb 8088, web 3010, because other local projects use 8080 and 3000.
- Load gate: 1000 VUs with think time and p95 < 1.5 s. `STRESS=1` measures saturation without a latency gate.
- In AWS, services build the DSN from `DB_HOST` + the RDS-managed secret, with `sslmode=verify-full`.

## ADR-013 — Final review fixes and deferred items (2026-10-25)
**Fixed:**
- Idempotency claims finish detached from request cancellation, so a client disconnect can no longer cause a double effect. Deposits also dedupe on the key hash.
- Settlement runs reserve → ledger → finalize, so the ledger and accounts cannot diverge.
- `X-Forwarded-For` is trusted only from configured proxies.
- Rejected webhooks no longer write per-request audit rows.
- Duplicate waiters are bounded: 409 `request_in_progress`. The readiness probe has its own pool.
- The simulator requires `SIMULATOR_ENABLED=1` and is bound to loopback.
- KMS key policies cover Logs and SNS. Schemas are created on startup. The RDS CA bundle ships in the image.

**Deferred (pre-deploy hardening, required before any real AWS use):**
- Per-service DB roles with least privilege. Today every service gets the RDS master secret, so the append-only triggers could be disabled by that role. Audit should use a role without UPDATE/DELETE/ALTER.
- One IAM task role per service instead of one shared role.

## ADR-014 — Sprint 2 scope decisions (2026-10-25)
- **Overdraft is removed from the product**, not deferred. ADR-007's "future opt-in feature" is withdrawn: a purchase can never exceed the collateral-backed limit.
- Outbound notifications move out of Sprint 2 into a sprint of their own.
- Freeze/unfreeze moves from Sprint 5 into Sprint 2, because `card_frozen` is a Sprint 2 decline reason.
- Card network simulated through HMAC-signed endpoints. Holds live 7 days. No partial approvals, incremental authorizations, partial captures, captures without an authorization, standalone refunds or currency conversion. Captures are for at most the authorized amount. Reversals are allowed within 3 days. Refunds after payment leave a credit balance. Velocity is 10 attempts per minute. The purchase limit applies per transaction. Handled declines return 201 with a reason.
- Full design: `docs/superpowers/specs/2026-10-25-sprint2-card-transactions-design.md`.

## ADR-015 — Sprint 2 implementation rulings (2026-10-25)
- Every ledger money operation (journal, hold, capture, release, refund) first locks the account's `collateral_held` balance row as a per-account mutex. This prevents deadlocks between hold paths and journal paths.
- Velocity runs inside the authorization transaction (`ratelimit.AllowTx`). A separate transaction needed a second pooled connection per in-flight authorization and deadlocked the pool under concurrency.
- Authorization holds use the deterministic reference `auth:<network_ref>`; capture journals use `capture:<authorization_id>` and refunds `refund:<network_ref>`. Retries after a crash therefore re-use the same ledger records.
- A declined hold stores nothing in the ledger; the decision lives on the authorization row.
- The account id doubles as the card token until card numbers exist.
- `buf lint` allows shared response envelopes (`RPC_REQUEST_RESPONSE_UNIQUE`, `RPC_RESPONSE_STANDARD_NAME`).
- The k6 script is now `tests/load/flows.js` and covers both sprints.

## ADR-016 — Sprint 2 final review fixes (2026-10-25)
**Context.** The final review found that accounts called the ledger while holding its own transaction and row locks. A lost response could then leave the two services divergent:
- orphan holds;
- a refused capture already charged;
- refunds credited twice;
- one bad row blocking hold expiry for everyone.

**Decision.** Every card network operation uses the Sprint 1 settlement pattern: **reserve (short committed tx) → ledger (idempotent, outside any accounts lock) → finalize (short tx)**.
- Authorizations are reserved as `pending`.
- Captures and reversals claim their `network_ref` and a `pending_op` before the ledger is touched, so a reused ref fails first.
- Refunds are reserved as `pending` rows that count toward the cap.
- After a reservation commits, the saga runs on a context detached from the caller's cancellation, bounded by 15 s.
- `ResolvePending`, run every minute, handles anything left after 2 minutes:
  - it fails unanswered authorizations and releases their holds, because the network treats them as declined;
  - it resumes captures, reversals, refunds and settling deposits, because they were explicitly requested.
- Expiry runs one row per transaction, and failures are marked and retried hourly.
- The ledger expires active holds older than 8 days as a final safety net.

**Other fixes.**
- Network HMAC signatures now cover the method and path.
- `network_ref_reuse` returns 422.
- A capture replay must carry the same amount.
- Account policy is read `FOR SHARE` inside the reservation.
- Velocity windows are swept.
- The request that performs a transition answers fresh; every other copy is a replay.

**Deferred.**
- `GetBalances` reads balances and holds in two statements, so available credit can be briefly overstated for display.
- Replays return the current state rather than a stored original response.
