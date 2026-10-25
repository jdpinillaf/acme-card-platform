# Customer Journey End-to-End Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One end-to-end test that walks a customer through Sprints 1–4 (open → deposit → spend → statements → pay → delinquency → charge-off), asserts every business outcome, runs an "intruder" at each stage, and reports per-step HTTP latency and async delivery times without thresholds.

**Architecture:** Reuses the in-process `stack` harness in `tests/integration` (real gRPC services, real gateway handler, Postgres + LocalStack via testcontainers). A small `journal` type wraps each call to record duration and polls Postgres for async outcomes (account state, `audit_events`). Security probes are helpers in their own file so the journey reads as a story. Nothing in product code changes except two test-only knobs (velocity and rate limit) exposed through `accountstest` and a `stackConfig`.

**Tech Stack:** Go 1.26, `testing`, testcontainers (existing `testutil`), `github.com/golang-jwt/jwt/v5` (already a dependency), pgx.

**Spec:** Agreed in chat on 2026-10-25 (no separate design doc): full lifecycle, timings reported not gated, intruder scenarios: tenancy, tokens, webhooks, idempotency, data exposure, business limits, rate limit. Results and security findings go to `docs/testing/journey.md`; failures are reported, not fixed.

## Global Constraints

- Build tag `//go:build integration` on every new test file; run with `make test-integration` (`-race -count=1 -timeout=10m`).
- No thresholds in v1: timings are logged, never asserted.
- Journey state flows phase to phase: phases are sequential `t.Run`s sharing one `stack`, one customer, one `journal`. A failing phase stops the test (`t.Fatalf`), later phases are skipped.
- Journey stack knobs: card velocity 30 attempts/min (default is 10, the journey needs more than 10 authorizations on one card within a minute); gateway rate limit 500/min per subject (default in tests is 100,000).
- Error envelope is always `{"error":{"code":..,"message":..}}`; assert on `code`, never on `message`.
- Keep the product's English for code and docs.

## Review Focus

1. Deposit/payment webhook with `amount_minor` ≤ 0 or an unknown JSON field → 400 `invalid_body` (strict decoder), and nothing changes. Pinned in Task 4, phase 1 and phase 3.
2. A statement id that belongs to customer A requested under customer B's own account path → 404, never the document (IDOR through a child resource). Pinned in Task 4, phase 5.
3. Tampered `page_token` on `GET /transactions` → 400 `invalid_page_token`, no 500. Pinned in Task 4, phase 2.
4. Webhook body over the size cap → 413 `body_too_large` before any signature work. Pinned in Task 4, phase 1.
5. The 403 body for an account the intruder is not authorized on must be byte-identical whether the account exists or not (no existence oracle). Pinned in Task 4, phase 1.

---

### Task 1: Test-only knobs — velocity and rate limit

**Files:**
- Modify: `services/accounts/accountstest/accountstest.go`
- Modify: `tests/integration/sprint1_test.go:60-140` (`startStack`)

**Interfaces:**
- Produces: `accountstest.Option` (alias of `accounts.Option`), `accountstest.WithVelocity(limit int, window time.Duration) Option`, `accountstest.NewServer(pool, ledger, opts ...Option)`.
- Produces: `type stackConfig struct { RateLimitPerMinute int; Accounts []accountstest.Option }`, `func startStackWith(t *testing.T, cfg stackConfig) *stack`; `startStack(t)` = `startStackWith(t, stackConfig{RateLimitPerMinute: 100_000})`.

- [ ] **Step 1: Write the failing test** in `tests/integration/journey_test.go` (new, tagged):

```go
func TestJourneyStackKnobs(t *testing.T) {
	s := startStackWith(t, stackConfig{RateLimitPerMinute: 3, Accounts: []accountstest.Option{accountstest.WithVelocity(2, time.Minute)}})
	card, tok := s.activeCard(t, 1_000) // uses 3 authenticated calls? no: open + deposit = 2 calls on this subject
	// rate limit: the 4th authenticated call by this subject is 429 with Retry-After
	var last resp
	for i := 0; i < 2; i++ { last = s.call(t, "GET", "/v1/accounts/"+card, tok, "", nil) }
	if last.status != 429 || last.body["error"].(map[string]any)["code"] != "velocity_limit_exceeded" { t.Fatalf("rate limit: %d %s", last.status, last.raw) }
	// velocity: 3rd authorization attempt on the card is declined velocity_limit_exceeded
	var a resp
	for i := 0; i < 3; i++ { a = s.network(t, "/v1/network/authorizations", authBody(card, "n-"+uuid.NewString(), 1), networkSecret) }
	if a.body["decline_reason"] != "velocity_limit_exceeded" { t.Fatalf("velocity: %s", a.raw) }
}
```
(The open + deposit in `activeCard` are the subject's first two calls; call 3 is allowed, call 4 is 429. Adjust the loop count to match once the test runs: the assertion is "the first call past the limit is 429".)

- [ ] **Step 2: Run to verify it fails**: `go test -tags=integration -run TestJourneyStackKnobs ./tests/integration/` → compile error `undefined: startStackWith`.

- [ ] **Step 3: Implement** `Option`, `WithVelocity`, variadic `NewServer` in `accountstest.go`; extract `startStackWith` from `startStack` (only the `NewLimiter(s.gateway, cfg.RateLimitPerMinute)` and `accountstest.NewServer(..., cfg.Accounts...)` lines change).

- [ ] **Step 4: Run to verify it passes**, then the whole suite still passes: `make test-integration`.

- [ ] **Step 5: Commit** `test(integration): stack knobs for velocity and rate limit`.

---

### Task 2: Journal — timed steps, async waits, report

**Files:**
- Create: `tests/integration/journey_report_test.go`

**Interfaces:**
- Produces:
  - `type stepRecord struct { Phase, Name string; Status int; Dur time.Duration; Kind string }` (`Kind` is `"http"` or `"async"`).
  - `type journal struct { s *stack; phase string; steps []stepRecord }`
  - `func newJournal(s *stack) *journal`
  - `func (j *journal) begin(phase string)`
  - `func (j *journal) step(name string, f func() resp) resp` — records wall time of `f` and the status.
  - `func (j *journal) until(name string, deadline time.Duration, ready func() bool) time.Duration` — polls every 20 ms, records elapsed; if the deadline passes it records `-1` and returns `-1` (caller decides whether to fail).
  - `func (j *journal) audit(t *testing.T, name, eventType, aggregateID string) time.Duration` — `until` on `SELECT count(*) FROM audit_events WHERE event_type=$1 AND aggregate_id=$2 > 0`, 30 s deadline, `t.Fatalf` on timeout.
  - `func (j *journal) markdown() string` — table `| Phase | Step | Kind | Status | ms |` plus a footer with p50/p95/max of http steps.
  - `func (j *journal) report(t *testing.T)` — `t.Log(j.markdown())`; if `os.Getenv("JOURNEY_REPORT")` is set, writes the markdown there (0644).

- [ ] **Step 1: Write the failing test** `TestJournalMarkdown` (no containers; `journal{}` with two hand-built records, one http 201 at 12 ms, one async at 340 ms): assert the output contains `| onboarding | open account | http | 201 | 12 |`, `| onboarding | card active in audit | async | - | 340 |` and a `p95` line.

- [ ] **Step 2: Run to verify it fails**: `go test -tags=integration -run TestJournalMarkdown ./tests/integration/` → `undefined: journal`.

- [ ] **Step 3: Implement** the type and methods above. `step` must call `time.Now()` immediately around `f()` only; durations rounded to whole ms in the table.

- [ ] **Step 4: Run to verify it passes.**

- [ ] **Step 5: Commit** `test(integration): journey journal (timings, async waits, markdown report)`.

---

### Task 3: Intruder helpers — tokens, tampered webhooks, leak scan

**Files:**
- Create: `tests/integration/journey_security_test.go`

**Interfaces:**
- Produces:
  - `func noneAlgToken(sub string, accounts []string) string` — header `{"alg":"none","typ":"JWT"}`, same claims shape as `auth.Claims`, empty signature segment.
  - `func expiredToken(t *testing.T, sub string, accounts ...string) string` — `auth.Sign(jwtSecret, sub, accounts, -2*time.Minute)` (leeway is 30 s).
  - `func foreignSecretToken(t *testing.T, sub string, accounts ...string) string` — signed with `[]byte("not-the-gateway-secret-not-the-gateway")`.
  - `func (s *stack) signedAt(t *testing.T, path string, body map[string]any, secret []byte, at time.Time, mutate func(*http.Request)) resp` — like `network` but with an explicit signing time and a hook to tamper headers/body after signing (`mutate` may be nil). Deposit webhook uses `webhook.Sign` (body-only); payments/network use `webhook.SignRequest`. Signature choice by path prefix `/v1/webhooks/bank/deposits`.
  - `var leakPatterns = []*regexp.Regexp{...}` for: `(?i)pgx|postgres|sqlstate|pq:`, `rpc error|grpc|codes\.`, `goroutine|panic|runtime error|\.go:\d+`, `pending_op|lease_until|owner|key_hash|request_hash|hold_id|journal_id|exposure_epoch`, `secret|password|bearer `, `/Users/|/app/|/home/`.
  - `func assertNoLeak(t *testing.T, where string, r resp)` — fails naming the pattern and `where`.
  - `func errCode(r resp) string` — returns `error.code` or `""`.

- [ ] **Step 1: Write the failing tests** `TestLeakScan` (table: a real-looking account JSON passes; bodies containing `rpc error: code = Internal`, `SQLSTATE 23505`, `"pending_op":"capture"`, `goroutine 12 [running]` each fail — use a recording `testing.T` substitute: run `assertNoLeak` inside `t.Run` with a `defer recover` is not possible for `Fatalf`; instead expose `func findLeak(raw string) string` returning the matched pattern or `""`, and have `assertNoLeak` call it. Test `findLeak`.) And `TestNoneAlgTokenShape`: the token has three segments, header decodes to `alg=none`, third segment is empty.

- [ ] **Step 2: Run to verify it fails** (undefined symbols).

- [ ] **Step 3: Implement** the helpers.

- [ ] **Step 4: Run to verify it passes.**

- [ ] **Step 5: Commit** `test(integration): intruder helpers (forged tokens, tampered webhooks, leak scan)`.

---

### Task 4: The journey

**Files:**
- Modify: `tests/integration/journey_test.go` (add `TestCustomerJourney`; keep `TestJourneyStackKnobs`)

**Interfaces:**
- Consumes: everything from Tasks 1–3 plus existing helpers `openAndDeposit`, `bank`, `network`, `bankReq`, `purchase`, `pay`, `settle`, `account`, `summary`, `advance`, `daysUntil`, `parseTime`, `statementList`, `chargeList`, `authBody`, `token`, `count`.
- Stack: `startStackWith(t, stackConfig{RateLimitPerMinute: 500, Accounts: []accountstest.Option{accountstest.WithVelocity(30, time.Minute)}})`.
- Actors: customer `cust-J-<rand>` (token `tok` after open), intruder `cust-X-<rand>` with its own active card `xcard` (so it has a valid token and an account to compare), support `agent-J`.
- Every 2xx body in every phase goes through `assertNoLeak`. Every HTTP call is made inside `j.step`.

Each phase is a `t.Run`; the expected outcomes below are the assertions (status, `errCode`, fields). Write them as code in the test; the plan only fixes the values.

- [ ] **Step 1: Phase `onboarding`** (write the whole phase, run, then move on; the test fails until the phase is complete, which is the RED→GREEN of this task):

| Step | Call | Expect |
|---|---|---|
| open account | `POST /v1/accounts {currency:USD}` key k1 | 201, `state=pending_collateral`, `credit_limit_minor=0` |
| open again same key | same body, same k1 | 200 or 201 with `Idempotency-Replayed: true`, same `id` |
| open same key other body | `{currency:"EUR"}` k1 | 422 `idempotency_key_reuse` |
| audit: account.opened | `j.audit("account.opened", accID)` | arrives |
| token: none alg | `GET /v1/accounts/{acc}` with `noneAlgToken` | 401 `unauthorized` |
| token: expired | `expiredToken` | 401 `unauthorized` |
| token: foreign secret | `foreignSecretToken` | 401 `unauthorized` |
| token: valid, no accounts | `token(t, sub)` (no account list) | 403 `account_not_authorized` |
| tenancy oracle | intruder token on `/v1/accounts/{acc}` vs `/v1/accounts/{uuid.New()}` | both 403; `raw` bodies identical |
| deposit intent | `POST .../collateral/deposits {amount_minor:100000}` | 201, `status=pending` |
| deposit unknown field | `{amount_minor:1,"note":"x"}` | 400 `invalid_body` |
| deposit non-positive | `{amount_minor:0}` | 400 (`invalid_amount` or `invalid_body`; record the code) |
| webhook: bad signature | `signedAt(..., mutate: set X-Acme-Signature to "deadbeef")` | 401; no `collateral.confirmed` yet |
| webhook: stale timestamp | `signedAt(..., at: now-10m)` | 401 |
| webhook: oversize | body with a 2 MiB string field, validly signed | 413 `body_too_large` |
| webhook: amount mismatch | settled with `amount_minor:99999` | 400 `amount_mismatch`; account still `pending_collateral` |
| webhook: settle | correct body, `external_ref` e1 | 200 |
| account active | `until` GET account `state=active`, 10 s | records ms; `credit_limit_minor=100000`, `available_credit_minor=100000` |
| webhook: replay | identical body, fresh signature | 200; `count(deposits where status='settled')=1` for the account; `collateral.confirmed` audit count for the deposit = 1 |
| webhook: same ref, other amount | `external_ref` e1, `amount_minor:5` on a second deposit intent | 422 `external_ref_reused` |
| audit: card.activated | `j.audit("card.activated", accID)` | arrives |

- [ ] **Step 2: Phase `spending`**

| Step | Call | Expect |
|---|---|---|
| purchase 30,000 | `purchase(card, 30_000)` wrapped in two steps (authorize, capture) | approved → captured; `available=70000`, `outstanding=30000` |
| audit: authorization.captured | `j.audit(..., authID)` | arrives |
| reversal | authorize 5,000 (approved, available 65,000) then `POST .../reversal` | 201 `status=reversed`; available back to 70,000 |
| reversal twice | same reversal again | 409 `already_reversed` |
| refund 5,000 | on the 30,000 capture | 201; transaction `refunded_amount_minor=5000`; `outstanding=25000`, `available=75000` |
| refund over | refund 30,000 more | 409 `refund_exceeds_captured` |
| capture over | fresh auth 1,000, capture 1,001 | 400 `invalid_capture_amount`; then reverse it to clean up (201) |
| capture twice | capture the 30,000 auth again with new network_ref | 409 `already_captured` |
| decline: currency | `authBody` with `currency:"EUR"` | 201 `status=declined`, `decline_reason=currency_not_supported` |
| decline: credit | amount 1,000,000 | `declined` `insufficient_credit` |
| decline: purchase limit | `PUT /limits {purchase_limit_minor:1000}` 200, then authorize 2,000 | `purchase_limit_exceeded`; then `PUT /limits {purchase_limit_minor:0}` |
| decline: frozen | `POST /freeze` 200 → authorize 100 → `card_frozen`; `POST /unfreeze` 200 | as stated |
| velocity (throwaway card `vcard` of the intruder) | 31 authorizations of 1 | first 30 `approved` or `declined` for another reason, the 31st `velocity_limit_exceeded` |
| tenancy: mutations | intruder token: `POST /payments`, `PUT /limits`, `POST /freeze`, `POST /v1/dev/accounts/{acc}/clock/advance` on the customer's account | all 403; customer's `frozen=false`, `purchase_limit_minor=0`, `account_time` unchanged |
| network: unsigned | `network(..., secret=nil)` | 401 |
| network: wrong secret | signed with `bankSecret` | 401 |
| page token tampering | `GET /transactions?page_token=AAAA` | 400 `invalid_page_token` |
| transactions list | `GET /transactions` | 200; the 30,000 capture and the reversed 5,000 present; `assertNoLeak` |

- [ ] **Step 3: Phase `billing-1`** (`outstanding=25,000` going in)

| Step | Call | Expect |
|---|---|---|
| close cycle 1 | `advance` to `cycle_end` | statement 1: `new_balance_minor=25000`, `minimum_due_minor=2500`, `interest_minor=0`, `fees_minor=0`; summary `grace=true`, `stage=none` |
| audit: statement.generated | aggregate = statement id | arrives |
| pay below minimum amount | `POST /payments {amount_minor:50}` | 400 `invalid_amount` |
| pay over balance | `{amount_minor:1000000}` | 409 `payment_exceeds_balance` |
| idempotent payments | 20 goroutines, same key, `{amount_minor:2500}` | all 201, exactly one `payment.created` for the account (`count(payments where account_id)=1` in `s.accounts`) |
| other tenant, same key string | intruder on `xcard` with that same key, `{amount_minor:100}` | 201, `Idempotency-Replayed` false |
| settle wrong amount | `settle(payID, 2400, "settled")` | 400 `amount_mismatch`; payment still `pending` |
| settle | `settle(payID, 2500, "settled")` | 200; `until` account `outstanding=22500` (records ms) |
| audit: payment.settled | aggregate = payment id | arrives |
| return the payment | `bankReq("/v1/webhooks/bank/payments/{payID}/return", {return_ref, amount_minor:2500})` | 200; account `restricted_reason=payment_returned`, `outstanding=25000` |
| decline: payment_returned | authorize 100 | `declined` `payment_returned` |
| return twice, other ref | same path, new `return_ref` | 409 `payment_already_returned` |
| regularize | pay 2,500 + settle | 200; `restricted_reason` empty; `outstanding=22500` |
| audit: card.restricted / card.unrestricted | aggregate = account id | both arrive |

- [ ] **Step 4: Phase `billing-2`**

| Step | Call | Expect |
|---|---|---|
| close cycle 2 | `advance` to `cycle_end` | statement 2: `interest_minor>0`, `fees_minor=0`; `len(chargeList("fee"))=0`; `len(chargeList("interest"))=1` |
| audit: interest.charged | aggregate = charge id | arrives |
| pay in full | pay `new_balance_minor` + settle | `outstanding=0`, `available=100000` |
| close cycle 3 | advance to `cycle_end` | statement 3: `new_balance_minor=0`, `interest_minor=0` (paid in full → grace) |
| purchase 10,000 | `purchase` | `outstanding=10000` |
| close cycle 4 | advance | statement 4: `new_balance_minor=10000`, `minimum_due_minor=2500` |
| miss the minimum | advance to `due_date+15d` | `stage=past_due`, `dpd>=15`, one fee charge of `2500` |
| decline: past_due | authorize 100 | `declined` `past_due` |
| support boundary | customer token on waiver path → 403; support token (no accounts) on `GET /v1/accounts/{acc}` → 403 | as stated |
| waiver | support `POST .../waivers {amount_minor:2500, reason}` | 201; `stage=none` |
| waiver replay | same key | replayed; `len(chargeList("fee"))` unchanged, `waived_minor=2500` |
| audit: fee.charged, charge.waived | aggregates = fee id | arrive |

- [ ] **Step 5: Phase `charge-off`**

| Step | Call | Expect |
|---|---|---|
| stop paying | `advance(200)` | `delinquency=charged_off`; `credit_limit_minor` < before; `collateral_applied_minor>0` on the last statement |
| decline: charged_off | authorize 100 | `declined` `charged_off` |
| payment after charge-off | `POST /payments {amount_minor:100}` | record status and code (expected 409 or 201 — this is a finding either way, not an assertion) |
| audit: account.charged_off | aggregate = account id | arrives |

- [ ] **Step 6: Phase `closing-checks`**

| Step | Call | Expect |
|---|---|---|
| statements integrity | for each statement: `GET /statements/{id}` 200, `document` present, S3 object sha256 == `sha256` field; `assertNoLeak` on each | as stated |
| IDOR: statement via other account | intruder `GET /v1/accounts/{xcard}/statements/{customer statement id}` | 404 |
| IDOR: charge waiver mismatch | support `POST /v1/support/accounts/{xcard}/charges/{customer fee id}/waivers` | 404 or 409, never 201; customer's `waived_minor` unchanged |
| audit completeness | `http.request` events for this customer's subject ≥ number of authenticated `http` steps made with `tok` | holds |
| rate limit burst | fresh subject, `GET /v1/accounts/{uuid}` ×501 sequentially inside one `j.step("rate limit burst (501 calls)")` | the 501st is 429 `velocity_limit_exceeded` with a numeric `Retry-After` |
| report | `j.report(t)` | table logged |

- [ ] **Step 7: Run the journey**: `JOURNEY_REPORT=/tmp/journey.md go test -tags=integration -race -count=1 -run 'TestCustomerJourney' -v ./tests/integration/`. Expected: PASS, table in the log. Any failing assertion that reflects product behavior (not a test mistake) is a finding: leave the assertion, mark the step name with `FINDING:` in the table via `t.Logf`, and skip only that step with `t.Skip`-free logic (record and continue) **only if** later phases do not depend on it; otherwise stop and report.

- [ ] **Step 8: Run the whole suite**: `make test-integration` → all packages pass (`-race`).

- [ ] **Step 9: Commit** `test(integration): customer journey end to end with timings and intruder probes`.

---

### Task 5: Results document

**Files:**
- Create: `docs/testing/journey.md`
- Modify: `docs/TESTING.md` (one line pointing at the new doc under the suites section)

- [ ] **Step 1: Write `docs/testing/journey.md`** with: how to run (`make test-integration`, `JOURNEY_REPORT`), the phase list, the timing table from the first green run (copied from the report), a "Security probes" table (scenario → observed response), and a "Findings" section listing anything observed that differs from the expectation above (or "none").

- [ ] **Step 2: Commit** `docs(testing): customer journey results and security probe matrix`.

---

## Self-review notes

- Coverage: lifecycle (T4 phases 1–5), timings (T2 + T4), each intruder family from the chat agreement (tenancy: P1/P2/P6; tokens: P1; webhooks: P1/P2/P3; idempotency: P1/P3/P4; exposure: `assertNoLeak` everywhere + P6 statements; business limits: P2/P3; rate limit: P6), report (T2, T5).
- Not covered on purpose: forced 5xx leak check (needs killing an upstream; the in-process harness cannot; ADR-017 tests cover the 5xx-not-stored path at the gateway unit level). Stated in `journey.md`.
- Names are consistent: `startStackWith`, `stackConfig`, `journal.step/until/audit/report`, `assertNoLeak`, `findLeak`, `errCode`, `signedAt`, `noneAlgToken`, `expiredToken`, `foreignSecretToken`.
