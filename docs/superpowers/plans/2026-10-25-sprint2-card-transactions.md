# Sprint 2 — Card Transactions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A simulated card network can authorize, capture, reverse and refund purchases against a secured card. Each decline carries a reason. Customers can freeze the card and set a purchase limit. The limit can never be exceeded.

**Architecture:**
- accounts owns policy and transaction rows: authorizations, refunds, freeze, purchase limit, velocity and the expiry worker.
- ledger owns money: holds plus capture and refund postings. Its final available check runs under a per-account lock.
- gateway exposes HMAC-signed network routes and JWT customer routes.
- No new service.

**Tech Stack:** Go 1.26, gRPC/protobuf (buf), pgx v5, chi, testcontainers, k6, and Next.js 16 for the simulator.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint2-card-transactions-design.md`. Read §3, §4, §4b and §6 before starting.

## Global Constraints

**Money**
- Money is `int64` minor units plus an ISO-4217 currency. No floats.
- Purchase amounts must be at least 1 and at most `accounts.MaxDepositMinor`, which is `1_000_000_000_000`.
- Strict limit: `Σ active holds + max(receivable, 0) + amount ≤ credit_limit`. In the ledger, credit_limit is the `collateral_held` balance.
- A negative receivable is a credit balance and never adds spending power.

**Declines**
- Order, first match wins:
  1. `account_closed`
  2. `collateral_not_confirmed`
  3. `currency_not_supported`
  4. `card_frozen`
  5. `velocity_limit_exceeded`
  6. `purchase_limit_exceeded`
  7. `insufficient_credit`
- A handled decline is HTTP 201 with `"status":"declined"` and `"decline_reason":"<reason>"`.

**Lifecycle**
- A hold lives 7 days (`HoldTTL = 7*24h`).
- A reversal is allowed only for an uncaptured authorization within 3 days of it (`ReversalWindow = 72h`).
- Exactly one capture per authorization, with `0 < capture ≤ authorized`.
- Refunds go only against captured authorizations, and Σ refunds ≤ captured. Multiple partial refunds are allowed.

**Limits**
- Velocity: 10 attempts per minute per card, as a sliding window serialized per card. Every attempt counts, including ones declined for any reason. No daily cap.
- `purchase_limit_minor`: `0` means no cap, and it must be ≤ credit_limit. It is checked per transaction.

**Freeze and close**
- Frozen blocks only new authorizations.
- On a closed account, captures are refused with 409 `account_closed`, while reversals and refunds are accepted.

**Network messages**
- `network_ref` is unique per message type: authorization, capture, reversal and refund.
- Same ref and type with the same body replays the original response, with header `Idempotency-Replayed: true`. A different body returns 422 `network_ref_reuse`.
- Network routes are HMAC-signed with `NETWORK_WEBHOOK_SECRET`, using the same scheme as `pkg/webhook`. Rejections go to `webhook_rejections` and are not audited per request.

**Platform**
- Customer writes require JWT plus `Idempotency-Key`, and the account must be in `authorized_accounts`.
- Every state change emits an outbox event with before and after snapshots:
  - `authorization.approved`, `authorization.declined`, `authorization.captured`, `authorization.reversed`, `authorization.expired`
  - `refund.posted`
  - `card.frozen`, `card.unfrozen`, `limits.updated`
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Every user prompt is appended to `PROMPTS.md`.

## Review Focus

1. **Capture after day 7 but before the expiry worker ran** must be refused with 409 `authorization_expired`. It must never post. Covered by a test in Task 6.
2. **Capture racing a reversal or the expiry worker** on the same authorization: exactly one wins, and the hold ends in exactly one terminal state. Covered by tests in Tasks 6 and 7.
3. **A purchase limit above the credit limit** gives 400. Lowering the limit below current holds is allowed and affects only new authorizations. Covered by a test in Task 4.
4. **An unknown `card_id`** gives 404. It stores no authorization row and consumes no velocity. Covered by a test in Task 5.
5. **A refund on a closed account** is accepted, and a capture is refused. Covered by a test in Task 6.

---

### Task 1: Contracts and shared building blocks

**Files:**
- Modify: `proto/acme/common/v1/common.proto` (add `DECLINE_REASON_CURRENCY_NOT_SUPPORTED = 7;`)
- Modify: `proto/acme/ledger/v1/ledger.proto`, `proto/acme/accounts/v1/accounts.proto`
- Create: `pkg/clock/clock.go`, `pkg/clock/clock_test.go`
- Move: `services/gateway/internal/ratelimit/*` → `pkg/ratelimit/` (gateway imports updated). The integration test migrates its own table, `rate_limit_windows`. Its DDL is exported as `ratelimit.Migration` (const string).
- Regenerate: `gen/go/...` (`buf generate`)

**Interfaces (Produces):**
- `clock.Clock interface{ Now() time.Time }`; `clock.System{}`; `clock.NewFake(t time.Time) *clock.Fake` with `Now()`, `Advance(d time.Duration)`. Fake is goroutine-safe.
- `ratelimit.New(pool *pgxpool.Pool, limit int, window time.Duration) *Limiter` and `(*Limiter).Allow(ctx, bucket string, now time.Time) (bool, time.Duration, error)`. Both are unchanged; only the package path changes.

**Ledger proto additions:**
- `rpc PlaceHold(PlaceHoldRequest) returns (PlaceHoldResponse)`
  - request: `account_id, reference, amount_minor, currency, meta`
  - response: `hold_id, approved bool, available_minor, replayed`
- `rpc CaptureHold(CaptureHoldRequest) returns (PostJournalResponse)`
  - request: `hold_reference, journal_reference, amount_minor, meta`
- `rpc ReleaseHold(ReleaseHoldRequest) returns (ReleaseHoldResponse)`
  - request: `hold_reference, HoldRelease reason {RELEASED, EXPIRED}, meta`
  - response: `status string, replayed`
- `rpc PostRefund(PostRefundRequest) returns (PostJournalResponse)`
  - request: `account_id, reference, amount_minor, meta`
- `GetBalancesResponse`: add `holds_minor`, `available_minor`.

**Accounts proto additions:**
- `rpc Authorize(AuthorizeRequest) returns (AuthorizationResponse)`
  - request: `network_ref, card_id, amount_minor, currency, Merchant merchant{id,name,mcc}, meta`
- `rpc Capture(CaptureRequest) returns (AuthorizationResponse)`
  - request: `authorization_id, network_ref, amount_minor, meta`
- `rpc Reverse(ReverseRequest) returns (AuthorizationResponse)`
  - request: `authorization_id, network_ref, meta`
- `rpc Refund(RefundRequest) returns (RefundResponse)`
  - request: `authorization_id, network_ref, amount_minor, meta`
  - response: `Refund refund{id, authorization_id, network_ref, amount, created_at}, Authorization authorization, replayed`
- `rpc SetFrozen(SetFrozenRequest{account_id, frozen bool, meta}) returns (GetAccountResponse)`
- `rpc SetPurchaseLimit(SetPurchaseLimitRequest{account_id, purchase_limit_minor, meta}) returns (GetAccountResponse)`
- `rpc ListTransactions(ListTransactionsRequest{account_id, page_size, page_token}) returns (ListTransactionsResponse{repeated Authorization authorizations, next_page_token})`
- `message Authorization`:
  - `id, account_id, network_ref, Money amount, Merchant merchant`
  - `AuthorizationStatus status` {APPROVED, DECLINED, CAPTURED, REVERSED, EXPIRED}
  - `common.DeclineReason decline_reason`
  - `captured_amount_minor, refunded_amount_minor`
  - timestamps `created_at, expires_at, captured_at, reversed_at`
- `AuthorizationResponse{Authorization authorization, bool replayed}`.

- [ ] **Step 1:** Write `pkg/clock/clock_test.go`: `TestFakeAdvance` asserts `NewFake(t0).Now()==t0` and that after `Advance(72*time.Hour)` it equals `t0.Add(72h)`. Add a concurrent Advance/Now test under `-race`.
- [ ] **Step 2:** Run `go test -race ./pkg/clock/`. Expected: FAIL, `undefined: NewFake`.
- [ ] **Step 3:** Implement `pkg/clock`. Move ratelimit to `pkg/ratelimit` and update the gateway imports. Edit the protos and run `buf lint && buf generate`.
- [ ] **Step 4:** Run `go build ./... && go test -race ./pkg/... ./services/... && go test -race -tags=integration ./pkg/ratelimit/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(proto): sprint 2 contracts; pkg/clock; move ratelimit to pkg`.

### Task 2: Ledger holds, capture, release, refund

**Files:**
- Create: `services/ledger/migrations/000003_holds.{up,down}.sql`
- Create: `services/ledger/internal/ledger/holds.go`
- Modify: `services/ledger/internal/ledger/service.go` (GetBalances)
- Test: `services/ledger/internal/ledger/holds_integration_test.go`

**Interfaces:**
- Consumes: Task 1 protos.
- Produces: `(*Service).PlaceHold`, `CaptureHold`, `ReleaseHold` and `PostRefund` (gRPC), plus GetBalances `holds_minor` and `available_minor`. `ledgertest.NewClient` needs no change, because it forwards everything. Extend its adapter methods for the 4 new RPCs.

**Schema:**
- Table `holds(id uuid pk, account_id uuid, reference text UNIQUE, amount_minor bigint >0, captured_minor bigint, status text CHECK in ('active','captured','released','expired'), created_at, finished_at)`.
- Index `(account_id) WHERE status='active'`.
- Append-only triggers do not apply: holds change status. Every change is evented.

**Locking rule:** PlaceHold, CaptureHold, ReleaseHold and PostRefund all start with `SELECT … FROM balances WHERE account_id=$1 AND sub_account='collateral_held' FOR UPDATE`, the per-account mutex.

**Semantics:**
- **PlaceHold:**
  - Insert-first on `reference`. A replay with the same amount returns the original result with `replayed=true`; a different amount returns `AlreadyExists`.
  - Compute `limit = collateral`, `exposure = Σactive + max(receivable,0)`.
  - If `exposure + amount > limit`, store nothing and return `approved=false`. The decline must not create an active hold, so the reference is not stored on decline.
  - A wrong currency returns `InvalidArgument`.
- **CaptureHold:**
  - Requires the hold to be `active` and `0 < amount ≤ hold.amount`.
  - Posts `debit receivable / credit cash_clearing` with `journal_reference`, which makes it idempotent.
  - Sets the hold `captured` with `captured_minor`.
  - If the hold is already captured with the same journal_reference, it replays. Otherwise it returns `FailedPrecondition` with reason `hold_not_active`.
- **ReleaseHold:** `active` → `released` or `expired`. Repeating with the same reason replays; a different terminal state returns `FailedPrecondition hold_not_active`.
- **PostRefund:** posts `debit cash_clearing / credit receivable`. It is idempotent on `reference` and may drive receivable negative.
- **GetBalances:** `available = max(0, collateral − max(receivable,0) − Σactive)`.

- [ ] **Step 1: Failing tests** in `holds_integration_test.go`. Fund 1000 via the existing deposit journal helper.
  - `TestPlaceHoldStrictLimit`: hold 600 approves; hold 500 returns `approved=false`; available is 400.
  - `TestConcurrentHoldsNeverExceedLimit`: 10 goroutines hold 250 each on a 1000 limit → exactly 4 approved, and Σactive = 1000.
  - `TestPlaceHoldReplayAndMismatch`: the same reference replays; a different amount returns `AlreadyExists`.
  - `TestCaptureHoldPostsReceivableAndReleasesRemainder`: hold 300, capture 200 → receivable 200, holds 0, available 800, and the journal is balanced.
  - `TestCaptureVsReleaseExactlyOneWins`: run capture and release concurrently 25 times. Each time exactly one succeeds and the other gets `FailedPrecondition`.
  - `TestRefundCanMakeReceivableNegativeButNotRaiseAvailable`: capture 200, then refund 200 and 100 as a fixture refund beyond capture, at ledger level (spec C-2). Receivable becomes −100 and available stays 1000, not 1100.
  - `TestHoldOperationsAreEvented`: one outbox event per state change (`ledger.hold_placed`, `ledger.hold_captured`, `ledger.hold_released`, `ledger.refund_posted`); replays add none.
- [ ] **Step 2:** Run `go test -race -tags=integration ./services/ledger/...`. Expected: FAIL, missing methods.
- [ ] **Step 3:** Implement the migration, `holds.go` and the GetBalances fields.
- [ ] **Step 4:** Run the same command. Expected: PASS, with the existing ledger tests still green.
- [ ] **Step 5:** Commit `feat(ledger): holds with strict-limit check, capture, release, refund`.

### Task 3: Accounts migrations and available-credit source

**Files:**
- Create: `services/accounts/migrations/000005_transactions.{up,down}.sql`
- Modify: `services/accounts/internal/accounts/service.go` (`available()` uses ledger `available_minor`)
- Test: extend `service_integration_test.go`

**Schema:**
- `authorizations`:
  - `id uuid pk, account_id uuid FK, network_ref text UNIQUE, request_hash char(64)`
  - `amount_minor, currency, merchant_id, merchant_name, mcc`
  - `status CHECK in (approved,declined,captured,reversed,expired)`, `decline_reason text`
  - `captured_minor bigint default 0, refunded_minor bigint default 0`
  - `capture_network_ref text UNIQUE, reversal_network_ref text UNIQUE`
  - `created_at, expires_at, captured_at, reversed_at`
  - index `(account_id, created_at desc)`; index `(expires_at) WHERE status='approved'`
- `refunds(id uuid pk, authorization_id FK, network_ref text UNIQUE, amount_minor >0, created_at)`
- `rate_limit_windows` (from `ratelimit.Migration`) for velocity.

- [ ] **Step 1:** Failing test `TestAvailableComesFromLedgerHolds`: after a settled deposit of 1000 and a direct ledger `PlaceHold` of 300 via `f.ledgerClient`, `GetAccount` shows available 700.
- [ ] **Step 2:** Run `go test -race -tags=integration -run AvailableComesFromLedgerHolds ./services/accounts/...`. Expected: FAIL (1000).
- [ ] **Step 3:** Implement the migration and the `available()` change: `min(ledger.available_minor, credit_limit)`.
- [ ] **Step 4:** Run all accounts integration tests. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): transaction tables; available credit from ledger holds`.

### Task 4: Freeze, unfreeze, purchase limit

**Files:**
- Create: `services/accounts/internal/accounts/controls.go`
- Test: `services/accounts/internal/accounts/controls_integration_test.go`

**Interfaces (Produces):** `(*Service).SetFrozen` and `(*Service).SetPurchaseLimit`. Both return `GetAccountResponse`.

- [ ] **Step 1:** Failing tests:
  - `TestSetFrozenTogglesAndEmits`: freeze → `frozen=true` with one `card.frozen` event; freezing again is a no-op with no new event; unfreeze emits `card.unfrozen`.
  - `TestSetPurchaseLimitBounds`: on an active account with a 1000 limit, 500 is ok, 0 is ok, 1001 → `InvalidArgument` with reason `purchase_limit_above_credit_limit`, and −1 → `InvalidArgument`. Each change emits `limits.updated` with before and after.
  - `TestControlsOnClosedAccount`: on a closed account, `SetPurchaseLimit` → `FailedPrecondition account_closed`. SetFrozen still succeeds, because freezing is harmless.
- [ ] **Step 2:** Run them. Expected: FAIL, undefined methods.
- [ ] **Step 3:** Implement with an account row lock (`FOR UPDATE`) and outbox events.
- [ ] **Step 4:** Run them. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): freeze/unfreeze and customer purchase limit`.

### Task 5: Authorize with ordered decline evaluation

**Files:**
- Create: `services/accounts/internal/accounts/authorize.go` (evaluation plus RPC)
- Create: `services/accounts/internal/accounts/decline.go` (pure function)
- Test: `decline_test.go` (unit), `authorize_integration_test.go`
- Modify: `NewService(pool, ledger, opts ...Option)`, adding options `WithClock(clock.Clock)` and `WithVelocity(limit int, window time.Duration)` (defaults 10, 1 min).

**Interfaces (Produces):**
- `func EvaluateDecline(in DeclineInput) DeclineReason` with `DeclineInput{State State; Frozen bool; AccountCurrency, Currency string; VelocityOK bool; PurchaseLimit, Amount int64}`. It returns `""` when policy passes; the ledger decides `insufficient_credit`.
- Constants: `DeclineAccountClosed="account_closed"`, `DeclineCollateralNotConfirmed="collateral_not_confirmed"`, `DeclineCurrencyNotSupported="currency_not_supported"`, `DeclineCardFrozen="card_frozen"`, `DeclineVelocity="velocity_limit_exceeded"`, `DeclinePurchaseLimit="purchase_limit_exceeded"`, `DeclineInsufficientCredit="insufficient_credit"`.
- `HoldTTL = 7*24*time.Hour`.

**Algorithm (Authorize):**
1. Validate the request; otherwise 400. Check the merchant id, name and mcc are non-empty, with lengths ≤ 64, 128 and 4.
2. Load the account without a lock. An unknown card returns `NotFound card_not_found`: no row, no velocity hit.
3. Begin tx. `INSERT … authorizations (status='approved' placeholder) ON CONFLICT (network_ref) DO NOTHING`.
   - On conflict, load the row. The same `request_hash` replays; a different one returns `AlreadyExists network_ref_reuse`.
   - Concurrent duplicates block on the unique index until the owner commits, then replay.
4. `velocityOK := limiter.Allow(ctx, "card:"+id, clock.Now())`. Every attempt that reaches here counts.
5. `reason := EvaluateDecline(...)`. If it is non-empty, update the row to declined with the reason.
6. Otherwise call ledger `PlaceHold(reference "auth:"+network_ref)`. `approved=false` declines with `insufficient_credit`; approved sets `expires_at = created_at + HoldTTL`.
7. Emit `authorization.approved` or `authorization.declined`, then commit.

The ledger call happens inside the open tx. The deterministic reference makes a retry after a crash replay the same hold.

- [ ] **Step 1: Failing unit tests** in `decline_test.go`:
  - `TestEvaluateDeclineEachReasonAlone`: table-driven, one case per reason.
  - `TestEvaluateDeclinePrecedence`: a closed, pending, wrong-currency, frozen, velocity-exceeded, over-limit input returns `account_closed`. Removing conditions one by one yields each next reason in order.
- [ ] **Step 2: Failing integration tests** in `authorize_integration_test.go`. Use an active account funded with 1000, the fake clock and velocity 10/min.
  - `TestAuthorizeApprovesAndHolds`: 300 → approved, available 700, `expires_at = created+7d`.
  - `TestAuthorizeDeclinesAre201Records`: frozen card → declined `card_frozen`, row stored, event `authorization.declined`, no hold.
  - `TestAuthorizeCurrencyNotSupported`: COP on a USD card → `currency_not_supported`.
  - `TestAuthorizePurchaseLimit`: limit 100, amount 101 → `purchase_limit_exceeded`.
  - `TestConcurrentAuthorizationsNeverExceedLimit`: 10 concurrent at 250 → exactly 4 approved and 6 `insufficient_credit`, with ledger Σactive = 1000.
  - `TestVelocityCountsDeclinedAttempts`: 10 attempts at 2000 (all `insufficient_credit`), then the 11th at 10 → `velocity_limit_exceeded`. After `clock.Advance(2*time.Minute)` the next attempt is approved.
  - `TestVelocityExactUnderConcurrency`: 50 concurrent attempts at 1 → exactly 10 pass velocity, i.e. are not declined `velocity_limit_exceeded`.
  - `TestAuthorizeSameNetworkRefOnce`: 50 concurrent identical requests → 1 row and 49 `replayed`. The same ref with a different amount → `AlreadyExists network_ref_reuse`.
  - `TestAuthorizeUnknownCard`: 404; no row; a subsequent velocity budget is untouched (Review Focus 4).
  - `TestAuthorizePendingCollateral`: `collateral_not_confirmed`.
- [ ] **Step 3:** Run `go test -race ./services/accounts/... && go test -race -tags=integration -run Authoriz ./services/accounts/...`. Expected: FAIL.
- [ ] **Step 4:** Implement `decline.go` and `authorize.go`.
- [ ] **Step 5:** Run the same commands. Expected: PASS.
- [ ] **Step 6:** Commit `feat(accounts): authorizations with ordered declines, velocity, strict-limit holds`.

### Task 6: Capture, reversal, refund

**Files:**
- Create: `services/accounts/internal/accounts/postauth.go`
- Test: `postauth_integration_test.go`

**Interfaces (Produces):** `(*Service).Capture`, `Reverse`, `Refund`. `ReversalWindow = 72*time.Hour`.

**Rules:** each operation locks the authorization row `FOR UPDATE` for the whole operation, including the ledger call.
- **Capture:**
  - Requires status approved and `clock.Now() < expires_at`; otherwise `FailedPrecondition authorization_expired`.
  - A closed account returns `FailedPrecondition account_closed`.
  - Requires `0 < amount ≤ amount_minor`.
  - Calls ledger `CaptureHold(hold "auth:"+network_ref, journal "capture:"+auth.id)` and sets status captured with `capture_network_ref`.
  - A replay with the same capture ref and the same amount replays; a different amount returns `AlreadyExists network_ref_reuse`.
  - A second capture with a new ref returns `FailedPrecondition already_captured`.
- **Reverse:**
  - Requires status approved and `now − created_at ≤ ReversalWindow`; otherwise `FailedPrecondition reversal_window_expired`.
  - Calls ledger `ReleaseHold(RELEASED)`, then sets status reversed.
  - A captured authorization returns `FailedPrecondition already_captured`. Replays work by `reversal_network_ref`.
- **Refund:**
  - Requires status captured; otherwise `FailedPrecondition not_captured`.
  - Requires `refunded + amount ≤ captured`; otherwise `FailedPrecondition refund_exceeds_captured`.
  - Inserts the refund (network_ref unique, replay on the same body) and calls ledger `PostRefund(reference "refund:"+network_ref)`. Then it increments `refunded_minor`.
  - Accepted on closed accounts.
- Events: `authorization.captured`, `authorization.reversed`, `refund.posted`.

- [ ] **Step 1: Failing tests:**
  - `TestCaptureLessThanAuthorizedReleasesRest`: auth 300, capture 200 → captured, receivable 200, available 800.
  - `TestCaptureOnlyOnce`: a second capture with a new ref → `already_captured`; the same ref replays.
  - `TestCaptureAfterExpiryRefused`: `clock.Advance(7*24h+time.Second)` with no worker run → `authorization_expired`, and no journal entry (Review Focus 1).
  - `TestCaptureVsReverseExactlyOne`: 25 rounds of concurrent capture and reverse → exactly one wins, and the ledger hold state matches the authorization status (Review Focus 2).
  - `TestReversalWindow`: within 72h → reversed and available restored. A new auth with `Advance(72h+1s)` → `reversal_window_expired`.
  - `TestPartialRefundsUpToCaptured`: capture 200, refunds of 120 and 80 succeed; another refund of 1 → `refund_exceeds_captured`. Concurrent refunds of 150 + 150 on a 200 capture → exactly one succeeds.
  - `TestClosedAccountFollowUps`: with the account set closed via SQL, capture → `account_closed`, while refund and reverse succeed (Review Focus 5).
  - `TestRefundBeforeCaptureRefused`: → `not_captured`.
- [ ] **Step 2:** Run `go test -race -tags=integration -run 'Capture|Reverse|Reversal|Refund|ClosedAccountFollowUps' ./services/accounts/...`. Expected: FAIL.
- [ ] **Step 3:** Implement `postauth.go`.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): capture, reversal window, partial refunds`.

### Task 7: Hold expiry worker and transaction listing

**Files:**
- Create: `services/accounts/internal/accounts/expiry.go`, `list.go`
- Test: `expiry_integration_test.go`
- Modify: `services/accounts/cmd/main.go` (start the worker every 1 minute)

**Interfaces (Produces):**
- `(*Service).ExpireDue(ctx context.Context, batch int) (int, error)`: selects approved rows with `expires_at < clock.Now()` `FOR UPDATE SKIP LOCKED LIMIT batch`, calls ledger `ReleaseHold(EXPIRED)`, sets status expired and emits `authorization.expired`.
- `(*Service).RunExpiry(ctx, every time.Duration)`.
- `(*Service).ListTransactions`: page_size default 20, max 100. The page token is an opaque base64 of `(created_at,id)`; results are newest first.

- [ ] **Step 1: Failing tests:**
  - `TestExpireDueReleasesHolds`: two approved auths, `Advance(7d+1s)` → `ExpireDue` returns 2, available is restored, and running it again returns 0.
  - `TestExpiryVsCaptureRace`: with the clock past expiry, run capture and `ExpireDue` concurrently ×10 → each auth ends `expired`, never captured, and no journal entries.
  - `TestListTransactionsPaginates`: 25 auths → pages of 20 and 5, newest first, with no duplicates.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement, and wire `RunExpiry` in main.
- [ ] **Step 4:** Run all accounts tests (unit and integration). Expected: PASS.
- [ ] **Step 5:** Commit `feat(accounts): hold expiry worker and transaction listing`.

### Task 8: Gateway routes

**Files:**
- Create: `services/gateway/internal/server/network.go`, `controls.go`
- Modify: `server.go` (routes and the `Deps.NetworkSecret []byte` field), `services/gateway/cmd/main.go`, `pkg/config` (`NETWORK_WEBHOOK_SECRET`, required by the gateway), `.env.example`, `.env.dev`, `deploy/docker-compose.yml`
- Test: `network_test.go`, `controls_test.go` (fake accounts client, as in `server_test.go`)

**Routes:**
- **Network** (HMAC with NetworkSecret, rejection log source = client IP, audit except signature failures):
  - `POST /v1/network/authorizations` → 201 for approved and declined, 404 `card_not_found`
  - `POST /v1/network/authorizations/{id}/capture` → 201
  - `POST /v1/network/authorizations/{id}/reversal` → 200
  - `POST /v1/network/authorizations/{id}/refunds` → 201
  - Replays carry `Idempotency-Replayed: true` and keep the same status code.
- **Customer** (JWT + RequireAccount + Idempotency-Key):
  - `POST /v1/accounts/{id}/freeze` and `/unfreeze` → 200 with the account JSON
  - `PUT /v1/accounts/{id}/limits` with `{"purchase_limit_minor":n}` → 200
  - `GET /v1/accounts/{id}/transactions?page_size=&page_token=` → 200 `{"transactions":[…],"next_page_token":""}`
- **Authorization JSON:** `id, account_id, network_ref, amount_minor, currency, merchant{id,name,mcc}, status, decline_reason (omitempty), captured_amount_minor, refunded_amount_minor, created_at, expires_at, captured_at, reversed_at`.

- [ ] **Step 1: Failing tests:**
  - `TestNetworkAuthorizeDeclineIs201`: the fake returns declined `card_frozen` → 201 with that `decline_reason`.
  - `TestNetworkRoutesRequireNetworkSignature`: a body signed with the bank secret → 401 `bad_signature`, recorded as a rejection.
  - `TestNetworkStrictJSON`: an unknown field → 400.
  - `TestReversalIs200AndReplayHeader`: the fake sets replayed → header present.
  - `TestFreezeRequiresAuthorizedAccount`: a foreign token → 403.
  - `TestLimitsBody`: an extra field → 400.
  - `TestTransactionsListShape`.
- [ ] **Step 2:** Run `go test -race ./services/gateway/...`. Expected: FAIL.
- [ ] **Step 3:** Implement the handlers and the gRPC error mapping (reuse `writeGRPCError`; new reasons pass through as codes).
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** Commit `feat(gateway): network transaction routes and card controls`.

### Task 9: End-to-end gates, load test, invariants

**Files:**
- Create: `tests/integration/sprint2_test.go` (reuses `startStack` from `sprint1_test.go`; the stack takes `NETWORK_WEBHOOK_SECRET`)
- Modify: `tests/integration/sprint1_test.go` (Deps gains NetworkSecret)
- Modify: `tests/load/sprint1.js`, renamed to `tests/load/flows.js` (Makefile updated), `tests/load/verify.sql`

- [ ] **Step 1: Failing e2e test** `TestSprint2EndToEnd`, with subtests through HTTP:
  - authorize → capture less than authorized → partial refunds → transactions list;
  - 10 concurrent authorizations at 25% → 4 approved;
  - the same `network_ref` ×50 → 1 authorization;
  - freeze → `card_frozen` 201, and unfreeze → approved;
  - an 11th attempt within a minute → `velocity_limit_exceeded`;
  - an unsigned network call → 401;
  - audit receives `authorization.approved|declined|captured`, `refund.posted` and `card.frozen`.
- [ ] **Step 2:** Run `go test -race -tags=integration -run Sprint2 ./tests/integration/`. Expected: FAIL until all routes are wired, then PASS.
- [ ] **Step 3:** Extend k6 so each VU, with its own card, performs authorize 2×same ref (expect one replay), capture 80%, refund half and reverse a second auth. Checks: replay flag, final available = deposit − captured + refunded.
- [ ] **Step 4:** Add rows to `verify.sql`, all expecting 0:
  - `holds + max(receivable,0) > collateral` per account (ledger);
  - authorizations captured with no captured hold;
  - holds active whose authorization is not approved;
  - Σ refunds > captured;
  - more than one capture journal per authorization.
- [ ] **Step 5:** Run `make up && make load` on an idle host. Expected: 0 failed requests, 0 invariant violations. Report p95 as measured; the latency gate applies only on an idle host (see TESTING.md).
- [ ] **Step 6:** Commit `test: sprint 2 end-to-end gates, load flow and invariants`.

### Task 10: Simulator network panel

**Files:**
- Create: `web/app/api/network/[...path]/route.ts`. It is guarded by `simulatorGuard`, signs with `NETWORK_WEBHOOK_SECRET` via `signWebhook`, and supports `repeat`.
- Modify: `web/app/page.tsx`. Add panel "5 · Merchant / network":
  - authorize form: amount, currency, merchant id/name/mcc, network_ref auto;
  - per-row Capture (amount), Reverse and Refund (amount);
  - concurrent sends;
  - Freeze/Unfreeze and purchase-limit controls in the Account panel;
  - a transactions table refreshed from `GET …/transactions`.
- Modify: `deploy/docker-compose.yml` (web env `NETWORK_WEBHOOK_SECRET`)
- Test: `web/lib/network.test.ts`. A `networkPath(op, authId?)` helper builds only the four allowed paths and rejects anything else.

- [ ] **Step 1:** Write the failing test: `networkPath("capture","abc")==="/v1/network/authorizations/abc/capture"`, `networkPath("evil")` throws, and an authId containing `/` throws.
- [ ] **Step 2:** Run `cd web && npm test`. Expected: FAIL.
- [ ] **Step 3:** Implement the helper, the route and the panel. Then run `npm run typecheck && npm run build`.
- [ ] **Step 4:** Run `npm test`. Expected: PASS. Run browser QA with /browse: authorize 300, capture 200, refund 50, freeze → declined `card_frozen`. No console errors.
- [ ] **Step 5:** Commit `feat(web): merchant/network simulator panel`.

### Task 11: Docs

**Files:** `docs/TESTING.md` (Sprint 2 results), `docs/SPRINTS.md` (mark Sprint 2 done), `docs/DECISIONS.md` (ADR-015 with the implementation rulings), `README.md` (network API section), `PROMPTS.md`.

- [ ] **Step 1:** Update the docs with the measured results and rulings.
- [ ] **Step 2:** Run `make test && make test-integration && make web-test && make tf-validate`. Expected: all PASS.
- [ ] **Step 3:** Commit `docs: sprint 2 results and decisions`.
