# Sprint 3 — Repayments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Customers can pay their debt, and the bank confirms, fails or returns each payment. A credit balance is spent before the credit line. A returned payment that leaves exposure above the limit restricts the card until it is back in order.

**Architecture:**
- accounts owns payments and the restriction.
- ledger posts payments and returns.
- Every bank-driven step runs as a reserve → ledger → finalize saga, with the reconciler (ADR-016) covering lost responses.
- The ledger's purchase rule becomes signed: `holds + receivable + amount ≤ limit`.

**Tech Stack:** Go 1.26, gRPC/protobuf (buf), pgx v5, chi, testcontainers, k6, and Next.js 16 for the simulator.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint3-repayments-design.md`. Read §3, §4, §4b and §7b.

## Global Constraints

**Payments**
- The minimum payment is `amount_minor ≥ 100`, in the account currency only.
- Payment cap at creation: `amount ≤ max(receivable, 0) − Σ amount of payments in (pending, settling)`. If the result is ≤ 0, return 409 `nothing_to_pay`. Above it, return 409 `payment_exceeds_balance`.
- Credit is restored only when the bank confirms (`settled`). A `failed` payment has no balance effect.
- A payment with no bank answer expires after 7 days (`PaymentTTL = 7*24h`).
- Returns are accepted only within 120 h of `settled_at` (`ReturnWindow = 120*time.Hour`). Later ones get 409 `return_window_expired`.

**Ledger rule (supersedes Sprint 2, ADR-014)**
- Available credit: `available = max(0, limit − receivable − Σ active holds)`. Receivable is signed: negative means a credit balance.
- PlaceHold approves iff `Σ active holds + receivable + amount ≤ limit`, where limit is the collateral.
- Available may exceed the credit limit when there is a credit balance.

**Restriction**
- `restricted_reason='payment_returned'` is set when a return leaves `holds + receivable > credit_limit`.
- It is lifted:
  - immediately by a payment settlement that brings exposure within the limit;
  - otherwise by the per-minute re-evaluation sweep.
- The customer cannot lift it.

**Declines**
- Order, first match wins:
  1. `account_closed`
  2. `collateral_not_confirmed`
  3. `currency_not_supported`
  4. `payment_returned`
  5. `card_frozen`
  6. `velocity_limit_exceeded`
  7. `purchase_limit_exceeded`
  8. `insufficient_credit`

**API**
- Bank payment webhooks are HMAC-signed with `BANK_WEBHOOK_SECRET`, with method and path bound (`webhook.SignRequest`).
- Customer writes require JWT + `Idempotency-Key`, on an authorized account.
- Events:
  - `payment.created`, `payment.settled`, `payment.failed`, `payment.expired`, `payment.returned`
  - `card.restricted`, `card.unrestricted`
  - `ledger.payment_posted`, `ledger.payment_returned`

**Process**
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Every prompt is appended to `PROMPTS.md`.
- TDD: write the failing test first.

## Review Focus

1. **A return racing a new purchase on the same account.** The purchase must be judged against the post-return exposure, or the return must restrict before the next approval, so that no purchase is ever approved when `holds + receivable + amount > limit`. Covered by a test in Task 6.
2. **A payment settling while a refund lands.** It may create a credit balance, and available must never go negative or wrong. Covered by a test in Task 5.
3. **Several pending payments created concurrently.** Their sum never exceeds the debt. Covered by a test in Task 4.
4. **A return on a payment whose restriction was already lifted.** The return must restrict the card again. Covered by a test in Task 6.
5. **A credit balance with an active hold expiring.** Available goes back to `limit − receivable`. Covered by a test in Task 2.

---

### Task 1: Contracts

**Files:**
- `proto/acme/common/v1/common.proto` (`DECLINE_REASON_PAYMENT_RETURNED = 8`)
- `proto/acme/ledger/v1/ledger.proto`
- `proto/acme/accounts/v1/accounts.proto`
- `services/ledger/ledgertest/ledgertest.go` (adapter methods)

**Ledger additions:**
- `rpc PostPayment(PostPaymentRequest{account_id, reference, amount_minor, meta}) returns (PostJournalResponse)`
- `rpc PostPaymentReturn(PostPaymentRequest) returns (PostJournalResponse)`
- `GetBalancesResponse.receivable_minor` (signed)

**Accounts additions:**
- `Account`: `outstanding_minor`, `credit_balance_minor`, `restricted_reason`.
- `PaymentStatus` {PENDING, SETTLING, SETTLED, FAILED, EXPIRED, RETURNING, RETURNED}.
- `Payment{id, account_id, Money amount, status, external_ref, return_ref, created_at, settled_at, returned_at}`.
- `rpc CreatePayment(CreatePaymentRequest{account_id, amount_minor, meta}) returns (PaymentResponse{payment, account, replayed})`
- `rpc SettlePayment(SettlePaymentRequest{payment_id, external_ref, amount_minor, currency, DepositStatus-like status SETTLED|FAILED, meta}) returns (PaymentResponse)`. Reuse the `PaymentStatus` values `SETTLED` and `FAILED` in the request.
- `rpc ReturnPayment(ReturnPaymentRequest{payment_id, return_ref, amount_minor, meta}) returns (PaymentResponse)`
- `rpc ListPayments(ListPaymentsRequest{account_id, page_size, page_token}) returns (ListPaymentsResponse{payments, next_page_token})`

- [ ] **Step 1:** Edit the protos, then run `buf lint && buf generate`. Add the 2 ledgertest adapter methods.
- [ ] **Step 2:** Run `go build ./... && go test ./pkg/... ./services/...`. Expected: PASS.
- [ ] **Step 3:** Commit `feat(proto): sprint 3 payment contracts`.

### Task 2: Ledger — spendable credit balance, payments, returns

**Files:**
- `services/ledger/internal/ledger/holds.go` (`availableOf`, the PlaceHold check)
- `services/ledger/internal/ledger/payments.go` (new)
- `service.go` (GetBalances `receivable_minor`)
- Tests:
  - `holds_integration_test.go`: replace `TestRefundCanMakeReceivableNegativeButNotRaiseAvailable` with `TestCreditBalanceIsSpendable`;
  - `payments_integration_test.go`.

**Interfaces (Produces):** `(*Service).PostPayment` and `PostPaymentReturn`. Both lock the per-account mutex and use `postJournalTx`.
- Payment: `debit cash_clearing / credit receivable`, `entry_type "payment_settled"`, event `ledger.payment_posted` (none on replay).
- Return: `debit receivable / credit cash_clearing`, `entry_type "payment_returned"`, event `ledger.payment_returned`.

- [ ] **Step 1: Failing tests:**
  - `TestCreditBalanceIsSpendable`: fund 1000, hold and capture 200, refund 200 + a fixture refund of 700. Receivable is −700 and available is 1700. A hold of 1700 approves; a further hold of 1 declines. Release, then capture 500 of the 1700 hold: receivable becomes −200 and the credit balance 200.
  - `TestHoldExpiryWithCreditBalance` (Review Focus 5): receivable −300 with a 500 hold gives available 800. Releasing the hold gives 1300.
  - `TestPostPaymentRestoresAvailable`: capture 600 gives available 400. Paying 600 gives receivable 0 and available 1000. Replay is the same and posts nothing.
  - `TestPostPaymentReturnRaisesDebtBeyondLimit`: after paying 600, spending 1000 and returning 600, receivable is 1600 and available 0. Ledger balanced.
  - `TestPaymentEvents`: one event per post, none on replay.
- [ ] **Step 2:** Run `go test -race -tags=integration ./services/ledger/...`. Expected: FAIL.
- [ ] **Step 3:** Implement. `availableOf(limit, holds, receivable) = max(0, limit-receivable-holds)`. The PlaceHold check is `holds+receivable+amount > limit` → decline.
- [ ] **Step 4:** Run the same command. Expected: PASS, with all earlier ledger tests green (update their expectations only where they encoded the superseded rule, and note each one in the ledger).
- [ ] **Step 5:** Commit `feat(ledger): spendable credit balance, payment and return postings`.

### Task 3: Accounts — schema, account fields, uncapped available

**Files:**
- `services/accounts/migrations/000008_payments.{up,down}.sql`
- `service.go`: `available()` no longer capped; account proto gets outstanding, credit balance and restriction.
- Test: extend `service_integration_test.go`.

**Schema:**
- `payments`:
  - `id uuid pk, account_id FK, amount_minor ≥ 100, currency`;
  - `status CHECK in (pending,settling,settled,failed,expired,returning,returned)`;
  - `external_ref UNIQUE, return_ref UNIQUE, idempotency_key_hash UNIQUE (partial)`;
  - `created_at, settling_at, settled_at, returning_at, returned_at`.
- Indexes:
  - `(account_id, created_at desc)`;
  - `(created_at) WHERE status='pending'`;
  - `(settling_at) WHERE status='settling'`;
  - `(returning_at) WHERE status='returning'`.
- `accounts.restricted_reason text CHECK in ('payment_returned')`, plus `restricted_at`.

- [ ] **Step 1:** Failing test `TestAccountShowsOutstandingAndCreditBalance`, using a ledger fixture:
  - capture 300 → outstanding 300, credit balance 0;
  - refund 300 and fixture 100 → outstanding 0, credit balance 100, available 1100 (above the credit limit).
- [ ] **Step 2:** Run it. Expected: FAIL.
- [ ] **Step 3:** Implement the migration and fields. `available()` returns ledger `available_minor`.
- [ ] **Step 4:** Run all accounts integration tests. Expected: PASS (update the Sprint 1–2 expectations that assumed the cap; ledger each one).
- [ ] **Step 5:** Commit `feat(accounts): payments schema, outstanding and credit balance on accounts`.

### Task 4: CreatePayment, ListPayments, payment expiry

**Files:** `services/accounts/internal/accounts/payments.go`, `payments_integration_test.go`.

**Interfaces (Produces):**
- `(*Service).CreatePayment` and `ListPayments`, which follows the `ListTransactions` keyset style.
- `(*Service).ExpirePayments(ctx, batch) (int, error)`, called from `RunExpiry`.
- `PaymentTTL`, `ReturnWindow`, `MinPaymentMinor = 100`.

**Rules:**
- The account must be active; a closed account gets 409 `account_closed`, a pending one 409 `collateral_not_confirmed`.
- Lock the account row `FOR UPDATE` to serialize cap checks. Dedupe on `idempotency_key_hash` as deposits do.

- [ ] **Step 1: Failing tests:**
  - `TestCreatePaymentMinimumAndCap`: debt 500 → 99 gives `InvalidArgument invalid_amount`, 600 gives `payment_exceeds_balance`, 300 is ok, then 201 gives `payment_exceeds_balance`.
  - `TestCreatePaymentNothingToPay`: debt 0 → `nothing_to_pay`.
  - `TestConcurrentPaymentsNeverExceedDebt` (Review Focus 3): debt 1000, 10 concurrent payments of 300 → exactly 3 succeed.
  - `TestCreatePaymentDedupesOnKeyHash`.
  - `TestExpirePaymentsAfter7Days`: `Advance(7d+1s)` → expired, and the cap is freed.
  - `TestListPaymentsNewestFirst`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): customer payment intents with cap, expiry and listing`.

### Task 5: SettlePayment saga and restriction lift

**Files:** `payments.go`, `restriction.go`, `expiry.go` (ResolvePending), tests.

**Rules:**
- The flow mirrors `SettleDeposit`:
  1. reserve `pending → settling` and claim `external_ref` (a reused ref gives 409 before the ledger call);
  2. post ledger `PostPayment("payment:<id>")` on a detached context;
  3. finalize `settled`.
- `failed` goes `pending → failed` with no ledger call.
- An amount or currency mismatch gives 400 and the payment stays pending.
- Replay by `external_ref` is supported.
- After finalize, if the account is restricted and `holds + receivable ≤ credit_limit`, clear the restriction and emit `card.unrestricted`, in the same transaction.
- `ResolvePending` resumes payments stuck in `settling` for longer than `PendingTimeout`.

- [ ] **Step 1: Failing tests:**
  - `TestSettlePaymentRestoresCreditOnlyOnConfirmation`: available is unchanged after create and rises after settle.
  - `TestFailedPaymentHasNoEffect`.
  - `TestSettlePaymentSameWebhookTwiceOnce`: 50 concurrent identical webhooks → one journal entry.
  - `TestSettlePaymentReusedExternalRefTouchesNothing`.
  - `TestSettleWhileRefundLands` (Review Focus 2): settle 500 on a debt of 500 while a 200 refund posts concurrently → final receivable −200, available = limit + 200, and the ledger is balanced.
  - `TestSettlingPaymentResumedByReconciler`, using the `flakyLedger` with a new `failPayment` flag.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run them. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): bank payment settlement saga with restriction lift`.

### Task 6: ReturnPayment, restriction, decline, sweep

**Files:** `payments.go`, `restriction.go`, `decline.go` (+ `decline_test.go`), `authorize.go` (decline input), `expiry.go` (sweep in `RunExpiry`), tests.

**Interfaces (Produces):**
- `DeclinePaymentReturned DeclineReason = "payment_returned"`.
- `DeclineInput.Restricted bool`.
- `(*Service).ReevaluateRestrictions(ctx, batch) (int, error)`.

**Rules:**
- Return flow:
  1. reserve `settled → returning` and claim `return_ref` (the window is checked against `settled_at`; the amount must equal the payment amount);
  2. post ledger `PostPaymentReturn("payment_return:<id>")`;
  3. finalize `returned`.
- In the same finalize transaction, if `holds + receivable > credit_limit`, set the restriction and emit `card.restricted`.
- Authorize reads `restricted_reason` inside the reservation transaction (it is already `FOR SHARE`), so a committed restriction is honored.
- The PlaceHold signed check independently guarantees no approval over the limit.

- [ ] **Step 1: Failing tests:**
  - `TestEvaluateDeclinePaymentReturnedPrecedence` (unit): a restricted, frozen card → `payment_returned`; a closed one → `account_closed`.
  - `TestReturnWithin5DaysRestrictsWhenOverLimit`: spend 1000, pay 1000, spend 1000, return → restricted, and authorize declines `payment_returned`. Paying 1000 and settling lifts it.
  - `TestReturnWindowExpired`: `Advance(120h+1s)` → `return_window_expired`.
  - `TestReturnUnderLimitDoesNotRestrict`.
  - `TestReturnAfterLiftRestrictsAgain` (Review Focus 4).
  - `TestReturnRacingPurchaseNeverOverLimit` (Review Focus 1): concurrent return and authorize ×25 → no approval leaves `holds + receivable > limit` unless caused by the return itself, and the restriction is set whenever exposure exceeds the limit.
  - `TestReevaluateLiftsAfterHoldRelease`: a restricted account whose exposure drops via reversal → the sweep lifts it.
  - `TestReturningPaymentResumedByReconciler`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement, and wire `ExpirePayments` and `ReevaluateRestrictions` into `RunExpiry`.
- [ ] **Step 4:** Run all accounts tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): payment returns, payment_returned restriction and decline`.

### Task 7: Gateway

**Files:**
- `services/gateway/internal/server/payments.go`
- Modify `server.go` (routes, account JSON fields) and `network.go` (decline map)
- Test: `payments_test.go`

**Routes:**
- `POST /v1/accounts/{id}/payments` with `{amount_minor}` → 201, `{payment, account}`.
- `GET /v1/accounts/{id}/payments` → `{payments, next_page_token}`.
- `POST /v1/webhooks/bank/payments` with `{payment_id, external_ref, amount_minor, currency, status}` → 200. The signature is request-bound with the bank secret.
- `POST /v1/webhooks/bank/payments/{paymentID}/return` with `{return_ref, amount_minor}` → 200.
- The account JSON gains `outstanding_minor`, `credit_balance_minor` and `restricted_reason` (omitempty). The decline map gains `payment_returned`.

- [ ] **Step 1: Failing tests:**
  - `TestCreatePaymentRoute` (strict JSON, 201);
  - `TestPaymentWebhooksRequireBoundSignature` (a deposit-style signature → 401);
  - `TestReturnRouteShape`;
  - `TestAccountJSONHasBalances`;
  - `TestDeclinePaymentReturnedJSON`.
- [ ] **Step 2:** Run `go test -race ./services/gateway/...`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(gateway): payment routes and bank payment webhooks`.

### Task 8: End to end, load, invariants

**Files:** `tests/integration/sprint3_test.go`, `tests/load/flows.js`, `tests/load/verify.sql`.

- [ ] **Step 1:** Write `TestSprint3EndToEnd` through HTTP:
  - spend → pay → settle → available restored;
  - the same webhook ×50 → one settlement;
  - credit balance spent on a purchase;
  - return → restricted → `payment_returned` decline → pay → unrestricted;
  - fail has no effect;
  - unsigned payment webhook → 401;
  - audit receives the `payment.*` and `card.restricted|unrestricted` events.
- [ ] **Step 2:** Run `go test -race -tags=integration -run Sprint3 ./tests/integration/`. Expected: PASS once all routes are wired.
- [ ] **Step 3:** In k6, after the Sprint 2 steps, pay the outstanding debt, settle it, and check available.
- [ ] **Step 4:** Add to `verify.sql`:
  - payment journals ↔ settled/returned payments, in both directions;
  - return journals ↔ returned payments;
  - signed exposure ≤ limit, except overage ≤ Σ returned (spec §7b);
  - accounts over the limit that are not restricted;
  - accounts restricted while within the limit for more than 5 minutes;
  - payments stuck in `settling` or `returning` for more than 5 minutes.

  Replace the Sprint 2 row `holds + max(receivable,0) > limit` with the signed version.
- [ ] **Step 5:** Verify the new rows fire on corrupted data, in a rolled-back transaction.
- [ ] **Step 6:** Run `make up && make load`. Expected: 0 failed requests and 0 violations.
- [ ] **Step 7:** Commit `test: sprint 3 end-to-end, load flow and invariants`.

### Task 9: Simulator — every action available

**Files:**
- `web/app/api/bank/payments/route.ts` (signs the request-bound bank secret; ops `settle|fail|return`, guarded)
- `web/lib/payments.ts` + `payments.test.ts` (`paymentWebhookPath(op, id?)`, allow-listed)
- `web/app/page.tsx`

**Panel:**
- "6 · Payments": create a payment; a list with Settle, Fail and Return per row; amount and ref inputs.
- The account panel shows outstanding, credit balance and the restriction badge.

**Coverage:** every API action is reachable from the UI. The test is a checklist assertion in `web/lib/actions.test.ts`: it lists the gateway routes from a shared table and asserts that each has a UI action id.

- [ ] **Step 1:** Write the failing tests: `paymentWebhookPath` builds only `/v1/webhooks/bank/payments` and `/v1/webhooks/bank/payments/{id}/return`; the actions table covers every route.
- [ ] **Step 2:** Run `cd web && npm test`. Expected: FAIL.
- [ ] **Step 3:** Implement the helper, the route and the panel. Run typecheck and build.
- [ ] **Step 4:** Browser QA with `$B`:
  1. spend 300 and capture;
  2. pay 300 and settle → available restored;
  3. return → restricted, and a purchase declines `payment_returned`;
  4. pay and settle → unrestricted.

  No console errors are allowed.
- [ ] **Step 5:** Commit `feat(web): payments panel and full action coverage`.

### Task 10: Docs

- [ ] **Step 1:** Update `docs/TESTING.md` (results), `docs/SPRINTS.md` (Sprint 3 done), `docs/DECISIONS.md` (ADR-018: the spendable credit balance supersedes the ADR-014 strict limit, accepted return risk, rulings), `README.md` (payment API) and `PROMPTS.md`.
- [ ] **Step 2:** Run `make test && make test-integration && make web-test && make tf-validate`. Expected: all PASS.
- [ ] **Step 3:** Commit `docs: sprint 3 results and decisions`.
