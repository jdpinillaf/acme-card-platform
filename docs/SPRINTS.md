# Sprint plan

Each sprint ships behind the same gates: unit tests, integration tests (testcontainers), the idempotency concurrency test, audit coverage, and (where relevant) a k6 load run. See `docs/TESTING.md`.

## Sprint 0 — Skeleton (with Sprint 1)
Monorepo layout, docker-compose (Postgres, LocalStack), Makefile, proto + buf, shared `pkg/` (config, db, money, logging, idempotency, auth, audit/outbox, events), Terraform skeleton, docs.

## Sprint 1 — Account opening & collateral deposit  ← current
- `POST /v1/accounts` → account + card in `pending_collateral`.
- `POST /v1/accounts/{id}/collateral/deposits` → deposit intent.
- `POST /v1/webhooks/bank/deposits` → HMAC-signed settlement; ledger posting `cash_clearing → collateral_held`; limit = Σ confirmed; state → `active`.
- `GET /v1/accounts/{id}`; `GET /healthz`, `GET /readyz`.
- Gateway middleware chain: strict JSON → JWT → rate limit → idempotency → audit.
- Events: `account.opened`, `collateral.deposit_created`, `collateral.confirmed`, `card.activated`.
- `web/` — minimal Next.js (App Router) simulator: mint dev JWT, create account, create deposit, fire signed bank webhook, inspect account, replay the same Idempotency-Key and see the replay flag. Secrets stay in server route handlers.
- Tests: state machine, money, idempotency (50 concurrent same key → 1 effect), webhook signature, ledger balance invariant, audit append-only, k6 1000 VUs.

## Sprint 2 — Authorizations, captures, reversals, refunds
- `POST /v1/accounts/{id}/authorizations` (hold), `.../captures`, `.../reversals`, `POST /v1/refunds`.
- Decline reasons: `account_closed`, `collateral_not_confirmed`, `card_frozen`, `velocity_limit_exceeded`, `purchase_limit_exceeded`, `insufficient_credit`.
- Customer-set `purchase_limit_minor`; velocity limit (N attempts / window).
- `notifications` service: outbound webhooks with HMAC, retry, rejection dedupe.
- Tests: concurrent authorizations never exceed limit; hold expiry; refund restores credit.

## Sprint 3 — Repayments
- `POST /v1/accounts/{id}/payments` (full/partial), payment intents, failed/returned payments.
- Ledger `receivable → credit_available`.
- Tests: concurrent payments same key → one posting; overpayment handling.

## Sprint 4 — Statements, interest, late fees
- `billing` service: billing cycles, statement generation (S3 object), minimum payment, interest accrual (daily, integer math), late fee on missed due date.
- Tests: deterministic accrual fixtures; statement totals reconcile with ledger.

## Sprint 5 — Freeze, close, cancel
- `POST /v1/accounts/{id}/freeze|unfreeze`, `.../close`.
- Close in good standing → release collateral (`collateral_held → cash_clearing`). Close with debt → apply collateral to `receivable`, remainder released.
- Tests: close is terminal; spend after freeze declines; collateral application math.

## Sprint 6 — Audit hardening
- Snapshots per aggregate, replay tooling from audit log, retention policy, reconciliation job (ledger vs snapshots).
