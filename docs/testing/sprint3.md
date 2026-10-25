# Sprint 3 test results (2026-10-25)

Kept separate from `docs/TESTING.md`, which has an uncommitted audit-results section from another session.

| Gate | Result |
|---|---|
| Unit (`make test`) | 12 packages pass with `-race` |
| Integration + end to end (`make test-integration`) | 17 packages pass. New: ledger spendable credit balance, payment and return postings; payment cap (10 concurrent × 300 on a 1000 debt → 3 created); settlement saga (same webhook ×50 → 1 journal); reused external_ref posts nothing; settle racing a refund; return within 5 days restricts over the limit and a later payment lifts it; return racing a purchase; reconciler resumes settling/returning payments; `TestSprint3EndToEnd` (6 scenarios over HTTP + gRPC + LocalStack audit) |
| Simulator (`make web-test`) | 9/9, including `actions.test.ts`; Go `TestEveryRouteHasASimulatorAction` walks the real router and fails if any route lacks a UI action (mutation-checked) |
| Browser QA | Settle restores 500.00; return → outstanding 1,000.00 and `payment_returned`; purchase declined `payment_returned`; paying 500.00 lifts the restriction; no console errors |
| Load, 1000 VUs, Sprints 1–3 flow | 86,155 requests, 9 failed (0.01%, cut at ramp-down), 0 wrong balances, 0 wrong states; **p95 2.09 s — latency gate (1.5 s) not met** with host load average 9–20 |
| A/B (isolated compose projects, fresh volumes, `SHORT=1`) | `main` vs HEAD on shared endpoints: HEAD equal or faster (e.g. authorizations p95 1.16 s vs 5.73 s); no regression |
| Invariants (`tests/load/verify.sql`) | all checks 0 violations after load; new Sprint 3 checks fire on deliberately corrupted data in a rolled-back transaction |

## After final-review fixes

| Gate | Result |
|---|---|
| Unit + integration | 12 + 17 packages pass with `-race`; new tests for sub-minimum payoff, late settlement after expiry, fail-safe restriction when the post-commit ledger read fails, stale lift blocked by `exposure_epoch`, reconciler continuing past a failing payment, 422 on reference reuse |
| Load, 1000 VUs, Sprints 1–3 incl. 1 return in 10 | 56,340 requests, 92 failed (0.16%, all 504 gateway timeouts on payment routes with host load average 13–26; sagas finish server-side), 0 wrong balances, 0 interrupted iterations; p95 4.25 s (gate not met on this host) |
| Invariants after that run (980 returned payments) | all 0; revised checks fire on corrupted data |
