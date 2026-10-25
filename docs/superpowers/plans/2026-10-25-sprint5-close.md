# Sprint 5 — Close, Cancel, Payouts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Customers and support can close an account: the card is blocked at once, in-flight work resolves, a final statement settles the debt (from the collateral on its due date if unpaid), and the remaining collateral plus credit balance is paid out to an encrypted destination bank account once the bank confirms. Unactivated accounts are cancelled instantly.

**Architecture:**
- accounts owns the close saga (a `close_stage` driven by a per-minute sweep), payouts and their attempts.
- billing cuts an off-cycle final statement on request and stops scheduling after it.
- ledger posts the two-leg payout.
- The destination account number is encrypted with AES-256-GCM (`pkg/secretbox`); the bank fetches the plain number through a bank-signed instruction route.

**Tech Stack:** Go 1.26, gRPC/protobuf (buf), pgx v5, chi, testcontainers, k6, Next.js 16.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint5-close-design.md`. Read §3, §4, §6b, §6c (§6c overrides §4 where they differ).

## Global Constraints

**States**
- Account states:
  - `pending_collateral → cancelled`
  - `active → closing → closed`
- `closing` and `closed` decline every new authorization: `account_closing` while closing, `account_closed` once closed. Both sit in the first slot of the decline order.
- Captures, reversals and refunds of authorizations approved before the request keep working. Payments are accepted while receivable > 0.
- New deposits are rejected on `closing`, `closed` and `cancelled` accounts (409 `account_closing` or `account_closed`).

**Close stages** (`accounts.close_stage`, in order):

| Stage | Leaves when |
|---|---|
| `in_flight` | there is no approved, pending or pending-op authorization, no pending/settling payment, no settling deposit, no pending refund and no pending charge-off |
| `final_statement` | billing `FinalizeStatement` returns the final due date |
| `awaiting_due` | receivable ≤ 0, or account time reaches `OnTimeCutoff(final_due)` with no payment received before it still pending, or the account is already charged off |
| `applying` | `min(max(receivable,0), collateral)` has been applied from the collateral (`closeout:<id>`, entry `collateral_applied`) |
| `awaiting_returns` | no settled payment of the account has `settled_at + 120h` still in the future (real time) |
| `payout` | the payout settled, or nothing was owed to the customer |
| `closed` | — |

- A return that lands while closing re-raises receivable. The saga moves back to `awaiting_due` from any stage before `payout`.

**Payout**
- Amount = remaining collateral + max(−receivable, 0).
- Ledger (one journal `payout:<attempt_id>`): debit `collateral_held` for the collateral part, debit `receivable` for the credit-balance part, credit `cash_clearing` for the total. Collateral and credit limit drop to 0.
- Payout states: `pending → settling → settled | failed`; attempts are 1..n. One open attempt per payout. `failed` is final for its attempt; a later `settled` for it gets 409 `attempt_failed`.
- Retry: support only, creates the next attempt.
- External reference = attempt id (issued by the platform).
- Webhook amount, currency and `destination_last4` must match, or 422 `destination_mismatch` / `amount_mismatch`.
- A refund landing on a `closed` account creates a credit balance. The sweep creates an additional payout to the same destination.

**Destination**
- `{bank_code, account_number, holder_name}`:
  - `bank_code` is 3–11 alphanumerics;
  - `account_number` is 4–34 digits;
  - `holder_name` is 1–100 characters.
- Stored as AES-256-GCM ciphertext (`pkg/secretbox`, key `DESTINATION_KEY`, 32 bytes base64; the gateway is not involved) plus `last4`.
- The plain number appears only in the bank instruction response. Logs, events and every other response carry `****<last4>`.

**Cancel**
- Only in `pending_collateral`. Pending deposits become `voided`.
- 409 `deposit_settling` while a deposit is settling.
- A webhook for a voided deposit gets 409 `deposit_voided` and posts nothing.
- A close request also voids pending deposits.

**Reopen**
- The unique (customer, currency) constraint becomes a partial unique index `WHERE state NOT IN ('closed','cancelled')`.
- `CreateAccount` gets 409 `previous_account_has_debt` while a closed account of the same customer and currency has receivable > 0 (ledger read).

**Final statement**
- An off-cycle statement `cycle = last+1`, `final = true`, period `[last close, account_now)`.
- Interest follows the normal grace rules. Due = close + 21 days.
- After it, catch-up generates no statements, fees, blocks or interest. Charge-off already applied stays.

**Events**
- `account.close_requested`, `account.cancelled`, `account.closed`, `account.close_stage_changed`, `deposit.voided`
- `payout.requested` (no plain number), `payout.settled`, `payout.failed`, `payout.retried`
- `statement.generated` with `final: true`
- `ledger.payout_posted`

**Process**
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Stage explicit paths only.
- Next migrations: accounts `000015`, ledger `000005`, billing `000004`. ADR-020.
- TDD: the failing test first.

## Review Focus

1. **A payment return during `awaiting_returns` or right before the payout.** No payout may be created while any return window is open, and the saga must re-apply collateral if the return re-raised debt. Covered by a test in Task 5.
2. **A late merchant capture after the final statement.** It cannot happen: the final statement waits for in-flight work to end. A capture racing the stage change must either land before the final statement or fail on an expired hold. Covered by a test in Task 4.
3. **The payout webhook ×50 concurrently, plus a retry racing a late `settled` of the failed attempt.** Exactly one release. Covered by a test in Task 5.
4. **The destination number leaking** into events, logs or `GET` responses. Covered by a test in Task 5 that greps outbox payloads.
5. **Close requested twice** (customer then support) or with a different destination. The first wins, the second replays, and a different destination gets 409 `close_already_requested`. Covered by a test in Task 4.

---

### Task 1: Contracts

**Files:**
- Modify:
  - `proto/acme/common/v1/common.proto` (`DECLINE_REASON_ACCOUNT_CLOSING = 12`)
  - `proto/acme/accounts/v1/accounts.proto`
  - `proto/acme/ledger/v1/ledger.proto`
  - `proto/acme/billing/v1/billing.proto`
  - `services/ledger/ledgertest/ledgertest.go`
- Regenerate: `gen/go`

**Interfaces (produced):**
- **accounts**
  - `AccountState` gains `CLOSING=4`, `CANCELLED=5`; `DepositStatus` gains `VOIDED=4`.
  - `Account` gains `close_stage`, `close_requested_at`, `destination_last4`.
  - `message Destination{bank_code, account_number, holder_name}`.
  - RPCs:
    - `CloseAccount(CloseAccountRequest{account_id, destination, reason, meta}) → GetAccountResponse`
    - `CancelAccount(CancelAccountRequest{account_id, meta}) → GetAccountResponse`
    - `ListPayouts(ListPayoutsRequest{account_id}) → ListPayoutsResponse{payouts}`
    - `RetryPayout(RetryPayoutRequest{account_id, payout_id, meta}) → PayoutResponse`
    - `SettlePayout(SettlePayoutRequest{attempt_id, amount_minor, currency, destination_last4, status, meta}) → PayoutResponse`
    - `GetPayoutInstruction(GetPayoutInstructionRequest{attempt_id}) → PayoutInstruction{attempt_id, amount_minor, currency, bank_code, account_number, holder_name}`
    - `AdvanceClose(AdvanceCloseRequest{account_id}) → GetAccountResponse` (runs one saga step; used by the sweep and tests)
  - `Payout{id, account_id, amount_minor, currency, status, attempt, attempt_id, destination_last4, created_at, settled_at}`; `PayoutResponse{payout, account, replayed}`
- **ledger**: `rpc PostPayout(PostPayoutRequest{account_id, reference, collateral_minor, credit_minor, meta}) returns (PostJournalResponse)`
- **billing**: `rpc FinalizeStatement(FinalizeStatementRequest{account_id}) returns (FinalizeStatementResponse{statement, due_date})`. It is idempotent: a second call returns the existing final statement. `Statement` gains `final`.

- [ ] **Step 1:** Edit the protos. Run `buf lint && buf generate && go build ./...`. Expected: clean.
- [ ] **Step 2:** Add the ledgertest adapter method. Run `go vet -tags=integration ./...`. Expected: clean.
- [ ] **Step 3:** Commit `feat(proto): Sprint 5 contracts (close, cancel, payouts)`.

### Task 2: Ledger payout and `pkg/secretbox`

**Files:**
- Create:
  - `services/ledger/internal/ledger/payouts.go`
  - `services/ledger/internal/ledger/payouts_integration_test.go`
  - `pkg/secretbox/secretbox.go`
  - `pkg/secretbox/secretbox_test.go`

**Interfaces:**
- `PostPayout`:
  - under the account mutex, idempotent on reference;
  - rejects `collateral_minor > held` and `credit_minor > max(−receivable,0)` with `FailedPrecondition insufficient_funds`;
  - at least one part > 0;
  - posts debit `collateral_held`(c) + debit `receivable`(r) / credit `cash_clearing`(c+r); entry `payout_settled`;
  - emits `ledger.payout_posted`.
- `secretbox`:
  - `func New(key []byte) (*Box, error)` (32 bytes);
  - `func (b *Box) Seal(plain []byte) (string, error)` (base64 of nonce‖ciphertext);
  - `func (b *Box) Open(sealed string) ([]byte, error)`;
  - `func Last4(s string) string`.

- [ ] **Step 1: Failing tests.**
  - `TestPostPayoutReleasesCollateralAndCredit`: collateral 100000, refund leaves receivable −700 → payout (100000, 700) → collateral_held 0, receivable 0, cash_clearing +100700, balanced; replay → `replayed`.
  - `TestPostPayoutRejectsMoreThanHeld`: (100001, 0) and (0, 1) with no credit balance → FailedPrecondition.
  - `TestPostPayoutConcurrent50`: one entry.
  - `TestSecretboxRoundTripAndTamper`: Open(Seal(x)) == x; one flipped byte → error; wrong key → error; two Seals of the same input differ; `New` with 31 bytes → error; `Last4("0012345678") == "5678"`.
- [ ] **Step 2:** Run `go test ./pkg/secretbox/ && go test -tags=integration ./services/ledger/...`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS.
- [ ] **Step 5:** Commit `feat(ledger): two-leg payout; feat(pkg): secretbox AES-GCM`.

### Task 3: Accounts — states, cancel, voided deposits, reopen, account_closing decline

**Files:**
- Create: `services/accounts/migrations/000015_close.{up,down}.sql`. It covers:
  - the state CHECK gains `closing`, `cancelled`;
  - the deposit status CHECK gains `voided`;
  - the partial unique index replaces `UNIQUE (customer_id, currency)`;
  - account columns `close_stage`, `close_requested_at`, `close_requested_by`, `close_reason`, `destination_ciphertext`, `destination_last4`, `final_due_at`, `close_stage_at`;
  - tables `payouts` and `payout_attempts` (used in Task 5).
- Create: `services/accounts/internal/accounts/close.go` (cancel and helpers).
- Modify: `state.go` (new states and transitions), `decline.go` (`account_closing`), `authorize.go`, `service.go` (`CreateAccount` reopen rule; `SettleDeposit` rejects voided), `postauth.go` (closed and closing accept capture/reverse/refund as before).
- Test: `state_test.go`, `decline_test.go`, `close_integration_test.go`.

**Interfaces:** `CancelAccount`; `func (s *Service) voidPendingDeposits(ctx, tx, accountID, meta) error`; `Transition` gains `EventRequestClose`, `EventCancel`, `EventCloseCompleted`.

- [ ] **Step 1: Failing tests.**
  - `state_test`: `pending_collateral --cancel--> cancelled`; `active --request_close--> closing --close_completed--> closed`; cancel on active → error; request_close on pending → error; closed and cancelled are terminal.
  - `decline_test`: `closing` → `account_closing`, ahead of `charged_off`.
  - `TestCancelVoidsPendingDeposits`: a pending account with 2 pending deposits → cancel → state `cancelled`, deposits `voided`, `deposit.voided` ×2. A late settle → 409 `deposit_voided`, 0 journals.
  - `TestCancelWhileDepositSettling`: a settling deposit → 409 `deposit_settling`.
  - `TestCancelActiveAccountRejected`: → FailedPrecondition `not_cancellable`.
  - `TestReopenRules`:
    - cancelled → create again in USD works;
    - a closed account with receivable 0 → create works;
    - a closed account with receivable > 0 (set up via the ledger) → `previous_account_has_debt`;
    - an active account → `AlreadyExists` as today.
- [ ] **Step 2:** Run unit and integration tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS; all existing accounts tests green.
- [ ] **Step 5:** Commit `feat(accounts): cancel, voided deposits, closing states, reopen rule`.

### Task 4: Billing final statement; accounts close request and saga up to collateral application

**Files:**
- Create: `services/billing/migrations/000004_final.{up,down}.sql` (`statements.final boolean NOT NULL DEFAULT false`), `services/billing/internal/billing/final.go`.
- Modify: `catchup.go` (stop after a final statement).
- Create: `services/accounts/internal/accounts/close_saga.go`.
- Modify: `delinquency.go` (reuse the application helper), `expiry.go` (the sweep calls `AdvanceClose` for closing accounts), `services/accounts/cmd/main.go` (billing client), `accountstest`.
- Test: `services/billing/internal/billing/final_integration_test.go`, `services/accounts/internal/accounts/close_saga_integration_test.go`.

**Interfaces:**
- `NewService` gains `WithBilling(billingv1.BillingServiceClient)`, optional; without it the saga stays in `final_statement`.
- `CloseAccount`:
  - validates the destination;
  - for an `active` account: encrypts the destination, sets `closing` / `in_flight`, voids pending deposits, emits `account.close_requested`;
  - for one already closing: replays when the destination is identical, otherwise 409 `close_already_requested`;
  - support needs a reason.
- `AdvanceClose`: one step per stage from Global Constraints. Each transition is under `FOR UPDATE` and emits `account.close_stage_changed`.
- Collateral application: reference `closeout:<account_id>`, the same posting as charge-off, `effective_at` = due cutoff (business date).

- [ ] **Step 1: Failing tests.**
  - `TestFinalizeStatement` (billing):
    - purchase, then advance mid-cycle → `FinalizeStatement` cuts a `final` statement for `[last close, account_now)`, due = +21 days, interest per grace;
    - a second call returns the same statement;
    - catch-up 60 days later creates no more statements or charges.
  - `TestCloseBlocksPurchasesImmediately`: close → a new authorization is declined `account_closing`; a capture of an earlier approval still works; `close_stage` = `in_flight`.
  - `TestCloseWaitsForInFlightThenFinalStatement`: approved auth → `AdvanceClose` keeps `in_flight` → capture → `AdvanceClose` → `awaiting_due` with `final_due_at` set (billing in-process).
  - `TestCloseWithUnpaidDebtAppliesCollateralOnDueDate`: owe 30000 → advance past the final due cutoff → `AdvanceClose` → `collateral_applied` 30000 effective at the cutoff → `awaiting_returns`.
  - `TestCloseDebtPaidBeforeDue`: pay in full before the due date → no application → `awaiting_returns`.
  - `TestCloseWaitsForPendingPaymentAtDue`: a payment created before the due date is still pending at the cutoff → stays `awaiting_due` until the bank answers.
  - `TestCloseTwiceSameDestinationReplays` / `TestCloseDifferentDestinationConflicts`.
  - `TestSupportCloseNeedsReason`.
  - `TestCloseChargedOffAccountSkipsWait`.
  - `TestCaptureRacingFinalStatement` (Review Focus 2): a capture and `AdvanceClose` run concurrently. Either the capture is on the final statement, or the final statement waits for it.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS.
- [ ] **Step 5:** Commit `feat: close request, final statement and close saga up to collateral application`.

### Task 5: Payouts — return window, attempts, instruction, settle, retry, late refunds

**Files:**
- Create: `services/accounts/internal/accounts/payouts.go`, `payouts_integration_test.go`.
- Modify: `close_saga.go` (stages `awaiting_returns`, `payout`, `closed`), `postauth.go` (a refund on a closed account triggers an additional payout through the sweep), `expiry.go` (`ResolvePending` resumes settling payouts), `services/accounts/cmd/main.go` + `pkg/config` (`DESTINATION_KEY`).

**Interfaces:**
- `SettlePayout`: reserve (attempt `pending → settling`, checks) → ledger `PostPayout(payout:<attempt_id>, collateral part, credit part)` → finalize (payout and attempt `settled`; collateral and limit 0; saga → `closed`).
- `failed`: the attempt and the payout are marked `failed`, with no ledger effect.
- `RetryPayout`: support only (gateway); payout `failed` → a new attempt, `payout.retried` + `payout.requested`.
- `GetPayoutInstruction`: decrypts for the bank only.
- `WithReturnWindow(d time.Duration)` option; `services/accounts/cmd/main.go` reads `RETURN_WINDOW` (default 120h) and refuses a value other than 120h unless `APP_ENV=dev`. Only the load test shortens it.

- [ ] **Step 1: Failing tests.**
  - `TestPayoutWaitsForReturnWindow` (Review Focus 1): settle a payment, close → stays `awaiting_returns` until the fake clock passes `settled_at + 120h`, then the payout is created. A return inside the window → back to `awaiting_due`, and the collateral is re-applied on the due date.
  - `TestPayoutSettleReleasesOnce`: webhook ×50 → one `payout_settled` journal, account `closed`, collateral 0, `account.closed` once.
  - `TestPayoutFailRetrySettle`: fail → money still held → retry → attempt 2 → settle → closed. A late `settled` for attempt 1 → 409 `attempt_failed`, still one journal.
  - `TestPayoutWebhookChecks`: wrong last4 → 422 `destination_mismatch`; wrong amount → `amount_mismatch`; unknown attempt → 404.
  - `TestPayoutIncludesCreditBalance`: credit balance 700 + collateral 100000 → payout 100700.
  - `TestDestinationNeverLeaks` (Review Focus 4): after a full close flow, no outbox payload, account proto or payout proto contains the full account number; the instruction does.
  - `TestLateRefundAfterCloseAdditionalPayout`: closed account, refund of a captured purchase → the sweep creates payout 2 for the refund amount → settle → credit balance 0.
  - `TestNothingToPayOut`: debt ≥ collateral → `closed` with no payout; remaining debt can still be paid.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): payouts with return-window hold, attempts, encrypted destination`.

### Task 6: Gateway

**Files:** `services/gateway/internal/server/close.go`, `close_test.go`, `server.go`, `network.go` (decline name `account_closing`).

**Routes:**
- Customer (JWT + account):
  - `POST /v1/accounts/{id}/close` `{destination}` → 202 account;
  - `POST /v1/accounts/{id}/cancel` → 200;
  - `GET /v1/accounts/{id}/payouts`.
- Support: `POST /v1/support/accounts/{id}/close` `{destination, reason}` → 202; `POST /v1/support/accounts/{id}/payouts/{payoutID}/retry` → 201.
- Bank (HMAC bound to the request):
  - `POST /v1/webhooks/bank/payouts` `{attempt_id, amount_minor, currency, destination_last4, status}`;
  - `GET /v1/bank/payout-instructions/{attemptID}` (signed GET with an empty body).
- Account JSON adds `close_stage`, `close_requested_at`, `destination_last4`. States `closing`, `cancelled`.

- [ ] **Step 1: Failing tests.** Route and auth matrix: customer, support and bank signatures; strict JSON; the instruction requires a bank signature (401 without); account JSON never contains a full destination; the decline name map includes `account_closing` (existing guard test).
- [ ] **Step 2–4:** Run (FAIL) → implement → `go test ./services/gateway/...` PASS (simulator coverage red until Task 7).
- [ ] **Step 5:** Commit `feat(gateway): close, cancel, payouts, bank payout instruction and webhook`.

### Task 7: Simulator — "8 · Close"

**Files:**
- `web/lib/ui-actions.json`
- `web/app/page.tsx` (panel 8: destination form, close, support close with reason, cancel, payouts table with bank Fetch instruction / Settle / Fail, support Retry, close stage badge)
- `web/app/api/bank/payouts/route.ts` (signs the instruction GET and the webhook server-side)
- `web/lib/payouts.ts` + test (path builder and last4 helper)

- [ ] **Step 1:** Write the failing tests (`payouts.test.ts`, `actions.test.ts`). Expected: FAIL.
- [ ] **Step 2:** Implement. Run `make web-test && go test ./services/gateway/...`. Expected: PASS including simulator coverage.
- [ ] **Step 3:** `make up`; `/browse` the flow: open → deposit → buy → close → *To due + 1* → collateral applied → payout fetch → settle → closed; also cancel on a fresh account.
- [ ] **Step 4:** Commit `feat(web): close, cancel and payouts panel`.

### Task 8: Testing flow — e2e, invariants, load

**Files:** `tests/integration/sprint5_test.go`, `tests/load/verify.sql`, `tests/load/flows.js`, `docs/testing/sprint5.md`.

- [ ] **Step 1: e2e** through the gateway with real stores:
  - close in good standing → payout settle → closed → reopen;
  - close with debt → advance past the due date → application → payout of the remainder;
  - fail + support retry;
  - cancel + late deposit webhook 409;
  - close ×50 same key;
  - audit events present.
- [ ] **Step 2: verify.sql** additions (each 0), with corruption tests in a rolled-back transaction:
  - a closed account still holding collateral or credit balance (unless the payout is failed or pending);
  - a `payout_settled` journal ≠ settled payout attempt (both directions);
  - Σ settled payouts per account > collateral ever held + credit balances ever created;
  - a closed account with in-flight work;
  - a payout created while a return window was open;
  - a `cancelled` account with a non-voided pending deposit;
  - closing accounts stuck in a stage > 10 min without a reason (excluding `awaiting_due` and `awaiting_returns`).
- [ ] **Step 3: k6**: 1 in 5 iterations closes its account at the end (clean balance), advances the clock 6 days, waits for the return window (the load runs with `RETURN_WINDOW` shortened via env `RETURN_WINDOW=1m` in the isolated project), fetches the instruction and settles the payout, then asserts `closed`.
- [ ] **Step 4:** Run the load in an isolated compose project (fresh volumes, main stack stopped and restarted). Record the results.
- [ ] **Step 5:** Commit `test: Sprint 5 end-to-end, invariants, load with closes`.

### Task 9: Docs

`docs/DECISIONS.md` ADR-020; `docs/SPRINTS.md`; `README.md` (close/payout API, `DESTINATION_KEY`); `PROMPTS.md`. Run the full suite (`make test`, integration all, web, tf-validate, buf lint). Commit `docs: Sprint 5 close and payouts (ADR-020)`.
