# Sprint 4 — Statements, Interest, Late Fees, Delinquency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every active account gets a monthly statement with interest, a minimum payment and a due date. A missed minimum leads to a late fee, a temporary block, and at 60 days a charge-off against the collateral. A per-account simulated clock drives all of it.

**Architecture:**
- **New `billing` service.** It is the only place where interest, fees and delinquency are decided. It derives principal / interest / fee buckets by replaying the ledger journal with pure functions in `services/billing/internal/engine`. It writes statement JSON to S3 and posts charges to the ledger as reserve → ledger → finalize sagas.
- **accounts** owns the clock offset, the delinquency stage, the `over_limit` restriction and the charge-off saga, because charge-off changes the credit limit.
- **ledger** gains `effective_at` (business date), income sub-accounts, and charge and journal-listing RPCs.

**Tech Stack:** Go 1.26, gRPC/protobuf (buf), pgx v5, chi, aws-sdk-go-v2 (S3), testcontainers (Postgres + LocalStack), k6, Next.js 16.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint4-statements-design.md`. Read §3, §4, §5, §5b.

## Global Constraints

**Clock**
- `account_now = clock.Now() + clock_offset_seconds`. The offset only grows.
- Advance: 1–400 days per call.
- The gateway registers `/v1/dev/...` only when `DEV_CLOCK_ENABLED=1`. Without it, the route returns 404.
- Account time drives: cycles, due dates, DPD, interest days, `payments.received_at`, and the `effective_at` of every ledger entry.
- Real time drives: the idempotency TTL, velocity, webhook tolerance, hold/intent expiry, the reversal and return windows, and the reconciler.

**Business date**
- `RequestMeta.effective_at` is optional. The ledger stores `effective_at = meta.effective_at ?? now()`.
- Every accounts saga captures `account_now` at reserve, persists it in the pending row, and reuses it on reconciler retry.

**Days and cycles**
- All days are UTC days of account time.
- `anchor_day = min(day(activated_at), 28)`.
- `close_N` = 00:00 UTC on `anchor_day` of the N-th month after the activation month (N ≥ 1).
- Cycle N covers `[close_{N-1}, close_N)`, with `close_0 = activated_at`.
- `due_N = close_N + 21 days`. A payment is on time if `received_at < due_N + 24h`. DPD 1 starts at `due_N + 24h`.

**Interest**
- `BILLING_APR_BPS` defaults to 2280.
- Per day: `micro += max(base,0) * apr_bps * 1_000_000 / (10_000*365)`, with integer division per day. `base` is the bucket balance at the end of the day.
- At close: `interest = micro / 1_000_000`; the remainder is dropped.
- `base`:
  - with grace kept: 0;
  - without grace: principal;
  - on days the account is past due (DPD ≥ 1): principal + interest + fees.
- Grace for cycle N:
  - N = 1, or
  - `new_balance_{N-1} ≤ 0`, or
  - credited in `[close_{N-1}, due_{N-1}+24h)` ≥ `new_balance_{N-1}`.
- Interest is posted with `effective_at = close_N − 1s`, reference `interest:<account_id>:<N>`. No posting when it is 0.
- No interest and no fees once the account is charged off.

**Minimum**
- `minimum_N = 0` if `new_balance_N ≤ 0`.
- Otherwise `min(new_balance_N, max(floor(currency), principal_N/100 + interest_billed_N + fees_billed_N + past_due_at_close_N))`.
- `floor` and `late_fee` come from `BILLING_FIXED_AMOUNTS`, default `USD:2500,EUR:2500,MXN:45000,COP:10000000`. With no entry, both are 0.

**Credited and delinquency (spec B-2)**
- Credited = payments `received_at` in the window that are settled and not returned, or pending/settling, plus waivers created in the window.
- Refunds never count.
- L = the latest statement with `due + 24h ≤ account_now`.
- `past_due = max(0, minimum_L − credited since close_L)`.
- S missed its due date ⇔ credited in `[close_S, due_S+24h)` < `minimum_S`.
- DPD = 0 if `past_due = 0`. Otherwise `DPD = floor((now − (due_K + 24h)) / 24h) + 1`, where K is the oldest statement in the unbroken run of missed statements ending at L.
- Late fee for S:
  - once, reference `late_fee:<account_id>:<N>`;
  - amount `min(late_fee(currency), minimum_S)`;
  - only when S missed its due date and nothing was pending in its window;
  - `effective_at = due_S + 24h` when processed in chronological catch-up, else `account_now`.
- Stages:
  - DPD ≥ 15 → `past_due`;
  - DPD ≥ 60 → charge-off (permanent `charged_off`);
  - `past_due = 0` → stage `none` (unless charged off).

**Allocation (replay, ordered by `(effective_at, entry id)`)**

| Entry type | Bucket effect |
|---|---|
| `purchase_captured` | +principal |
| `purchase_refunded` | −principal |
| `payment_settled`, `collateral_applied` | fees → interest → principal; any excess makes principal negative |
| `payment_returned` | restores exactly the buckets its `payment:<id>` reduced |
| `interest_charged` | +interest |
| `fee_charged` | +fees |
| `interest_waived` | −interest |
| `fee_waived` | −fees |

- Other entry types don't touch receivable.
- Invariant: `principal + interest + fees == ledger receivable` at every point.

**Sealed periods (closes the settle-vs-close race)**
- Ledger table `account_seals(account_id, sealed_through)`.
- `SealPeriod(account_id, t)` raises the watermark under the account mutex.
- Every posting stores `effective_at = max(requested, sealed_through)`, so a saga that reserved before a close but posts after it lands in the next cycle.
- Close order:
  1. replay and compute interest;
  2. post interest at `close−1s`;
  3. `SealPeriod(close)`;
  4. replay again for the totals;
  5. write the statement.
- An entry that races steps 1–3 is in the statement but not in that cycle's interest. That is accepted.

**Charge-off**
- At DPD 60:
  1. expire every approved authorization (hold → `expired`, auth → `expired`, the existing expiry path);
  2. post `collateral_applied` (debit `collateral_held` / credit `receivable`) for `min(max(receivable,0), collateral)`;
  3. `collateral_minor` and `credit_limit_minor` drop by that amount;
  4. set delinquency `charged_off`.
- Reference: `chargeoff:<account_id>`.
- Afterwards: statements are balance-only (minimum 0, no due date), payments are still allowed, purchases are declined.

**Declines (first match wins)**
1. `account_closed`
2. `charged_off`
3. `collateral_not_confirmed`
4. `currency_not_supported`
5. `payment_returned` | `over_limit` (from `restricted_reason`)
6. `past_due`
7. `card_frozen`
8. `velocity_limit_exceeded`
9. `purchase_limit_exceeded`
10. `insufficient_credit`

- The proto enum gains `PAST_DUE = 9`, `CHARGED_OFF = 10`, `OVER_LIMIT = 11`.

**Over limit**
- After billing posts interest or a fee, it calls accounts `CheckExposure`. If `holds + receivable > credit_limit` and `restricted_reason IS NULL`, accounts sets `restricted_reason='over_limit'`.
- The existing re-evaluation sweep lifts it the same way it lifts `payment_returned`.

**Waivers**
- JWT claim `role: "support"` is required.
- `amount_minor ≥ 1`, and `Σ waived ≤ charged` per charge (409 `waiver_exceeds_charge`). `reason` is 1–200 characters and required.
- Ledger reference `waiver:<waiver_id>`.

**Statements**
- `s3://$STATEMENTS_BUCKET/statements/<account_id>/<cycle>.json` (default bucket `acme-card-statements-dev`).
- The body is deterministic (sorted keys, fixed field order). `sha256` of the body is stored on the row.
- Row unique on `(account_id, cycle)`.

**Events (outbox → audit)**
- `account.clock_advanced`, `account.delinquency_changed`, `account.charged_off`, `card.restricted` (reason `over_limit`)
- `statement.generated`, `interest.charged`, `fee.charged`, `charge.waived`
- `ledger.charge_posted`, `ledger.collateral_applied`

**Process**
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Stage explicit paths only (never `git add -A`; another session may share the checkout).
- Next migrations: accounts `000012`, ledger `000004`, billing `000001`. Next ADR: ADR-019.
- TDD: the failing test first.

## Review Focus

1. **A payment pending across a due date that later fails.** The fee and DPD must be computed from the original due date. Catch-up may block or charge off at once, without double-posting the fee. Covered by a test in Task 6.
2. **Clock advance racing a payment settlement or a purchase on the same account.** No entry may get an `effective_at` inside an already-closed cycle, and buckets must still equal receivable. Covered by a test in Task 6: a settlement during catch-up.
3. **Charge-off while a capture of an approved authorization is in flight.** Exposure must never exceed the reduced limit, and the hold/authorization invariant must hold. Covered by a test in Task 4.
4. **A waiver on an already-waived or partially waived charge, submitted concurrently.** Total waived never exceeds the charge. Covered by a test in Task 7.
5. **Advancing 400 days on an account with a credit balance and no activity.** The result is 13 zero statements, no interest, no fees and no delinquency. Covered by a test in Task 6.

---

### Task 1: Contracts

**Files:**
- Modify:
  - `proto/acme/common/v1/common.proto`
  - `proto/acme/ledger/v1/ledger.proto`
  - `proto/acme/accounts/v1/accounts.proto`
- Create: `proto/acme/billing/v1/billing.proto`
- Regenerate: `gen/go/...`
- Modify: `services/ledger/ledgertest/ledgertest.go` (adapter methods for the new RPCs)

**Interfaces (produced):**
- **common**
  - `RequestMeta.effective_at` (`google.protobuf.Timestamp`, field 3);
  - `DeclineReason` `PAST_DUE=9`, `CHARGED_OFF=10`, `OVER_LIMIT=11`.
- **ledger**
  - `SubAccount` `INTEREST_INCOME=5`, `FEE_INCOME=6`.
  - `rpc PostCharge(PostChargeRequest) returns (PostJournalResponse)`. Request fields: `account_id`, `reference`, `ChargeKind kind`, `amount_minor`, `meta`. `ChargeKind` is `INTEREST=1 | FEE=2 | INTEREST_WAIVER=3 | FEE_WAIVER=4`, with entry types `interest_charged | fee_charged | interest_waived | fee_waived`.
  - `rpc PostCollateralApplication(PostCollateralApplicationRequest) returns (PostJournalResponse)`. Request fields: `account_id`, `reference`, `amount_minor`, `meta`.
  - `rpc SealPeriod(SealPeriodRequest{account_id, sealed_through}) returns (SealPeriodResponse{sealed_through})`. It is monotonic: a lower value is a no-op.
  - `rpc ListEntries(ListEntriesRequest) returns (ListEntriesResponse)`. Request: `account_id`, `effective_from`, `effective_to` (half-open, either may be unset), `page_token`, `page_size` (≤ 1000). Response: repeated `JournalEntry{id, reference, entry_type, effective_at, receivable_delta_minor (signed: debit +, credit −)}`, plus `next_page_token`.
- **accounts**
  - `Account` gains `account_time`, `activated_at`, and `delinquency` (`"none"|"past_due"|"charged_off"`).
  - `Payment` gains `received_at`.
  - RPCs:
    - `AdvanceClock(AdvanceClockRequest{account_id, days, meta}) returns (GetAccountResponse)`
    - `SetDelinquency(SetDelinquencyRequest{account_id, stage, meta}) returns (GetAccountResponse)`
    - `ChargeOff(ChargeOffRequest{account_id, meta}) returns (GetAccountResponse)`
    - `CheckExposure(CheckExposureRequest{account_id, meta}) returns (GetAccountResponse)`
    - `ListAccountIDs(ListAccountIDsRequest{after_id, limit}) returns (ListAccountIDsResponse{ids})` (active accounts only)
- **billing** (`acme.billing.v1.BillingService`)
  - `CatchUp(account_id) → CatchUpResponse{statements_generated}`
  - `ListStatements(account_id) → {statements[]}`
  - `GetStatement(account_id, statement_id) → {statement, body_json}`
  - `GetSummary(account_id) → BillingSummary`, with fields `account_time`, `cycle_start`, `cycle_end`, `last_statement`, `minimum_due_minor`, `due_date`, `past_due_minor`, `dpd`, `grace`, `stage`
  - `ListCharges(account_id) → {charges[]}`
  - `WaiveCharge(account_id, charge_id, amount_minor, reason, meta) → {charge, waiver}`
  - `Statement{id, account_id, cycle, period_start, period_end, due_date, previous_balance, purchases, refunds, payments, returns, interest, fees, waivers, collateral_applied, new_balance, minimum_due, principal, interest_bucket, fee_bucket, s3_key, sha256, generated_at}`, all amounts `*_minor`
  - `Charge{id, kind, cycle, amount_minor, waived_minor, effective_at}`

- [ ] **Step 1:** Edit the protos. Run `buf lint && buf generate`. Expected: no output; `go build ./...` passes.
- [ ] **Step 2:** Add `ledgertest` adapter methods for `PostCharge`, `PostCollateralApplication` and `ListEntries`. Run `go build ./... && go vet ./services/...`. Expected: clean.
- [ ] **Step 3:** Commit `feat(proto): Sprint 4 contracts (billing service, charges, clock, delinquency)`.

### Task 2: Ledger — effective_at, income sub-accounts, charges, collateral application, ListEntries

**Files:**
- Create: `services/ledger/migrations/000004_billing.{up,down}.sql`
  - add `journal_entries.effective_at timestamptz NOT NULL DEFAULT now()`, backfilled with `posted_at`;
  - create table `account_seals`;
  - index `(account_id, effective_at, id)`;
  - extend the sub_account CHECK with `interest_income`, `fee_income`;
  - `OpenLedger` creates the two new rows, and the migration inserts them for existing accounts.
- Create: `services/ledger/internal/ledger/charges.go`
- Modify: `services/ledger/internal/ledger/service.go` (`PostJournal` stores `meta.effective_at`), `holds.go` / `payments.go` (pass meta through)
- Test: `services/ledger/internal/ledger/charges_integration_test.go`

**Interfaces:**
- Produces: the ledger RPCs from Task 1, plus the seal clamp on every posting path. Every one takes the `collateral_held` FOR UPDATE account mutex, is idempotent on `reference`, and emits outbox `ledger.charge_posted` / `ledger.collateral_applied`.
- Posting rules:
  - INTEREST: debit `receivable` / credit `interest_income`.
  - FEE: debit `receivable` / credit `fee_income`.
  - Waivers: the reverse.
  - Collateral application: debit `collateral_held` / credit `receivable`. Fail `FailedPrecondition` if `amount > collateral`.

- [ ] **Step 1: Failing tests.**
  - `TestEffectiveAtDefaultsToNowAndHonoursMeta`: post with `meta.effective_at = 2027-01-15T00:00Z`; ListEntries returns that `effective_at`. Without it, `|effective_at − posted_at| < 1s`.
  - `TestPostChargeKinds`: four kinds on an account; receivable goes +1000 +2500 −300 −2500, so it ends at +700. `interest_income` raw = −700. The journal stays balanced.
  - `TestPostChargeIdempotentOnReference`: 50 concurrent identical calls → one entry; 49 replies have `replayed=true`.
  - `TestPostChargeReferenceReuseDifferentAmount` → `AlreadyExists`.
  - `TestCollateralApplication`: collateral 100000, receivable 30000; apply 30000 → collateral_held raw −70000, receivable 0. Applying 80000 on a fresh account with collateral 50000 → `FailedPrecondition`.
  - `TestSealPeriodClampsLaterPostings`: seal at T; a post with `effective_at` T−1h gets T; sealing at T−2h afterwards leaves T.
  - `TestListEntriesOrderAndRange`: entries at t1 < t2 < t3; range `[t2, ∞)` returns t2 and t3 in `(effective_at, id)` order. Paging with `page_size=1` yields the same sequence. Purchase capture delta is +, payment −.
- [ ] **Step 2:** Run `go test -tags=integration ./services/ledger/...`. Expected: FAIL (undefined RPCs / missing column).
- [ ] **Step 3:** Implement the migration, `charges.go`, and the `effective_at` pass-through.
- [ ] **Step 4:** Rerun. Expected: PASS. All existing ledger tests still pass.
- [ ] **Step 5:** Commit `feat(ledger): effective_at, income sub-accounts, charges, collateral application, ListEntries`.

### Task 3: Accounts — clock offset, account time, business dates in sagas

**Files:**
- Create: `services/accounts/migrations/000012_billing.{up,down}.sql`. On `accounts`, add:
  - `clock_offset_seconds bigint NOT NULL DEFAULT 0 CHECK (≥0)`;
  - `activated_at timestamptz`, backfilled for active accounts with the `updated_at` of the first settled deposit, else `updated_at`;
  - `delinquency text NOT NULL DEFAULT 'none' CHECK (IN ('none','past_due','charged_off'))`.
  Also add `payments.received_at timestamptz NOT NULL DEFAULT now()`, and an `effective_at timestamptz` column on `deposits`, `authorizations`, `refunds` and `payments`, set at reserve.
- Create: `services/accounts/internal/accounts/clock.go`
- Modify:
  - `service.go`: account row/proto fields, `accountNow`, and setting `activated_at` on activation;
  - every saga that posts to the ledger (`SettleDeposit`, capture/reverse/refund in `postauth.go`, `payments_settle.go`, `payments_return.go`, and `expiry.go` `ResolvePending`), so each passes the persisted `effective_at` in meta;
  - `payments.go`: `received_at = accountNow` at creation.
- Test: `services/accounts/internal/accounts/clock_integration_test.go`

**Interfaces:**
- Produces:
  - `func (s *Service) accountNow(acc accountRow) time.Time`
  - `func (s *Service) AdvanceClock(ctx, *AdvanceClockRequest) (*GetAccountResponse, error)`: validates 1 ≤ days ≤ 400 (`InvalidArgument`) and that the account is not closed; adds `days*86400`; emits `account.clock_advanced`.
  - The `Account` proto fills `account_time`, `activated_at` and `delinquency`.
- Consumes: `RequestMeta.effective_at` (Task 1).

- [ ] **Step 1: Failing tests.**
  - `TestAdvanceClockMovesAccountTimeOnly`: advance 30 → `account_time ≈ now+30d`; another account is unchanged; days 0 or 401 → InvalidArgument.
  - `TestPostingsCarryAccountTime`: advance 10 days, then settle a payment → the ledger entry `effective_at ≈ now+10d`, and `payment.received_at ≈ now+10d` (at creation).
  - `TestReconcilerReusesPersistedEffectiveAt`: settle with the ledger failing after the reserve, advance the clock 5 more days, then run `ResolvePending` → the entry `effective_at` is the reserve-time value, not +5d.
  - `TestActivatedAtSetOnActivation`.
- [ ] **Step 2:** Run `go test -tags=integration ./services/accounts/...`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS, and existing accounts suites green.
- [ ] **Step 5:** Commit `feat(accounts): per-account simulated clock and business dates on every posting`.

### Task 4: Accounts — delinquency, over_limit, charge-off, declines, ListAccountIDs

**Files:**
- Create: `services/accounts/internal/accounts/delinquency.go`
- Modify:
  - `decline.go`: `DeclineInput` gains `Delinquency string` and `RestrictedReason string` (replacing `Restricted bool`); new reasons; the order from Global Constraints;
  - `authorize.go`: fill them;
  - `restriction.go`: `restrictInTx(ctx, tx, accountID, reason, meta)` keeps an existing reason.
- Test: `decline_test.go` and `services/accounts/internal/accounts/delinquency_integration_test.go`

**Interfaces:**
- Produces:
  - `SetDelinquency`: `none|past_due` only; it can never leave `charged_off`, so `FailedPrecondition` there. Emits `account.delinquency_changed` on change.
  - `CheckExposure`: restricts with `over_limit` when over the limit.
  - `ChargeOff`: idempotent; it is a no-op on an account already `charged_off`. It runs the reserve → ledger → finalize saga from Global Constraints (pending marker `accounts.charge_off_pending_at`, resumed by `ResolvePending`).
  - `ListAccountIDs`.

- [ ] **Step 1: Failing tests.**
  - `decline_test.go` table: every pair of adjacent reasons in the new order; `charged_off` beats `collateral_not_confirmed`; `over_limit` and `payment_returned` both map from `RestrictedReason`; `past_due` beats `card_frozen`.
  - `TestChargeOffAppliesCollateralAndBlocks`:
    - setup: collateral 100000, captured 30000, one approved authorization of 5000;
    - after charge-off: the authorization and hold are `expired`; collateral and limit are 70000; receivable 0; delinquency `charged_off`;
    - an authorization then declines `charged_off`; payments are still accepted (`nothing_to_pay` here);
    - charge-off ×50 concurrently → one `collateral_applied` entry.
  - `TestChargeOffDebtAboveCollateral`: collateral 50000, receivable 60000 → applied 50000, limit 0, receivable 10000.
  - `TestChargeOffRacesCapture`: the capture of the approved authorization runs concurrently with charge-off. Either the capture wins (receivable includes it before application) or it fails with the expired-hold error. Afterwards `holds + receivable ≤ limit`, or the overage is restricted, and the hold/auth states are consistent.
  - `TestCheckExposureRestrictsOverLimit`: interest posted directly through the ledger pushes receivable over the limit → `CheckExposure` sets `over_limit`; a payment settlement lifts it.
  - `TestSetDelinquencyCannotLeaveChargedOff`.
- [ ] **Step 2:** Run the decline unit test and the accounts integration tests. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS; Sprint 2/3 decline tests updated only where the enum order changed.
- [ ] **Step 5:** Commit `feat(accounts): delinquency stages, over_limit restriction, charge-off saga, new decline order`.

### Task 5: Billing engine (pure, no I/O)

**Files:**
- Create in `services/billing/internal/engine/`:
  - `cycle.go`
  - `replay.go`
  - `interest.go`
  - `minimum.go`
  - `delinquency.go`
  - `statement.go`
- Tests: `*_test.go` beside each file.

**Interfaces (produced):**
- `type Entry struct{ ID, Reference, Type string; EffectiveAt time.Time; Delta int64 }`
- `type Buckets struct{ Principal, Interest, Fees int64 }` with `func (b Buckets) Total() int64`
- `type Credit struct{ ReceivedAt time.Time; Amount int64; Pending bool }`, used for payments counted as credited and for waivers
- `func CycleClose(activatedAt time.Time, n int) time.Time`
- `func DueDate(close time.Time) time.Time`
- `func Replay(start Buckets, entries []Entry) (Buckets, []DayBalance, Totals)`
  - `DayBalance` holds the end-of-day buckets per UTC day.
  - `Totals` holds `Purchases, Refunds, Payments, Returns, Interest, Fees, Waivers, CollateralApplied`.
  - Payment allocations are tracked by reference so a return restores them.
- `func Interest(days []DayBalance, aprBps int64, grace bool, pastDue func(day time.Time) bool) int64`
- `func Minimum(newBalance int64, b Buckets, interestBilled, feesBilled, pastDueAtClose, floor int64) int64`
- `type StatementFacts struct{ Cycle int; Close, Due time.Time; Minimum int64 }`
- `func Delinquency(stmts []StatementFacts, credits []Credit, now time.Time) (pastDue int64, dpd int, missed []int)`
  - `missed` lists the cycles that missed their due date with nothing pending, i.e. those owed a fee.
- `func Grace(prev *StatementFacts, prevNewBalance int64, credits []Credit) bool`
- `func RenderStatement(s StatementDoc) ([]byte, string)`: deterministic JSON and its sha256 hex.

- [ ] **Step 1: Failing tests, all exact integers.**
  - `TestCycleClose`:
    - activation 2027-01-31 → closes 2027-02-28, 2027-03-28 (anchor 28);
    - activation 2027-01-10 → 2027-02-10;
    - `DueDate(2027-02-10) = 2027-03-03`;
    - 13 closes from activation 2027-01-10 fall within 400 days.
  - `TestReplayAllocation`:
    - capture 100000, fee 2500, interest 1500 → total 104000;
    - payment 3000 → fees 0, interest 1000, principal 100000 (total 101000);
    - refund 1000 → principal 99000;
    - return of that payment → fees 2500, interest 1500, principal 99000 (total 103000);
    - a waiver of 500 interest → interest 1000.
  - `TestReplayExcessPaymentMakesCredit`: principal 1000, payment 1500 → principal −500.
  - `TestInterestGraceKept`: any balances, grace=true, no past-due days → 0.
  - `TestInterestGraceLost`: principal 100000 for 30 days at 2280 bps → per day `100000*2280*1_000_000/3_650_000 = 62_465_753` micro → ×30 = 1_873_972_590 → **1873**.
  - `TestInterestPastDueIncludesInterestAndFees`: principal 100000, interest 2000, fees 2500, past due for all 30 days → base 104500 → per day `104500*2280*1_000_000/3_650_000 = 65_276_712` → ×30 → **1958**.
  - `TestInterestCreditBalanceZero`: principal −5000 → 0.
  - `TestMinimum`:
    - principal 100000, interest 1500, fees 2500, past due 0, floor 2500 → `1000+1500+2500 = 5000`;
    - principal 1000, no charges → floor 2500, capped at new balance 1000 → **1000**;
    - new balance 0 → 0;
    - past due 5000 added → 10000.
  - `TestGrace`: no previous statement → true; previous new balance 50000 with credited 50000 by `due+24h` → true; 49999 → false; a credit received at `due+24h` exactly → not counted (false).
  - `TestDelinquency`:
    - one statement with minimum 5000 and nothing credited, now = due+24h+14d → dpd 15, pastDue 5000, missed [1];
    - same with a pending credit of 5000 received before due → dpd 0, missed [];
    - that credit no longer pending and failed (absent from credits) → dpd 15 again;
    - two consecutive missed statements → dpd counted from the first due;
    - a late payment covering the past due → dpd 0, but missed still [1] (the fee is still owed);
    - a waiver of 5000 → dpd 0.
  - `TestRenderStatementDeterministic`: the same doc twice → identical bytes and hash; different docs → different hash.
- [ ] **Step 2:** Run `go test ./services/billing/internal/engine/...`. Expected: FAIL (package missing).
- [ ] **Step 3:** Implement. Tests use fixed instants (activation 2027-01-10T15:00Z), never `time.Now()`.
- [ ] **Step 4:** Rerun. Expected: PASS.
- [ ] **Step 5:** Commit `feat(billing): pure engine (cycles, replay allocation, interest, minimum, delinquency, statement JSON)`.

### Task 6: Billing service — store, catch-up, S3, worker, wiring

**Files:**
- Create:
  - `services/billing/migrations/000001_billing.{up,down}.sql` + `migrations.go`
    - tables: `outbox`; `statements` (unique `(account_id, cycle)`); `charges` (unique `reference`; `status pending|posted|failed`; `waived_minor`); `waivers` (unique `idempotency_key_hash`); `billing_accounts` (`account_id` PK, `last_cycle`, `last_close_buckets`, `stage`, `updated_at`), used as the per-account catch-up lock (FOR UPDATE).
  - `services/billing/internal/billing/service.go`, `catchup.go`, `store.go`
  - `services/billing/internal/billing/objectstore.go`: `type ObjectStore interface{ Put(ctx, key string, body []byte) error; Get(ctx, key string) ([]byte, error) }`, with an S3 implementation and a `MemStore` for unit tests
  - `services/billing/cmd/main.go`
- Modify:
  - `pkg/awsx/awsx.go`: add an `S3 *s3.Client` with path-style addressing for LocalStack;
  - `pkg/config/config.go`: `BillingGRPCAddr` (default `billing:9004`), `AccountsGRPCAddr` if absent, `StatementsBucket`, `BillingAPRBps`, `BillingFixedAmounts`;
  - `deploy/docker-compose.yml`: `billing` service; `GATEWAY` gets `BILLING_GRPC_ADDR`;
  - `Makefile`.
- Test: `services/billing/internal/billing/catchup_integration_test.go` (Postgres + LocalStack testcontainers, real ledger and accounts services in-process, as the existing gateway tests do)

**Interfaces:**
- Consumes: Task 1–4 RPCs and the Task 5 engine.
- Produces:
  - the `BillingService` server;
  - `func (s *Service) RunSweep(ctx, every time.Duration, log)`: calls `ListAccountIDs` pages and `CatchUp` for each.
- `CatchUp(account)` algorithm, under `billing_accounts` FOR UPDATE, looping in order:
  1. While `close_{last+1} ≤ account_now` (after charge-off: balance-only, skip b and the minimum):
     - a. replay entries in `[close_last, close_{last+1})`;
     - b. compute grace and interest; post interest (reserve a charge row, ledger `PostCharge` with `effective_at = close−1s`, finalize);
     - c. `SealPeriod(close)`, then replay again through the close to get totals and buckets;
     - d. compute the minimum;
     - e. write S3, then insert the statement row + outbox `statement.generated`;
     - f. `last++`.
     Before each close, any fee/stage event whose scheduled time is < that close is processed first (step 2 evaluated at that time).
  2. Delinquency at `account_now` (or at each scheduled time during the loop):
     - post owed late fees;
     - DPD ≥ 60 → `ChargeOff`; else DPD ≥ 15 → `SetDelinquency(past_due)`; else past due 0 → `SetDelinquency(none)`.
  3. After any charge: `CheckExposure`.
- Charge sagas reuse the deposit pattern: a pending charge row, the ledger call outside the lock with a detached 15s context, then finalize. Pending charges older than 2 min are resumed at the start of `CatchUp`.
- Credits come from accounts `ListPayments`, using `received_at` plus status (`pending`/`settling` = pending; `settled` = credited; `returned`/`failed`/`expired` = ignored), plus billing's own waivers.

- [ ] **Step 1: Failing integration tests.**
  - `TestCatchUpEmptyAccount400Days`: activate, advance 400 → 13 statements, all zero, minimum 0, no charges, stage `none`, S3 objects present with hashes matching the rows.
  - `TestStatementReconcilesWithLedger`: purchase 100000, advance to after close 1 → statement 1: purchases 100000, interest 0 (grace), new balance 100000 = `Σ ListEntries` deltas `< close_1`, minimum 2500 (`1%=1000 < floor`).
  - `TestInterestAfterGraceLost`:
    - pay nothing; advance past due_1 and close_2;
    - statement 2 has a late fee of 2500 (posted at `due_1+24h`) and interest equal to the engine value computed in-test from the same day balances (not hard-coded);
    - minimum_2 includes past due 2500.
  - `TestPendingPaymentAtDueDateThenFails`:
    - create a 2500 payment before due_1 (pending); advance past due_1 + 20 days → no fee, stage `none`;
    - the bank fails it, then `CatchUp` → fee posted once (`effective_at = account_now`), DPD 20 → stage `past_due`;
    - `CatchUp` ×50 concurrently → still one fee.
  - `TestPendingPaymentSettlesLate`: same, but the bank settles it → no fee ever.
  - `TestBlockAndCure`: miss the minimum; advance to DPD 15 → authorizations decline `past_due`; pay the past-due amount and catch up → stage `none`, and purchases are approved again.
  - `TestChargeOffAtDPD60`: miss repeatedly through DPD 60 → collateral applied, `charged_off`, later statements balance-only, no interest after charge-off.
  - `TestSettlementDuringCatchUp` (Review Focus 2): one goroutine runs `AdvanceClock(60)+CatchUp` while another settles payments. Afterwards, at every statement close, buckets.Total and new_balance == the ledger `Σ delta` with `effective_at < close`.
  - `TestCatchUpConcurrent50`: 50 concurrent `CatchUp` calls after a 60-day advance → exactly 2 statements, one interest charge per cycle, one fee.
- [ ] **Step 2:** Run `go test -tags=integration ./services/billing/...`. Expected: FAIL.
- [ ] **Step 3:** Implement the migrations, store, catch-up, S3 store, sweep, `main.go`, config and compose. Add `deploy/postgres-init.sql` schema `billing` if schemas are created there.
- [ ] **Step 4:** Rerun. Expected: PASS. Run `make build && docker compose ... config -q`. Expected: OK.
- [ ] **Step 5:** Commit `feat(billing): billing service with catch-up, statements in S3, late fees, delinquency, charge-off`.

### Task 7: Waivers and the support role

**Files:**
- Modify:
  - `pkg/auth/jwt.go` (or wherever `Claims` lives): `Role string \`json:"role,omitempty"\``; `SignWithRole(secret, sub, accounts, role, ttl)`; `RequireRole(role) func(http.Handler) http.Handler`, which returns 403 `forbidden`;
  - `cmd/devtoken/main.go`: `--role`.
- Create: `services/billing/internal/billing/waivers.go`
- Test: `pkg/auth` unit tests, `services/billing/internal/billing/waivers_integration_test.go`

**Interfaces:**
- Produces: `WaiveCharge`.
  1. Lock the charge FOR UPDATE.
  2. Check `waived + amount ≤ amount_minor`, else `FailedPrecondition waiver_exceeds_charge`.
  3. Insert the waiver as pending and bump `waived_minor`.
  4. Call ledger `PostCharge(INTEREST_WAIVER|FEE_WAIVER, reference waiver:<id>)`.
  5. Finalize, emit `charge.waived`, then `CatchUp` (which may cure delinquency).
  6. On ledger failure, undo the waiver and the `waived_minor` bump in finalize.
- Idempotent on `meta.idempotency_key_hash`.

- [ ] **Step 1: Failing tests.**
  - `TestRequireRole`: a customer token → 403; a support token → passes.
  - `TestWaiverPartialThenFull`: fee 2500, waive 1000 then 1500 → `waived_minor` 2500; a further 1 → `waiver_exceeds_charge`.
  - `TestWaiversConcurrentNeverExceed`: 20 concurrent waivers of 200 on a 2500 fee, each with a distinct key → exactly 12 succeed; `waived_minor` 2400; ledger `Σ fee_waived` 2400.
  - `TestWaiverCuresPastDue`: past due 2500 made of the fee → waive it → stage `none`.
  - `TestWaiverIdempotentKey`: 50 calls with the same key → one waiver.
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS.
- [ ] **Step 5:** Commit `feat(billing): support waivers of interest and fees with role-gated access`.

### Task 8: Gateway

**Files:**
- Create: `services/gateway/internal/server/billing.go`
- Modify:
  - `server.go`: `Deps.Billing billingv1.BillingServiceClient`, `Deps.DevClock bool`; routes; error mapping for `waiver_exceeds_charge` → 409, `charge_not_found` → 404; account JSON fields `account_time`, `activated_at`, `delinquency`;
  - `services/gateway/cmd/main.go`: dial billing; `DEV_CLOCK_ENABLED`.
- Test: `services/gateway/internal/server/billing_test.go`

**Routes:**
- Customer, inside `/v1/accounts/{accountID}`:
  - `GET /statements`
  - `GET /statements/{statementID}`: returns the row plus `document` (the parsed S3 JSON)
  - `GET /billing`
  - `GET /charges`
- Support group: JWT + `RequireRole("support")` + rate limit + audit + idempotency.
  - `POST /v1/support/accounts/{accountID}/charges/{chargeID}/waivers` with `{amount_minor, reason}` → 201.
- Dev group (only if `DevClock`): JWT + `RequireAccount` + audit + idempotency.
  - `POST /v1/dev/accounts/{accountID}/clock/advance` with `{days}` → calls accounts `AdvanceClock`, then billing `CatchUp`, then returns `{account, billing}` with 200.

- [ ] **Step 1: Failing tests.**
  - `TestDevClockRouteAbsentWhenDisabled` → 404.
  - `TestDevClockAdvanceIdempotent`: ×50 with the same key → one `AdvanceClock` call on the fake, and identical bodies.
  - `TestStrictJSONOnNewRoutes`: an unknown field → 400.
  - `TestSupportWaiverRequiresRole`: a customer token → 403.
  - `TestStatementDetailIncludesDocument`.
  - `TestAccountJSONHasBillingFields`.
  - `TestSimulatorCoverage` (existing) fails until Task 9 adds the routes to `web/lib/ui-actions.json`; mark the ordering in the ledger.
- [ ] **Step 2:** Run `go test ./services/gateway/...`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Rerun. Expected: PASS except `TestSimulatorCoverage`, which is green after Task 9.
- [ ] **Step 5:** Commit `feat(gateway): statements, billing summary, charges, support waivers, dev clock`.

### Task 9: Simulator — "7 · Billing"

**Files:**
- Modify:
  - `web/lib/ui-actions.json`: the new routes;
  - `web/app/page.tsx`: panel "7 · Billing" with:
    - advance +1d, +N days, to next close, to due + N;
    - the summary;
    - a statements list with a detail viewer (pretty JSON);
    - charges with a support "Waive" form (amount + reason);
    - stats for account time, DPD, stage, minimum due and due date.
    Every control carries `data-action`.
  - `web/app/api/token/route.ts`: accepts `role` (support) server-side;
  - compose `web` / `gateway` env `DEV_CLOCK_ENABLED=1`.
- Create: `web/lib/billing.ts` + `web/lib/billing.test.ts` (formatting helpers, and the "to next close" / "to due + N" day math from the summary)
- Test: `web/lib/actions.test.ts`, the gateway `TestSimulatorCoverage`

- [ ] **Step 1: Failing tests.**
  - `billing.test.ts`: `daysToNextClose(summary)`, where `account_time 2027-02-05T10:00Z` and `cycle_end 2027-02-10` gives 5; `daysToDuePlus(summary, 1)`; the money formatting of minor units.
  - `actions.test.ts`: every `ui-actions.json` entry has a matching `data-action` in `page.tsx`.
- [ ] **Step 2:** Run `make web-test`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `make web-test && go test ./services/gateway/...`. Expected: PASS, including `TestSimulatorCoverage`.
- [ ] **Step 5:** Run `make up`. Then use the `/browse` skill against `http://127.0.0.1:3010`: open an account → deposit → settle → purchase → advance to the next close → the statement shows → advance past due + 15 → an authorization declines `past_due` → waive the fee. Save a screenshot to the workspace.
- [ ] **Step 6:** Commit `feat(web): billing panel (clock, statements, charges, waivers)`.

### Task 10: Testing flow — end to end, load, invariants

**Files:**
- Create: `tests/integration/sprint4_test.go`
- Modify: `tests/load/flows.js`, `tests/load/verify.sql`
- Create: `docs/testing/sprint4.md`

- [ ] **Step 1: Failing test `TestSprint4EndToEnd`**, through the gateway with `DEV_CLOCK_ENABLED`:
  1. open → deposit 100000 → settle → purchase 60000 captured;
  2. advance to close 1 → statement 1 with minimum 2500 and grace;
  3. pay 2500 on time → advance to close 2 → no fee; interest > 0 (grace lost, because the full balance was not paid);
  4. miss minimum 2 → advance to DPD 15 → purchase declined `past_due`;
  5. a support token waives the fee;
  6. advance to DPD 60 → `charged_off`, limit reduced, and the S3 documents match the hashes;
  7. audit has `statement.generated`, `fee.charged`, `charge.waived` and `account.charged_off` events.
- [ ] **Step 2:** Run `go test -tags=integration -run TestSprint4 ./tests/integration/`. Expected: FAIL before wiring is complete, then PASS.
- [ ] **Step 3: `verify.sql` additions (each must report 0).**
  - statement new balance ≠ `Σ` receivable deltas of `ledger.journal_entries` with `effective_at < period_end`;
  - more than one interest or fee charge per `(account, cycle)`;
  - `waived_minor > amount_minor`, or `waived_minor ≠ Σ` posted waivers;
  - a posted charge with no matching journal entry, or the reverse;
  - a charged-off account that has active holds, or whose `credit_limit ≠ −collateral_held`;
  - a pending billing charge older than 5 min.
  - Rewrite the exposure check: "exposure > limit not explained by returns + Σ (interest + fees − waivers)" must be 0, and exposure > limit with `restricted_reason IS NULL` and stage ≠ `charged_off` must be 0.
  - Corruption test in a rolled-back transaction: insert a duplicate fee row → that check reports 1.
- [ ] **Step 4: k6 `flows.js`.** After the Sprint 3 steps, each iteration:
  - advances its account 31 days (the statement exists);
  - pays the minimum in 2 of every 3 iterations, otherwise advances 20 more days (fee);
  - advances 20 again;
  - checks that `GET /billing` returns 200 with `stage ∈ {none, past_due}`.
  Keep the 409 `request_in_progress` retry and the balance checks only on fully successful iterations.
- [ ] **Step 5:** Run `make up && make load` (1000 VUs). Expected:
  - every `verify.sql` row is 0;
  - k6 checks pass at > 99.5%;
  - record request count, failure %, p95, and statements/fees generated (SQL counts) in `docs/testing/sprint4.md`.
  If p95 misses the 1.5 s gate on this host, record the result and compare it with `main` in an isolated A/B compose project (`-p acme-card-ab-<label>`, fresh volumes), as in Sprint 3.
- [ ] **Step 6:** Commit `test: Sprint 4 end-to-end, billing invariants, load with cycle closes`.

### Task 11: Docs

**Files:**
- `docs/DECISIONS.md`: ADR-019 covering billing, the clock, business dates, buckets by replay, delinquency derivation, charge-off and waivers.
- `docs/SPRINTS.md`: Sprint 4 done.
- `README.md`: billing/support/dev API tables, `DEV_CLOCK_ENABLED`, devtoken `--role`.
- `services/billing/README.md`.
- `PROMPTS.md`.

- [ ] **Step 1:** Write them. Run `make test && make test-integration && make web-test && make tf-validate`. Expected: all green.
- [ ] **Step 2:** Commit `docs: Sprint 4 billing (ADR-019, sprints, README)`.
