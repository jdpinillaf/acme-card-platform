# Sprint 3 — Repayments, returned payments, spendable credit balance

Status: ready for user approval · 2026-10-25 · builds on Sprints 1–2 (`docs/DECISIONS.md` ADR-001…016)

## 1. Intent

Customers pay back what they spent, and a confirmed payment restores their available credit. A payment can fail before confirmation, which has no effect. It can also be returned after confirmation. A return raises the debt again and, if exposure ends up above the limit, restricts the card until the account is back in order.

The bank is never exposed beyond the confirmed collateral. Purchases draw first on the customer's own credit balance and then on the credit line.

## 2. Goals, non-goals, anti-goals

**Goals**
- Customer-initiated payments that copy the collateral-deposit flow: an intent through the customer API, then confirmation by an HMAC-signed bank webhook.
- Partial payments with a minimum of $1.00. The total of open payments is capped at the outstanding debt.
- Credit is restored only when the bank confirms (`settled`).
- Failed payments have no balance effect but are stored and audited.
- Returned payments within 5 days of settlement: the debt goes back up, and the card is auto-restricted (`payment_returned`) while exposure exceeds the limit.
- **Spendable credit balance (user decision, supersedes the Sprint 2 strict-limit rule):** purchases consume the credit balance first. With a balance of 700, a purchase of 500 leaves 200 and charges nothing.
- A simulator panel to create, settle, fail and return payments, with debt and credit balance visible.

**Non-goals (decided by the user)**
- Autopay. It comes in Sprint 4 with statements.
- Overpayment by payment. A credit balance only arises from refunds after paying.
- Cash-out of a credit balance. It is **never** returned in cash; it is only spent on purchases.
- Payments in a currency other than the account currency.
- Payment allocation to interest and fees. That is Sprint 4.
- Payment fees.

**Anti-goals**
- Signed exposure `Σ active holds + receivable` above the credit limit (= collateral) through any purchase path.
- Restoring credit before the bank confirms.
- A returned payment leaving the card spendable beyond collateral.
- The ledger and accounts disagreeing on any payment, as checked by `verify.sql`.

## 3. Decisions made by the user

| # | Topic | Decision |
|---|---|---|
| 1 | Payment flow | Copy the collateral-deposit flow: intent, then signed bank webhook |
| 2 | Who pays | Customer only, via JWT |
| 3 | Minimum | $1 |
| 4 | Currency | Account currency only (USD in practice) |
| 5 | Credit restore | Only when the bank confirms |
| 6 | Overpayment | Not via payments. A credit balance only from refunds after paying |
| 7 | Credit balance | **Spendable on purchases**, consumed before the credit line. Never cashed out |
| 8 | Failed payment | No balance effect |
| 9 | Returned payment | Accepted: debt increases, card restricted with reason `payment_returned` while over the limit |
| 10 | Simulator | Create, settle, fail and return payments |

## 4. Interpretations to confirm

- **I-1 Minimum.** $1 means `amount_minor ≥ 100` in the account currency.
- **I-2 Payment cap.** On creation, `amount ≤ max(receivable, 0) − Σ pending/settling payments`, so open payments can never sum to more than the debt. A refund landing between creation and settlement can still create a small credit balance at settlement. That is accepted, because the bank already moved the money.
- **I-3 Return window.** Returns are accepted within 5 days (120 h) of `settled_at`. Later returns get 409 `return_window_expired`.
- **I-4 Restriction.** `payment_returned` is a system restriction, separate from the customer's freeze. The customer cannot lift it. It lifts automatically once `Σ active holds + receivable ≤ limit` again, normally after a later payment settles. In the decline order it goes right after `currency_not_supported` and before `card_frozen`.

## 4b. Builder's calls after design review
- **C-1 Lifting the restriction.**
  - Settling a payment re-checks exposure in the same transaction and lifts the restriction at once when exposure is back within the limit.
  - Every other path that lowers exposure is covered by a re-evaluation sweep that runs each minute in the accounts worker loop: releases, expiries, reversals, refunds and deposits that raise the limit. Lifting a little later than the event is acceptable there.
  - The invariant checks "restricted only while over the limit" with a 5-minute grace period.
- **C-2 Payments the bank never answers.** A payment with no answer expires after 7 days (`pending → expired`) and stops counting toward the cap. There is no customer cancel endpoint in this sprint.

## 5. Constraints

Same as Sprints 1–2:
- Go services and gRPC inside.
- REST plus HMAC webhooks at the gateway.
- Int64 minor units.
- Reserve → ledger → finalize sagas with the reconciler.
- Outbox → audit.
- TDD with the concurrency gate.
- Everything in English.
- Prompts logged in `PROMPTS.md`.

## 6. How it works

### 6.1 Ledger
- **Spendable credit balance.** The strict-limit rule changes to the signed form:
  - `available = max(0, limit − receivable − Σ active holds)`, where receivable is negative for a credit balance;
  - PlaceHold approves iff `Σ active holds + receivable + amount ≤ limit`.
  - The Sprint 2 test that pinned "credit balance not spendable", and the `max(receivable,0)` invariant, are updated to the new rule.
- **New RPC `PostPayment(account_id, reference, amount)`**: debit `cash_clearing` / credit `receivable`, idempotent on reference (`payment:<id>`).
- **New RPC `PostPaymentReturn(account_id, reference, amount)`**: debit `receivable` / credit `cash_clearing` (`payment_return:<id>`).
- **`GetBalances`** also returns `receivable_minor` (signed), so accounts can show debt and credit balance.

### 6.2 Accounts
- **Table `payments`**, mirroring `deposits`:
  - `pending → settling → settled | failed`, then `settled → returning → returned`;
  - `external_ref` unique, `return_ref` unique, `idempotency_key_hash` unique;
  - timestamps for creation, settling, settlement and return.
- **`CreatePayment`** (customer): requires an active account, enforces I-1 and I-2, and emits `payment.created`.
- **`SettlePayment`** (bank webhook): `settled` runs reserve → ledger `PostPayment` → finalize and emits `payment.settled`. `failed` simply stores the failure and emits `payment.failed`. After a settlement, the restriction is lifted if exposure is back within the limit.
- **`ReturnPayment`** (bank webhook): checks the I-3 window, then reserve → ledger `PostPaymentReturn` → finalize, and emits `payment.returned`. If exposure is above the limit, it sets `restricted_reason='payment_returned'` and emits `card.restricted`.
- **Account fields:** `restricted_reason`, plus derived `outstanding_minor = max(receivable,0)` and `credit_balance_minor = max(−receivable,0)`. Available credit is no longer capped at the credit limit.
- **Decline evaluation** gains `payment_returned`, at the position given in I-4.
- **`ResolvePending`** resumes `settling` and `returning` payments, exactly like deposits.

### 6.3 Gateway
- Customer: `POST /v1/accounts/{id}/payments` with `{amount_minor}` (JWT + Idempotency-Key) → 201; `GET /v1/accounts/{id}/payments`.
- Bank, HMAC with the bank secret, bound to method and path:
  - `POST /v1/webhooks/bank/payments` with `{payment_id, external_ref, amount_minor, currency, status}`;
  - `POST /v1/webhooks/bank/payments/{id}/return` with `{return_ref, amount_minor}`.
- Account JSON gains `outstanding_minor`, `credit_balance_minor` and `restricted_reason`.

### 6.4 Simulator
- A "6 · Payments" panel:
  - create a payment;
  - list payments with Settle, Fail and Return buttons;
  - show debt, credit balance and restriction.
- The simulator exposes every action the APIs offer (Q-B), including payment return and the restriction state.

## 7. Test gates
- **Same payment, many times.** The same `Idempotency-Key` ×50 creates one payment. The same bank webhook ×50 settles once (one journal entry).
- **Payment cap.** Concurrent payment creation never exceeds the debt (I-2).
- **Credit restored only on confirmation.** Before settlement, available is unchanged; after it, available rises by the amount. A failed payment changes nothing.
- **Returns.** A return within 5 days raises the debt, and a return after that gets 409. If exposure exceeds the limit, the card is restricted and authorizations decline `payment_returned`. The next settled payment lifts the restriction.
- **Spendable credit balance.**
  1. With a credit balance of 700 and a limit of 1000, available is 1700.
  2. A 500 purchase captured leaves a 200 credit balance and no debt.
  3. Concurrent purchases never push `holds + receivable` above the limit.
- **Reconciler.** Lost-response tests for `settling` and `returning` payments.
- **Invariants.** `verify.sql` gains:
  - payments ↔ ledger journals, in both directions;
  - returns ↔ return journals;
  - signed exposure ≤ limit, except overage covered by returns (Q-A);
  - over-limit accounts are restricted;
  - restricted accounts are still over the limit, or the restriction lifted within the 5-minute grace period.
- **Load.** The k6 flow gains pay → settle.

## 7b. Resolved by the user (2026-10-25)
- **Q-A: accept the risk.** Credit is restored as soon as a payment settles. A later return can push the debt above the collateral; the bank accepts that exposure. The card stays restricted (`payment_returned`) until exposure is back within the limit.
  - **Consequence for invariants.** "Signed exposure ≤ limit" applies to every account **except** those with a returned payment on file whose overage is covered by the returned amount (`exposure − limit ≤ Σ returned`). Those must instead be restricted.
  - **Purchases still never push exposure over the limit.** PlaceHold enforces it unconditionally.
- **Q-B: every action available in the simulator.** The phrase meant that all possible actions must exist. The simulator exposes every operation in the system: create, settle, fail and return payments, plus everything from Sprints 1–2. It is not about outbound notifications.

## 8. Left to the builder
- Table and column names.
- Proto shapes.
- Pagination of payments.
- How the restriction is stored.
- Event payloads.
- Simulator layout.
