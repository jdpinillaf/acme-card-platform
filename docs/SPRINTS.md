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

## Sprint 4 — Statements, interest, late fees  ✓ done
- `billing` service: monthly statements on the activation day (JSON in S3 + Postgres row), due 21 days later, minimum = 1% + interest + fees + past due (floor $25).
- Interest 1.9% monthly (22.80% APR), daily integer accrual, standard grace; $25 late fee; block at 15 days past due; collateral applied and card blocked for good at 60 (ADR-019).
- Per-account simulated clock (dev only) drives cycles and business dates; support waivers of interest and fees.
- Design: `docs/superpowers/specs/2026-10-25-sprint4-statements-design.md`. Results: `docs/testing/sprint4.md`.

## Sprint 5 — Close, cancel  ✓ done
- Close by the customer or support (reason required): card blocked at once, final statement, debt taken from the collateral on its due date, payout of the remaining collateral + credit balance to an encrypted destination once the bank confirms (ADR-020).
- Payouts wait for the 5-day payment return window; failed payouts are retried by support; late refunds get an additional payout.
- Cancel of an unactivated account voids its pending deposits. A new account is allowed after a close with no debt.
- Design: `docs/superpowers/specs/2026-10-25-sprint5-close-design.md`. Results: `docs/testing/sprint5.md`.

## Sprint 6 — Audit hardening  ✓ done
- Tamper-evident hash chain, gap-free per-source numbering, signed checkpoints in an Object-Lock bucket, redaction of personal data and secrets (ADR-021).
- Account snapshots and `account.changed`, replay against live state, daily reconciliation of the 36 invariants.
- Retention: 13 months hot, archive → verify → purge, 7 years archived. Auditor role with an audited read-only API, exports, simulator panel 9.
- Design: `docs/superpowers/specs/2026-10-25-sprint6-audit-design.md`. Results: `docs/testing/sprint6.md`.

## Sprint 7 — Notifications  ✓ done
- Customer notifications by push, SMS and email (simulated), nine types, preferences per customer with three mandatory types; suspected-fraud detection (ADR-022).
- Partner webhooks registered by support, HMAC-signed, secret shown once; retries with backoff then `failed`; SSRF-safe delivery.
- Simulator: popup at the bottom right and panel 10. Design: `docs/superpowers/specs/2026-10-25-sprint7-notifications-design.md`. Results: `docs/testing/sprint7.md`.

