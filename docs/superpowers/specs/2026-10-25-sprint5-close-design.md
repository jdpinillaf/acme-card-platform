# Sprint 5 — Close, cancel, collateral and credit-balance payout

Status: builder-checked · 2026-10-25 · builds on Sprints 1–4 (ADR-001…019)

## 1. Intent

A customer, or support, can end the relationship. The card stops working at once. The account closes once everything in flight has resolved and any debt is settled. Whatever money is left goes back to a bank account the customer names: the collateral, and any credit balance. The bank confirms each payout, just like deposits in reverse.

An account that never activated is simply cancelled.

## 2. Goals, non-goals, anti-goals

**Goals**
- Close request by the customer (JWT) or by support (`role: support`, reason required), for any active account: frozen, restricted, past due or charged off.
- Cancellation of an account still in `pending_collateral`: immediate; pending deposits are voided.
- A `closing` state: every purchase declines from the request on. The account becomes `closed` when in-flight work has resolved and the debt is settled.
- Debt at close: a final statement, then a wait until its due date. Whatever is still owed on the due date is debited from the collateral.
- Payout of the remaining collateral plus any credit balance to the bank account given in the close request, confirmed or rejected by an HMAC bank webhook. A rejected payout stays pending, and support retries it.
- Late merchant refunds after close produce an additional payout.
- After a close with no debt, the customer can open a new account in the same currency.
- The simulator exposes every new action. The k6 load includes closes.

**Non-goals**
- Re-opening a closed account.
- Collections or debt sale for debt left after the collateral is exhausted. That debt stays on the closed account and can still be paid.
- Notifications (Sprint 7).
- Real bank account validation (shape only). The number is stored encrypted plus last 4 (U-2).

**Anti-goals**
- Money leaving the platform twice for the same balance, or a payout that is larger than what is held.
- A purchase approved after a close was requested.
- Collateral released while debt is owed.
- An account closed while a hold, payment, deposit, refund or charge-off is still in flight.
- Bank account details in logs, events or API responses beyond the last 4 digits.

## 3. Decisions made by the user

| # | Topic | Decision |
|---|---|---|
| 1 | Who closes | Customer and support |
| 2 | In-flight work | Card blocked at once; the close completes when everything resolves |
| 3 | Frozen / restricted / past due accounts | Can close |
| 4 | Cancel (`pending_collateral`) | Immediate; pending deposits voided; **no money moves** (a late bank settlement of a voided deposit is rejected) |
| 5 | Debt at close | Wait until a date: if the debt is still owed on the final due date, it is debited from the collateral |
| 6 | Interest at close | Yes, up to the close date, with a final statement |
| 7 | Debt above collateral | The remainder stays on the closed account and can still be paid; no new charges |
| 8 | Payout | Like a deposit in reverse: an intent, approved or rejected by the bank |
| 9 | Credit balance | Returned to a bank account at close (supersedes "never paid out in cash" from ADR-018, only at close) |
| 10 | Rejected payout | Stays pending; support retries it |
| 11 | Late refund after close | An additional payout |
| 12 | Charged-off accounts | Closed with the same flow |
| 13 | New account after close | Allowed in the same currency, once closed with no debt |
| 14 | Simulator / load | Everything exposed; load includes closes |
| — | Payout destination | The customer gives the bank account in the close request |

## 4. Interpretations (builder's calls, for review)

- **I-1 States.** `pending_collateral → cancelled`; `active → closing → closed`. `closing` declines every authorization with a new reason `account_closing`, in the same priority slot as `account_closed`. Captures, reversals and refunds of authorizations approved before the request still work. Payments are accepted while debt is owed. New deposits are rejected.
- **I-2 Close timeline.**
  1. The request records who asked, the reason (support), the destination account and `close_requested_at` (account time).
  2. **Final statement**: billing closes an off-cycle final statement at the request time, with interest up to that day (grace rules apply) and a due date 21 days later. No new charges after it: no late fee, no block, no further interest.
  3. **Wait for in-flight work**: approved authorizations (until captured, reversed, or expired at 7 days), pending or settling payments, settling deposits, refunds and a pending charge-off.
  4. **Debt**: if receivable is > 0 once in-flight work is done, wait until the final due date. Payments are still accepted. Whatever is still owed on the due date (or at once if the account is already charged off) is applied from the collateral (`collateral_applied`, like charge-off).
  5. **Payout**: amount = remaining collateral + credit balance (max(−receivable, 0)). If it is 0, skip to closed. Otherwise create a payout intent and release the money only when the bank confirms.
  6. **Closed**: once the payout settles (or there was nothing to pay out), the state becomes `closed`. If debt remains after the collateral is exhausted, the account is closed with the debt outstanding.
- **I-3 Payout ledger.** The bank-confirmed release posts debit `collateral_held` (for the collateral part) and debit `receivable` (for the credit-balance part) / credit `cash_clearing`, as one journal `payout:<id>`. Collateral and credit limit drop to 0. Nothing posts before confirmation.
- **I-4 Payout states.** `pending → settling → settled | failed`. A `failed` payout can be retried by support (`POST …/payouts/{id}/retry`), which creates a new attempt with a new external reference. Money stays held while not settled. Only one open payout exists per account at a time.
- **I-5 Late refunds.** A refund that lands on a `closed` account creates a credit balance. A sweep creates an additional payout for it to the same destination. The account stays `closed`.
- **I-6 Destination account.** `{bank_code, account_number, holder_name}`, validated for shape only (digits, length 4–34). It is stored on the close request. Responses, events and logs only ever show `****1234`. The bank webhook body carries the full number to the simulated bank only, in the signed request from the simulator.
- **I-7 Reopen.** One account per (customer, currency) among accounts that are not `closed` or `cancelled`, or closed with debt outstanding. The existing unique constraint becomes a partial unique index.
- **I-8 Cancel.** Allowed only in `pending_collateral`. Pending deposits become `voided`. A later bank webhook for a voided deposit is rejected with 409 `deposit_voided`, and no money is recorded.

## 5. How it works (outline)

- **accounts**: new states and the close saga, driven by a per-minute sweep plus events (payment settled, auth resolved). Payouts mirror deposits: reserve → ledger → finalize, resumed by `ResolvePending`.
- **billing**: an off-cycle final statement (`cycle = n+1`, `final = true`). Catch-up stops scheduling closes, fees and blocks after it.
- **ledger**: `PostPayout`, the two-leg release, idempotent on `payout:<id>`.
- **gateway**:
  - customer: `POST /v1/accounts/{id}/close` `{destination}`, `POST /v1/accounts/{id}/cancel`, `GET /v1/accounts/{id}/payouts`;
  - support: `POST /v1/support/accounts/{id}/close` `{destination, reason}`, `POST /v1/support/accounts/{id}/payouts/{payoutId}/retry`;
  - bank, HMAC bound to the request: `POST /v1/webhooks/bank/payouts` `{payout_id, external_ref, amount_minor, currency, status}`.
- **simulator**: an "8 · Close" panel with close, cancel, payouts (settle, fail, retry) and the account state.

## 6. Test gates

- **Close in good standing** releases collateral + credit balance once the bank confirms; a failed payout keeps the money held, and a retry settles it.
- **Close with debt**:
  - a payment before the final due date → full collateral released;
  - nothing paid → the debt is applied from the collateral on the due date and the remainder is released;
  - debt above the collateral → closed with the remainder owed.
- **In flight**: a close with an approved authorization waits until it is captured or expires; a capture after the request still works, and a new authorization declines `account_closing`.
- **Concurrency**: close ×50 with the same key → one close; the payout webhook ×50 → one release; a concurrent close and purchase never approves the purchase.
- **Cancel**: voids deposits; a late webhook gives 409 and no ledger entry.
- **Late refund** → additional payout.
- **Reopen** after close works; reopen while debt is owed → 409.
- **Invariants**: Σ payouts settled = collateral + credit balance released; a closed account holds no collateral; no payout ledger entry without a settled payout; no closed account with in-flight work.
- **Load**: some iterations close their account and settle the payout.

## 6b. Builder's calls (pre-builder-check)

- **C-1 Final statement and the due-date wait use account time**, so the simulator's clock can fast-forward a close.
- **C-2 Support close** may set the destination when the customer never gave one. Without a destination, the payout waits, `awaiting_destination`, until the customer or support provides it.


## 6c. Builder-check answers (2026-10-25)

**Decided by the user**
- **U-1 Return window holds the payout.** A settled payment can still be returned for 5 days (ADR-018). The payout is created only once no payment of the account is inside its return window (`settled_at + 120h` has passed, in real time, like every return-window check), so money never leaves twice. Returns stay allowed on `closing` accounts; a return re-raises the debt and the close saga re-evaluates. Payments can no longer be returned once the payout exists (none is inside its window by construction).
- **U-2 Destination encrypted + instruction to the bank.** The full account number is stored encrypted (AES-256-GCM, key from `DESTINATION_KEY`; KMS in AWS, a dev key locally), plus `last4`. Creating a payout attempt emits a `payout.requested` instruction through the outbox carrying the attempt id, amount and the encrypted destination reference only (never the plain number). The simulated bank (simulator route) fetches the instruction through a bank-authenticated route (HMAC bound to method and path), which returns the plain number to the bank only. The bank's webhook echoes `destination_last4`, and accounts rejects a mismatch (422 `destination_mismatch`).

**Builder's calls**
- **B-1 Final statement after in-flight work.** The final statement is cut when in-flight work has resolved (≤ 7 days after the request: hold expiry), not at the request, so captures of earlier authorizations land on it. Interest accrues until then. Due date = its close + 21 days.
- **B-2 Delinquency while closing.** Until the final statement, billing runs normally (fees, block, charge-off at DPD 60). After it: no fees, blocks or interest. Earlier statements keep their own due dates. On the final due date the remaining debt is applied from the collateral, but if a payment received before that date is still pending, it waits for the bank's answer (as in ADR-019). An account already charged off skips the wait.
- **B-3 Deposits at close.** A close request voids `pending` deposits (a late settlement is rejected with 409 `deposit_voided`, no money recorded); `settling` deposits are waited for. Cancel answers 409 `deposit_settling` while a deposit is settling.
- **B-4 Payout attempts.** The platform issues the reference: each attempt id is its `external_ref`. The webhook names the attempt. `failed` is final for that attempt; a later `settled` for a failed attempt is rejected (409 `attempt_failed`). One open attempt per payout; a support retry creates the next attempt.
- **B-5 Reopen.** The partial unique index covers accounts not `closed`/`cancelled`. `CreateAccount` additionally refuses (409 `previous_account_has_debt`) while a closed account of the same customer and currency still has positive receivable (checked against the ledger at creation). It is allowed again once that debt is paid.

## 7. Left to the builder

- Table and column names, proto shapes, event payloads, pagination, simulator layout.
