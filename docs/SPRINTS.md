# Sprint plan

Each sprint ships behind the same gates: unit tests, integration tests (testcontainers), the idempotency concurrency test, audit coverage, and (where relevant) a k6 load run. See `docs/TESTING.md`.

## Sprint 0 — Skeleton (with Sprint 1)
Monorepo layout, docker-compose (Postgres, LocalStack), Makefile, proto + buf, shared `pkg/` (config, db, money, logging, idempotency, auth, audit/outbox, events), Terraform skeleton, docs.

## Sprint 1 — Account opening & collateral deposit  ✓ done
- `POST /v1/accounts` → account + card in `pending_collateral`.
- `POST /v1/accounts/{id}/collateral/deposits` → deposit intent.
- `POST /v1/webhooks/bank/deposits` → HMAC-signed settlement; ledger posting `cash_clearing → collateral_held`; limit = Σ confirmed; state → `active`.
- `GET /v1/accounts/{id}`; `GET /healthz`, `GET /readyz`.
- Gateway middleware chain: strict JSON → JWT → rate limit → idempotency → audit.
- Events: `account.opened`, `collateral.deposit_created`, `collateral.confirmed`, `card.activated`.
- `web/` — minimal Next.js (App Router) simulator: mint dev JWT, create account, create deposit, fire signed bank webhook, inspect account, replay the same Idempotency-Key and see the replay flag. Secrets stay in server route handlers.
- Tests: state machine, money, idempotency (50 concurrent same key → 1 effect), webhook signature, ledger balance invariant, audit append-only, k6 1000 VUs.

## Sprint 2 — Authorizations, captures, reversals, refunds, declines  ✓ done
- Simulated card network over HMAC: authorize (7-day hold), single capture ≤ authorized, reversal within 3 days, refunds against captured purchases (partials up to captured).
- Decline reasons, in order: `account_closed`, `collateral_not_confirmed`, `card_frozen`, `velocity_limit_exceeded` (10/min), `purchase_limit_exceeded` (per transaction), `insufficient_credit`. Returned as 201 with `status: declined`.
- Customer: freeze/unfreeze, purchase limit.
- Credit balance when a refund exceeds what is owed.
- Design: `docs/superpowers/specs/2026-10-25-sprint2-card-transactions-design.md`.

## Sprint 3 — Repayments  ✓ done
- Customer payment intents (min $1, capped at the outstanding debt) confirmed by an HMAC bank webhook bound to method and path; credit restored only on settlement; failed payments have no effect; unanswered intents expire after 7 days.
- Returns within 5 days raise the debt; over the limit, the card is restricted (`payment_returned`) until exposure is back within it.
- Credit balance is spendable on purchases (ADR-018); never paid out in cash.
- Design: `docs/superpowers/specs/2026-10-25-sprint3-repayments-design.md`. Results: `docs/testing/sprint3.md`.

## Sprint 4 — Statements, interest, late fees  ← next
- `billing` service: billing cycles, statement generation (S3 object), minimum payment, interest accrual (daily, integer math), late fee on missed due date.
- Tests: deterministic accrual fixtures; statement totals reconcile with ledger.

## Sprint 5 — Close, cancel
- `POST /v1/accounts/{id}/close` (freeze/unfreeze moved to Sprint 2).
- Close in good standing → release collateral (`collateral_held → cash_clearing`). Close with debt → apply collateral to `receivable`, remainder released.
- Tests: close is terminal; collateral application math.

## Sprint 6 — Audit hardening
- Snapshots per aggregate, replay tooling from audit log, retention policy, reconciliation job (ledger vs snapshots).

## Sprint 7 — Notifications
- `notifications` service: outbound webhooks with HMAC, retries with backoff, rejection dedupe. Moved out of Sprint 2.
