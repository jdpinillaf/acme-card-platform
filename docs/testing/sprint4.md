# Sprint 4 — test results (2026-10-25)

Branch `sprint-4`. Design: `docs/superpowers/specs/2026-10-25-sprint4-statements-design.md`; decisions: ADR-019.

## Automated suites
- **Unit**: billing engine fixtures with exact integers (22.80% APR: 100,000 for 30 days without grace → 1,873; past due with interest and fees → 1,958; minimum, grace, derived delinquency, allocation and returns, deterministic statement JSON), config, auth role, gateway routes, decline names.
- **Integration (testcontainers)**: ledger (effective_at, charges, collateral application, sealed periods, ListEntries), accounts (per-account clock, persisted business dates on reconciler retry, charge-off incl. racing a capture, over_limit, delinquency stages), billing (400-day catch-up, statement ↔ ledger reconciliation, pending payment at the due date that fails or settles late, block and cure, charge-off at DPD 60, settlement racing a close, late saga landing in the next cycle, 50 concurrent catch-ups, waivers incl. concurrency and idempotency, catch-ups on a 4-connection pool), S3 store on LocalStack.
- **End to end** (`tests/integration/sprint4_test.go`) through the gateway: statement 1 with grace → on-time minimum → interest on statement 2 → missed minimum → `past_due` decline → support waiver cures it → charge-off at 60 days → statement documents match their hashes → audit trail; same Idempotency-Key ×50 advances the clock once.
- **Mutation checks**: billing tests were proven able to fail by breaking the lock, the pending-payment rule, the fee date, the block and the seal (each made at least one test fail).

## Browser (simulator, gstack headless browser)
Open → deposit → settle → purchase + capture → *To next close* (statement 1: 120.00, minimum 25.00, due 2026-12-16) → *Due + 15* (stage `past_due`, purchase declined `past_due`, fee 25.00, cycle-2 interest 2.37) → support *Waive* of the fee → stage `none`. No console errors.

## Load — 1000 VUs, isolated compose project with fresh volumes
Flow per iteration: Sprints 1–3 unchanged, then a 50.00 purchase, clock +32 days (statement), pay the minimum in 2 of 3 iterations, +20 days, +20 days, check stage and late fee.

**Run 1 — found a bug.** 92% of clock advances timed out at the gateway's 10 s (`http_req_failed` 4.75%). Root cause: each catch-up held a transaction for its account lock and needed a second pool connection for its writes; with ≥ pool-size catch-ups in flight none could proceed. Fixed by turning the lock into a lease row (autocommit UPDATE, no connection held); reproduced first by `TestConcurrentCatchUpsOfDistinctAccountsOnSmallPool` (24 accounts, 4 connections: deadline exceeded → passes).

**Run 2 — after the fix.**

| Metric | Result |
|---|---|
| Requests | 81,206, **0 failed** |
| Checks | **100%** (81,206 / 81,206) |
| Iterations / cards activated | 3,420 / 3,420 |
| Statements generated (every iteration reached its close) | 6,840 (2 per account) |
| Interest charges / late fees | 3,420 / 937 (late fee exactly when the minimum was missed) |
| Accounts past due at the end | 937 |
| wrong balance / wrong final state / wrong billing state | 0 / 0 / 0 |
| p95 overall | **2.24 s — above the 1.5 s gate** |
| p95 clock advance (runs a whole cycle of billing) | 3.64 s (avg 1.96 s) |
| p95 other routes | 0.41–1.01 s |

**Invariants** (`tests/load/verify.sql`, 29 checks incl. 9 new for billing): **0 violations**. Corruption tests in rolled-back transactions confirm each new billing check fires (duplicate fee, statement ≠ ledger, lines/buckets, non-contiguous cycles, waived ≠ waivers, charge/waiver ↔ journal, stuck work).

**Latency gate.** Not met on this laptop, as in Sprint 3 (host saturated by 1000 VUs plus 11 containers). The overall p95 now includes the clock-advance route, which by design runs a full billing cycle (statement, interest, S3 write) per call; it exists only in dev. No A/B against `main` this time: `main` has no Sprint 4 flow to compare.
