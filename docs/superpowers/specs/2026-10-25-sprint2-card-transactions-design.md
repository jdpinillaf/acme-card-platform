# Sprint 2 — Card transactions: authorizations, captures, reversals, refunds, declines

Status: ready for user approval · 2026-10-25 · builds on Sprint 1 (`docs/DECISIONS.md` ADR-001…013)

## 1. Intent

Sprint 1 made a card spendable: confirmed collateral sets a credit limit. Sprint 2 lets the card **spend**. A purchase is authorized against available credit, captured, possibly reversed or refunded, and every refusal carries an explicit reason.

The non-negotiable promise of a secured card is: **a purchase can never exceed the credit limit, which equals the confirmed collateral.** Every design choice below serves that promise under concurrency, retries and duplicate network messages.

Success looks like this:
- Concurrent authorizations against one card never push holds plus spend past the limit, whatever the interleaving.
- The same network message delivered N times has exactly one effect.
- Every declined attempt is stored, audited and answered with a reason.

## 2. Goals, non-goals, anti-goals

**Goals**
- Authorize, capture, reverse and refund card purchases, driven by a simulated card network over HMAC-signed endpoints.
- Holds expire automatically after 7 days.
- Six decline reasons, evaluated in a fixed order and returned as handled outcomes.
- Customer controls: freeze and unfreeze, plus a per-transaction purchase limit.
- Velocity limit of 10 purchase attempts per minute per card.
- A credit balance (saldo a favor) when a refund exceeds what is owed.
- Simulator panel acting as merchant and network.

**Non-goals for Sprint 2 (decided by the user)**
- Partial approvals.
- Incremental authorizations.
- Multiple or partial captures per authorization.
- Captures without an authorization.
- Refunds without an original captured purchase.
- Currency conversion.
- Outbound notifications. They move to a later sprint of their own.
- Overdraft. **It is removed from the product entirely, not deferred.**

**Anti-goals: failure even if "it works"**
- Any state where `holds + max(receivable, 0) > credit_limit` for an account.
- A declined attempt that leaves no stored, audited record.
- A duplicate network message that creates a second hold, capture or refund.
- Available credit shown above the credit limit, even with a credit balance.

## 3. Decisions made by the user

| # | Topic | Decision |
|---|---|---|
| 1 | Who authorizes | A simulated card network calls HMAC-signed endpoints, same scheme as the bank webhook |
| 2 | Hold lifetime | 7 days, then auto-release |
| 3 | Partial approval | No. Insufficient credit declines the whole amount |
| 4 | Incremental auth | No |
| 5 | Merchant data | Merchant id, name and category (MCC) |
| 6 | Foreign currency | No conversion to the account currency |
| 7 | Capture amount | Equal to or less than authorized |
| 8 | Captures per auth | Exactly one. No partial captures |
| 9 | Capture without auth | Not allowed |
| 10 | Reversals | Allowed within 3 days of the transaction |
| 11 | Refunds | Only against a captured purchase |
| 12 | Refund without original | Not allowed |
| 13 | Refund after the customer paid | Leaves a credit balance on the card |
| 14 | Decline order | `account_closed` → `collateral_not_confirmed` → `card_frozen` → `velocity_limit_exceeded` → `purchase_limit_exceeded` → `insufficient_credit` |
| 15 | Velocity | 10 purchase attempts per minute per card |
| 16 | Purchase limit | Per purchase transaction |
| 17 | Freeze | Moves into Sprint 2 |
| 18 | Decline response | Handled declines answer **201** with `status: declined` and the reason |
| 19 | Notifications | Moved to a later sprint |
| 20 | Overdraft | Removed from the product |

## 4. Interpretations (confirmed by the user)

All five were confirmed by the user on 2026-10-25.

- **I-1 Foreign currency (6).** A purchase in a currency different from the account currency is **declined** with a new reason, `currency_not_supported`. It is checked right after `collateral_not_confirmed` and before `card_frozen`.
- **I-2 Reversal window (10).** A reversal voids an **uncaptured** authorization and releases its hold. It is accepted only within 3 days of the authorization. After 3 days the network gets 409 `reversal_window_expired`; the hold still expires on its own at day 7. A captured purchase is undone with a refund, not a reversal.
- **I-3 Refund amounts (11).** Several partial refunds are allowed against one captured purchase, as long as their sum stays at or below the captured amount.
- **I-4 Velocity counting (15).** Every authorization attempt counts toward the 10 per minute, including declined ones. There is no daily cap.
- **I-5 Purchase limit (16).** Set by the customer. `0` means no customer cap. It can never exceed the credit limit.

## 4b. Builder's calls after design review

These were settled by the builder because each has a conventional answer. The user can override any of them.

- **C-1 Velocity is serialized per card.** The velocity counter locks the card's window row, as the Sprint 1 rate limiter does, so exactly 10 attempts per minute get through even under concurrency. The limit-never-exceeded test therefore stays within the velocity budget: 10 concurrent authorizations of 25% of the limit give 4 approvals and 6 `insufficient_credit`. A separate test fires 50 concurrent attempts and expects exactly 10 to pass velocity. The k6 scenario uses one card per virtual user.
- **C-2 Credit balance before repayments exist.** The money model supports a negative receivable from Sprint 2. No Sprint 2 path can push it below zero, because refunds are capped per purchase and repayments arrive in Sprint 3. So in Sprint 2 the credit-balance rule is tested at ledger level with a fixture, and end to end in Sprint 3. Repayments are not pulled forward.
- **C-3 Follow-up messages on frozen or closed cards.**
  - Freezing blocks only **new** authorizations. Captures, reversals and refunds on purchases approved before the freeze still go through, as card networks require.
  - On a `closed` account, reversals and refunds are accepted because they reduce exposure, and captures are refused with 409 `account_closed`. Sprint 5 will release active holds when closing.
- **C-4 `network_ref` uniqueness.** Unique per message type: the pair (type, network_ref). A capture may reuse its authorization's reference without colliding. The same ref and type with an identical body replays. A different body returns 422 `network_ref_reuse`, mirroring ADR-004.

## 5. Constraints

Everything Sprint 1 established carries over:
- Go services and gRPC inside, REST at the gateway.
- Postgres with one schema per service.
- Int64 minor units.
- Insert-first idempotency.
- Outbox to SNS to SQS to the append-only audit.
- TDD with the concurrency test as the primary gate.
- LocalStack locally, Terraform describing AWS but never applied.
- Everything in English.
- Prompts logged in `PROMPTS.md`.

## 6. How it works

### 6.1 Ownership (builder's call, explained)

- **accounts** owns policy and the transaction records:
  - account state, frozen flag, purchase limit and currency;
  - authorization, capture, reversal and refund rows;
  - decline evaluation and velocity.
- **ledger** owns money:
  - it is the only place a hold is placed or money is posted;
  - it performs the final `available >= amount` check under a per-account lock, so the limit can never be exceeded even if policy checks race.
- **No new service.** Authorizations live in accounts, holds in ledger.

### 6.2 Money model in the ledger

- **Holds.** New ledger table `holds` with `active`, `captured`, `released` or `expired`. Holds are reservations, not postings.
- **Available credit (user decision: strict limit, a credit balance is not spendable).**
  - `available = credit_limit − max(receivable, 0) − Σ active holds`, floored at 0.
  - `receivable` may go **negative**. A negative receivable is the customer's credit balance.
  - The credit balance never raises spending power. It offsets the next statement (Sprint 4) or is returned on close (Sprint 5).
  - The locked check is `Σ active holds + max(receivable, 0) + amount ≤ credit_limit`.
- **Capture.** One ledger transaction that marks the hold `captured` and posts `debit receivable / credit cash_clearing` for the captured amount. Whatever was authorized but not captured is released by that same step.
- **Refund.** Posts `debit cash_clearing / credit receivable`.
- **Reversal and expiry.** Mark the hold `released` or `expired`. Nothing is posted.
- **Serializing the money check.** All of these lock the account's `collateral_held` balance row, which acts as the per-account mutex. Concurrent authorizations are therefore checked one at a time against a consistent available figure.

### 6.3 Network API (HMAC-signed, new secret `NETWORK_WEBHOOK_SECRET`)

The account id doubles as the card token (builder's call: Sprint 1 has no card numbers). Every message carries a unique `network_ref`. A repeated `network_ref` replays the original response.

| Endpoint | Body | Result |
|---|---|---|
| `POST /v1/network/authorizations` | `network_ref, card_id, amount_minor, currency, merchant{id,name,mcc}` | 201 `approved` with hold, or 201 `declined` with `decline_reason` |
| `POST /v1/network/authorizations/{id}/capture` | `network_ref, amount_minor` (≤ authorized) | 201 `captured`. 409 if already captured, reversed or expired |
| `POST /v1/network/authorizations/{id}/reversal` | `network_ref` | 200 `reversed`. 409 `reversal_window_expired` after 3 days, or if captured |
| `POST /v1/network/authorizations/{id}/refunds` | `network_ref, amount_minor` | 201 `refunded`. 409 `refund_exceeds_captured`. 409 if not captured |

Decline evaluation order, first match wins:
1. `account_closed`
2. `collateral_not_confirmed`
3. `currency_not_supported` (I-1)
4. `card_frozen`
5. `velocity_limit_exceeded`
6. `purchase_limit_exceeded`
7. `insufficient_credit`

The last one comes from the ledger's locked check. Unknown `card_id` → 404 (not a card, so not a handled decline). Validation errors → 400.

### 6.4 Customer API (JWT, authorized account only, `Idempotency-Key` required on writes)

- `POST /v1/accounts/{id}/freeze` and `POST /v1/accounts/{id}/unfreeze`. Freezing a closed or already-frozen card is idempotent.
- `PUT /v1/accounts/{id}/limits` with `{ "purchase_limit_minor": n }`. Range: 0 to the credit limit.
- `GET /v1/accounts/{id}/transactions`, newest first, paginated. Each item shows authorization status, amounts, merchant and decline reason.

### 6.5 Background work

An expiry worker releases `active` holds older than 7 days. It runs every minute with `SKIP LOCKED`, so several replicas can run it safely. Each expiry emits an event.

### 6.6 Events (outbox → audit)

- `authorization.approved` and `authorization.declined` (with reason)
- `authorization.captured`, `authorization.reversed`, `authorization.expired`
- `refund.posted`
- `card.frozen`, `card.unfrozen`
- `limits.updated`

Each event carries before and after snapshots, as in Sprint 1.

### 6.7 Simulator

A new "Merchant / network" panel:
- authorize with merchant fields;
- capture, reverse and refund buttons per transaction;
- concurrent sends, including a burst that exceeds the limit;
- freeze and unfreeze and a purchase limit control;
- a transactions table.

## 7. Test gates (must pass before Sprint 2 is done)

- **Limit never exceeded.** 10 concurrent authorizations of 25% of the limit each on one card: exactly 4 approve, 6 decline `insufficient_credit`, and `holds + max(receivable, 0) ≤ credit_limit` at the end (see C-1).
- **One effect per network message.** The same `network_ref` ×50 gives one authorization. The same holds for captures and refunds.
- **Competing operations.** Concurrent capture vs reversal of one authorization: exactly one wins. Concurrent refunds that together exceed the captured amount: the sum never exceeds it.
- **Decline order.** Each reason is reproducible in isolation, and every combination returns the highest-precedence reason.
- **Velocity.** The 11th attempt within a minute is declined, and declined attempts count.
- **Lifecycle.** Expiry at 7 days and the 3-day reversal window, both tested with an injectable clock.
- **Credit balance.** With a negative receivable, available never exceeds `credit_limit − holds`. In Sprint 2 this is tested with a ledger fixture; end to end in Sprint 3 (see C-2).
- **Security.** Unsigned or stale network messages → 401. Foreign JWT on freeze or limits → 403.
- **Load.** The k6 scenario gains authorize → capture → partial refund. The SQL invariants gain:
  - `holds + max(receivable, 0) ≤ credit_limit` per account;
  - one capture per authorization;
  - Σ refunds ≤ captured.

## 8. Left to the builder

- Table and column names.
- Proto message shapes.
- Pagination style.
- Worker interval.
- How the clock is injected for tests.
- Internal error reasons beyond the ones named here.
- Simulator layout.
