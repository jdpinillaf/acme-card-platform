# Sprint 5 — test results (2026-10-25)

Branch `sprint-5`. Design: `docs/superpowers/specs/2026-10-25-sprint5-close-design.md`; decisions: ADR-020.

## Automated suites
- **Unit**:
  - `pkg/secretbox`: round trip; tamper, wrong-key and short-key failures; 500 repetitions.
  - Account state machine and decline order (`account_closing`).
  - Engine: `CapToOwed`.
  - Gateway close / cancel / payout / bank routes and their auth matrix.
  - Web: payout request builder and masking.
- **Integration (testcontainers)**:
  - **Ledger:** two-leg payout, over-payout refused, 50 concurrent posts.
  - **Accounts:**
    - cancel, with voided deposits and a late webhook;
    - reopen rules;
    - close: blocks purchases, waits for in-flight work, collateral applied on the final due date, debt paid before the due date, pending payment at the due date, repeated or conflicting close, charged-off account, capture racing the final statement;
    - payouts: return-window hold with a return mid-close, ×50 settle, fail + retry + late settled, webhook checks, credit balance included, destination never leaks, additional payout for a late refund, nothing to pay out.
  - **Billing:** final statement (idempotent, nothing billed after it), billing continues while closing, past due capped at what is owed.
- **End to end** (`tests/integration/sprint5_test.go`) through the gateway:
  - close in good standing → payout → closed → reopen;
  - close with debt → collateral applied → remainder paid out;
  - failed payout → support retry → late settled of the failed attempt gets 409, one journal;
  - cancel → late deposit webhook gets 409;
  - close ×50 with one key → one payout;
  - audit trail contains the new events and never the full destination number;
  - a short return window holds the payout.

## Browser (simulator, gstack headless browser)
- **Close with debt:**
  1. open → deposit 500 → buy 120 + capture;
  2. close (stage `awaiting_due`);
  3. *Due + 1*: collateral applied, payout 380.00 pending;
  4. bank instruction → settle → `closed`, collateral 0.
- **Cancel:** a fresh account → deposit voided; the late webhook gets `deposit_voided`.
- No console errors.
- **Finding:** the billing summary showed past due 25.00 on a closed account whose debt the collateral had paid. Fixed with `CapToOwed`.

## Load — 1000 VUs, isolated compose project (fresh volumes, `RETURN_WINDOW=1m`)
One virtual user in five closes its account on its first iteration, after the Sprints 1–4 flow. The flow: close → +25 days → wait out the return window → bank instruction → settle → assert `closed` with nothing held.

**Run 1 — test design flaw.** Closes ran on iteration 5, which barely fits the test, so only 8 closes started and all were cut at ramp-down. Moved to the first iteration.

**Run 2.**

| Metric | Result |
|---|---|
| Requests | 73,760; 1 counted failed (no 5xx in the gateway audit) |
| Checks | **100%** (73,426 / 73,426) |
| Accounts closed end to end (payout settled) | **134** |
| Closes cut at ramp-down while their payout waits for the bank | 66, correctly in `payout` / `pending` |
| Statements generated | 3,054 |
| wrong balance / final state / billing state / close state | 0 / 0 / 0 / 0 |
| p95 overall | **1.86 s, above the 1.5 s gate** (saturated host, as in Sprints 3–4; the dev-only clock advance runs a billing cycle per call) |

**Invariants.** `tests/load/verify.sql` has 36 checks, 7 of them new for close and payouts: **0 violations**. Corruption tests in rolled-back transactions confirm each new check fires:
- closed account still holding money;
- payout journal ≠ attempt;
- collateral paid out above what was deposited;
- closed account with in-flight work;
- payment returned after its close payout;
- cancelled account with an open deposit;
- stuck saga.
