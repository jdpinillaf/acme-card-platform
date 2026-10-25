# Sprint 4 — Statements, interest, late fees, delinquency

Status: builder-checked, approved to build · 2026-10-25 · builds on Sprints 1–3 (`docs/DECISIONS.md` ADR-001…018)

## 1. Intent

Every active account gets a monthly statement. The statement says what the customer owes, the minimum to pay and the due date. Carried balances accrue interest. A missed minimum costs a late fee, then a temporary block, and at 60 days the collateral is applied to the debt and the card is blocked for good.

Everything has to be testable without waiting a month, so each account gets a simulated clock that only moves forward.

## 2. Goals, non-goals, anti-goals

**Goals**
- A new `billing` service owns cycles, statements, interest, fees and delinquency.
- One statement per account per cycle, including cycles with no activity. The statement is a JSON object in S3, plus a Postgres row with totals and a SHA-256 of the object.
- Interest accrues daily with integer math and is posted at cycle close. Standard grace period.
- Minimum payment, late fee, past-due block, charge-off at 60 days past due.
- Payments are allocated fees → interest → principal.
- Support can reverse (waive) interest and fees, fully or partially.
- A per-account simulated clock (dev only) drives all of the above.
- The simulator exposes every new action. The k6 load includes cycle closes.

**Non-goals**
- Autopay.
- PDF statements, email delivery, notifications (Sprint 7).
- Account close and release of surplus collateral (Sprint 5).
- Promotional rates, per-account rates, cash advances, penalty APR.
- Credit-bureau reporting.

**Anti-goals**
- A statement whose totals don't reconcile with the ledger.
- Interest or a fee charged twice for the same cycle or the same missed due date.
- Late fees caused by a payment the customer made on time but the bank confirmed late.
- A customer charged interest on purchases while they kept the grace period.
- Float arithmetic anywhere.
- The simulated clock reachable outside the dev environment.

## 3. Decisions made by the user

| # | Topic | Decision |
|---|---|---|
| 1 | Cycle | Monthly, anchored on the activation day of month. Days 29–31 become 28 |
| 2 | Due date | Statement close + 21 days |
| 3 | Clock | Simulated clock, per account, with an "advance time" control |
| 4 | Statement content | Previous balance, purchases, refunds, payments, returns, interest, fees, new balance, minimum, due date |
| 5 | Format | JSON in S3 (LocalStack) plus a Postgres row with totals and hash. No PDF |
| 6 | Empty cycles | A statement is generated anyway, with zero balance and zero minimum |
| 7 | API | `GET /v1/accounts/{id}/statements` and `GET /v1/accounts/{id}/statements/{statementId}` |
| 8 | Minimum | 1% of principal + cycle interest + cycle fees + any past-due minimum. Floor $25. Never above the new balance |
| 9 | On time | The payment was **received** (created) before the due date. A payment still pending confirmation holds off delinquency until the bank settles or rejects it |
| 10 | Rate | 1.9% monthly = **22.80% APR**, configurable |
| 11 | Grace | Standard grace period |
| 12 | Accrual | Daily balance × APR / 365, accumulated in micro-units, floored to minor units at close |
| 13 | Interest on interest/fees | Only while the account is past due |
| 14 | Late fee | $25 |
| 15 | Block | 15 days after the due date, if the minimum is still unpaid |
| 16 | Charge-off | At 60 days past due: apply the collateral to the debt and block the card permanently |
| 17 | Over collateral | Interest and fees may push the debt above the collateral. Allowed |
| 18 | Income accounts | Per customer account in the ledger |
| 19 | Allocation | Fees → interest → principal |
| 20 | Reversals | Interest and fees can be reversed |
| 21 | Simulator | Exposes everything |
| 22 | Load | Includes cycle closes |

## 4. Interpretations (calls already shown to the user)

- **I-1 Pending payment at the due date.** A payment created before the due date that later settles counts as on time. If it fails or is returned, delinquency counts from the original due date, with the fee and days that would have applied. Nothing is charged while it is pending.
- **I-2 Delinquency timeline** (DPD = days past due, in account time, since the due date of the oldest unpaid minimum):
  - DPD 1: late fee of $25, capped at the unpaid minimum of that statement. One fee per missed due date. Interest on interest and fees starts.
  - DPD 15: decline reason `past_due`. It lifts once all past-due minimums are covered.
  - DPD 60: charge-off. Collateral is applied to the debt up to `min(debt, collateral)`, allocated fees → interest → principal like a payment, and the credit limit drops by the same amount. The card is blocked permanently with decline reason `charged_off`. Surplus collateral stays held until the Sprint 5 close. No more interest or fees after charge-off; statements keep being generated so the remaining debt or surplus is visible.
- **I-3 Over collateral.** If interest or fees push `Σ active holds + receivable` above the limit, the card is restricted (decline reason `over_limit`, same priority slot as `payment_returned`) until exposure is back within the limit. Purchases still never push exposure over the limit.
- **I-4 Reversals.** A support role (JWT claim `role: "support"`, issued by `cmd/devtoken`) reverses all or part of a specific interest or fee charge, with a mandatory reason. The total reversed per charge never exceeds the charge. A reversal lowers the bucket it came from and is audited.

## 5. Builder's calls (made while writing the design)

- **C-1 Clock ownership.** The account clock is an offset stored on the account (`accounts` service). `account_now = now() + offset`. It only moves forward. The advance endpoint exists only when the gateway runs with `DEV_CLOCK_ENABLED=1` (compose dev); otherwise the route is not registered (404).
- **C-2 What the clock drives.** Cycle closes, due dates, DPD, interest days, the "received" time of payments, and the business date of every ledger entry. Infrastructure timers stay on real time: idempotency TTL, velocity windows, webhook timestamp tolerance, hold expiry (7 days), payment-intent expiry, reversal and return windows, and the reconciler. Cost if wrong: in the simulator a hold does not expire when a month is skipped.
- **C-3 Business date in the ledger.** Journal entries gain `effective_at` (business date) next to `posted_at` (system time). Every posting RPC takes an optional `effective_at`; accounts passes `account_now`, captured at reserve time and persisted in the pending row so a reconciler retry posts the same date. Existing entries get `effective_at = posted_at`.
- **C-4 Buckets come from the ledger.** `receivable` stays the single total debt, so all exposure rules stay as they are. Billing derives principal / interest / fees buckets by replaying the account's journal in `(effective_at, id)` order with one pure function. Purchases add to principal; refunds and reversals of purchases reduce principal; payments and collateral application go fees → interest → principal, with any excess making principal negative (credit balance); a payment return restores exactly the buckets its payment reduced; interest and fee charges add to their bucket; waivers reduce theirs. Each statement stores its closing buckets, so the next replay starts there.
- **C-5 New ledger sub-accounts.** `interest_income` and `fee_income`, per customer account. Interest charge: debit `receivable` / credit `interest_income`. Fee: debit `receivable` / credit `fee_income`. Waiver: the reverse. Charge-off: debit `collateral_held` / credit `receivable`.
- **C-6 Grace.** A cycle keeps grace if the previous statement's new balance was ≤ 0 or was paid in full (payments received by its due date, minus returns). The first cycle has grace. Without grace, interest accrues on each day's positive principal for the whole cycle. While past due, the daily base is the whole positive balance (principal + interest + fees). No trailing interest when grace is regained.
- **C-7 Interest math.** `apr_bps = 2280`. Per day: `micro += max(base,0) × apr_bps × 1_000_000 / (10_000 × 365)` using integer division per day. At close: `interest_minor = micro / 1_000_000`; the remainder is dropped (in the customer's favour). Config `BILLING_APR_BPS`.
- **C-8 Minimum.** `min(new_balance, max(2500, principal/100 + cycle_interest + cycle_fees + past_due_minimum))`. 0 when `new_balance ≤ 0`. Refunds don't count toward paying a minimum; only payments do.
- **C-9 Catch-up.** Billing runs every due event for an account (cycle close, late fee, block, charge-off) in chronological order up to `account_now`. Advancing the clock triggers a synchronous catch-up for that account; a per-minute sweep catches up the rest. Every event is idempotent on a deterministic reference (`interest:<account_id>:<cycle>`, `late_fee:<account_id>:<cycle>`, `chargeoff:<account_id>`), so a retried or concurrent catch-up posts once.
- **C-10 Decline order** becomes:
  1. account_closed
  2. charged_off
  3. collateral_not_confirmed
  4. currency_not_supported
  5. payment_returned / over_limit
  6. past_due
  7. card_frozen
  8. velocity_limit_exceeded
  9. purchase_limit_exceeded
  10. insufficient_credit
  System restrictions can coexist (an account can be both past due and over limit); the decline reports the highest one.
- **C-11 Billing summary.** `GET /v1/accounts/{id}/billing` returns account time, current cycle dates, last statement summary, minimum due, due date, past-due amount, DPD, grace status and delinquency stage.

## 5b. Builder-check calls (user delegated these: "al terminar inicia con el sprint")

- **B-1 Payment dates.** A payment carries two dates. `received_at` = `account_now` when it is created, stored on the payment row. `effective_at` in the ledger = `account_now` at settlement, like every other entry. On-time, minimum-paid and paid-in-full are judged by `received_at`. Statements show a payment in the cycle where it was posted, and list payments still pending at close as an informational line. Interest keeps accruing until the payment posts. Closed statements are never changed. Cost if wrong: a customer with lost grace pays a few days of interest on a payment that is in flight.
- **B-2 Later events never rewrite closed cycles.** Delinquency is always derived, never stored as a running counter. "Credited" means payments that are settled and not returned, or still pending, plus waivers. A pending payment counts as paid until the bank answers.
  - Minimums cumulate, because each minimum already includes any past-due minimum (C-8). So only L, the latest statement whose due date has passed, matters.
  - **Past-due amount** = `max(0, minimum_L − credited since close_L)`.
  - **Missed due date.** Statement S missed its due date if what was credited in `[close_S, end of due_S]` is less than `minimum_S`.
  - **DPD.** If the past-due amount is 0, DPD is 0. Otherwise DPD counts from the due date of the oldest statement in the unbroken run of missed statements that ends at L. A late payment that covers the past-due amount cures the account.
  - **Late fee.** The fee for S is posted once (`late_fee:<account_id>:<cycle>`) as soon as S has missed its due date with nothing pending in that window, even if that is weeks later.
  - **Block and charge-off** follow the derived DPD, so a payment that fails late can trigger them immediately.
  - **Interest.** Interest for closed cycles is never recomputed. Grace for a cycle is decided at its close with what is known then, counting pending payments as paid. A later failure does not charge back that interest.
  - Cost if wrong: a little interest revenue is lost on payments that fail after a close.
- **B-3 Currency.** The $25 floor and late fee come from a per-currency table in billing config (`BILLING_FIXED_AMOUNTS`), in minor units. Defaults: USD 2500, EUR 2500, MXN 45000, COP 10000000. An account in a currency with no entry gets no floor and no fee. Cost if wrong: a config change.
- **B-4 After charge-off.**
  - Active holds are released at charge-off; a later capture of one fails like a capture of an expired hold.
  - The customer can still pay any remaining debt (the Sprint 3 cap applies).
  - Later statements are balance-only: no minimum, no due date, no DPD, no interest, no fees.
  - A refund after charge-off can create a credit balance. It stays on the account next to the surplus collateral; both are settled by the Sprint 5 close.
  - Invariant: `credit_limit = remaining collateral`, and no active holds.
- **B-5 Waivers count toward the minimum.** A waiver lowers the past-due amount (B-2), so waiving the fee or interest that makes up the unpaid minimum can end delinquency and lift `past_due`.
- **B-6 Days and boundaries.** All days are UTC days of account time. Cycle N covers `[close_{N-1} 00:00, close_N 00:00)`, so a purchase on the closing day belongs to the next cycle. Interest accrues on the balance at the end of each day. A payment received any time on the due date is on time; DPD 1 starts at 00:00 the day after.

## 6. How it works

### 6.1 Ledger
- `effective_at` on journal entries (C-3); new sub-accounts (C-5).
- New RPCs: post an interest charge, post a fee, post a waiver against a charge, post a charge-off; list an account's journal entries by `effective_at` range (for billing replay).

### 6.2 Accounts
- Clock offset on the account; `AdvanceClock(account_id, days)` (dev); `account_now` exposed on the account.
- Pending rows store the `effective_at` they will post with.
- Delinquency stage set by billing (`past_due`, `charged_off`), plus the `over_limit` restriction (I-3), feeding decline evaluation (C-10).
- Charge-off operation (I-2) owned by accounts, because it changes the credit limit: reserve → ledger → finalize like every other money op.

### 6.3 Billing (new service)
- Own schema: cycles/statements (totals, buckets, minimum, due date, S3 key, SHA-256, status), charges (interest and fee rows with their waived amount), delinquency state per account, catch-up cursor per account.
- gRPC: catch up an account, list/get statements, billing summary, list charges, waive a charge.
- Writes statement JSON to `s3://acme-card-statements-dev/statements/<account_id>/<statement_id>.json` (bucket already created by `deploy/localstack/init-aws.sh`).
- Publishes `statement.generated`, `interest.charged`, `fee.charged`, `charge.waived`, `account.past_due`, `account.charged_off` through the outbox, so audit records them.

### 6.4 Gateway
- Customer (JWT): `GET /v1/accounts/{id}/statements`, `GET /v1/accounts/{id}/statements/{statementId}`, `GET /v1/accounts/{id}/billing`, `GET /v1/accounts/{id}/charges`.
- Support (JWT with `role: support`, any account): `POST /v1/support/accounts/{id}/charges/{chargeId}/waivers` with `{amount_minor, reason}` + Idempotency-Key.
- Dev only (C-1): `POST /v1/dev/accounts/{id}/clock/advance` with `{days}` + Idempotency-Key; 1–400 days per call.
- Account JSON gains `account_time`; decline reasons gain `past_due`, `charged_off`, `over_limit`.

### 6.5 Simulator
- A "7 · Billing" panel: advance the clock (+1 day, +N days, to next close, to due date + N), billing summary, statements list and detail (rendered JSON), charges list with a support "Waive" action.
- `web/lib/ui-actions.json` lists the new routes, so the existing route-coverage test keeps every action exposed.

## 7. Test gates
- **Interest fixtures.** Deterministic cases with exact expected minor units: grace kept → 0 interest; grace lost → daily principal × 2280 bps / 365 floored; past due → base includes interest and fees; credit balance → 0; mid-cycle purchases and payments.
- **Allocation.** Fees → interest → principal; return restores the same buckets; waiver reduces its own bucket; charge-off allocates like a payment.
- **Cycle dates.** Activation on the 31st closes on the 28th; due = close + 21 days; a 400-day advance produces 13 statements in order.
- **Statement reconciles.** `previous + purchases − refunds − payments + returns + interest + fees − waivers − charge-off = new balance = ledger receivable at close (effective_at)`; S3 object SHA-256 = stored hash.
- **Delinquency.** Unpaid → fee at DPD 1 (once), `past_due` decline at DPD 15, lift after paying the past-due amount, charge-off at DPD 60 (limit drops, `charged_off` decline, surplus collateral kept). A payment created before the due date and settled after it → no fee. The same payment failing → fee and DPD from the original due date.
- **Idempotency and concurrency.** Advance ×50 with the same key → one advance. 50 concurrent catch-ups → one statement, one interest charge, one fee per cycle. Waivers ×50 with the same key → one waiver; concurrent waivers never exceed the charge.
- **Over limit.** Interest pushing exposure over the limit → `over_limit` restriction; a payment lifts it.
- **Clock guard.** Without `DEV_CLOCK_ENABLED` the advance route returns 404.
- **Invariants (`verify.sql`).** Statement totals reconcile with journals; one interest and at most one fee per statement; waived ≤ charged; buckets sum to receivable at each close; charged-off accounts blocked with limit = remaining collateral; exposure > limit only when explained by returns or charges, and then restricted.
- **Load.** k6 iterations advance their own account's clock through a few cycles, pay some minimums and miss others; invariants stay at 0.

## 8. Left to the builder
- Table and column names, proto shapes, event payloads.
- Statement JSON field names (must carry the decision-4 content).
- Pagination.
- Simulator layout.
