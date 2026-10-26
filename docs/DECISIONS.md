# Architecture Decision Records

Append-only. One entry per decision. Format: context → decision → consequences.

## ADR-001 — Go monorepo, six services (2026-10-25)
**Context.** Fintech flow with strict concurrency guarantees; team wants microservices from day one.
**Decision.** Single Go module (`github.com/acme/acme-card`) holding `services/{gateway,accounts,ledger,billing,audit,notifications}` plus shared `pkg/`. Each service has its own `cmd/` and Dockerfile target. One go.mod (not go.work) so `go test ./...` and Docker builds stay trivial.
**Consequences.** Shared code changes ripple instantly; service boundaries enforced by package layout and gRPC contracts, not by repo boundaries.

## ADR-002 — gRPC internally, REST at the gateway (2026-10-25)
**Decision.** Public API is REST/JSON on `gateway`. Service-to-service is gRPC + protobuf (`proto/acme/*/v1`, generated with `buf`). Async events go to SNS → SQS.

## ADR-003 — Double-entry, append-only ledger in integer minor units (2026-10-25)
**Decision.** `ledger` owns money. `journal_entries` + `postings` are immutable; every entry must balance (Σ debit = Σ credit, enforced in a single transaction). Amounts are `int64` minor units + ISO-4217 currency. No floats anywhere. Balances are derived; a `balance_snapshots` table caches them.
**Consequences.** Audit trail is intrinsic; corrections are new reversing entries, never updates.

## ADR-004 — Idempotency via insert-first in Postgres (2026-10-25)
**Decision.** Mutating REST endpoints require `Idempotency-Key`. Gateway stores `sha256(subject || key)` as PK with `sha256(method|path|body)`. `INSERT … ON CONFLICT DO NOTHING` first; on conflict `SELECT … FOR UPDATE` waits for the in-flight request, then replays the stored status + body. Mismatched body with same key → 422. Outcome (`success|client_error|server_error|timeout|rolled_back`) and HTTP status are always stored. TTL 24h, swept hourly.

## ADR-005 — Append-only audit via outbox → SNS → SQS (2026-10-25)
**Decision.** Services write domain events to an `outbox` table in the same transaction as the state change. A publisher worker ships them to SNS. `audit` service consumes SQS into `audit.audit_events`, whose role lacks UPDATE/DELETE and whose triggers reject both. Audit middleware exists from Sprint 1.

## ADR-006 — Security baseline (2026-10-25)
**Decision.** JWT HS256 with `authorized_accounts` claim; gateway denies any `account_id` not in the claim. Strict JSON (`DisallowUnknownFields`). Webhooks signed with HMAC-SHA256 over `timestamp.body`, ±5m tolerance; rejections deduplicated to one row per (source, 5-minute window). Rate/velocity limiting via Postgres sliding window.

## ADR-007 — No overdraft in MVP (2026-10-25)
**Decision.** Credit limit = total confirmed collateral. Purchases never exceed it. The 10% overdraft is a future, opt-in feature.

## ADR-008 — LocalStack for AWS locally; Terraform never applied (2026-10-25)
**Decision.** docker-compose runs Postgres + LocalStack (SNS, SQS, S3, KMS). Terraform under `infra/terraform` describes prod-grade AWS (TLS 1.3, KMS, RDS Multi-AZ, S3) with `envs/dev` and `envs/prod`; only `terraform validate` runs.

## ADR-009 — Collateral confirmed by simulated bank webhook (2026-10-25)
**Decision.** `POST /v1/webhooks/bank/deposits` (HMAC-signed) settles a deposit intent. `cmd/banksim` signs and sends one. Dedup by `external_ref`.

## ADR-010 — Multi-currency = one account per (customer, currency) (2026-10-25)
**Decision.** A customer may hold several accounts, one per currency. All postings inside an account share its currency. Top-ups after activation raise the limit.

## ADR-011 — Next.js flow simulator (2026-10-25)
**Context.** Manual and exploratory testing of the flows needs a UI; curl alone is slow for demos.
**Decision.** `web/` is a single-page Next.js (App Router, TypeScript) app. Browser talks only to its own route handlers (`/api/*`), which hold `JWT_HS256_SECRET` and `BANK_WEBHOOK_SECRET`, mint tokens, sign webhooks and forward to the gateway. It ships in docker-compose as `web` on port 3000. It is a dev tool, not a customer product.

## ADR-012 — Sprint 1 implementation rulings (2026-10-25)
- `POST /v1/accounts` takes `customer_id` from the JWT `sub`; the body only carries `currency`.
- Bank webhook duplicates are deduplicated by `external_ref`; the route does not require `Idempotency-Key`.
- Webhook amount or currency mismatch → 400, deposit stays `pending` so the bank can resend.
- `credit_available` is never posted; available credit = `collateral_held − receivable`, derived from the ledger.
- Settlement saga: ledger `PostJournal` (idempotent on `deposit:<id>`) first, then the accounts transaction. A crash in between heals on the next webhook retry.
- The gateway has its own outbox. Every mutating request emits `http.request` with status and outcome (`success`, `client_error`, `server_error`, `timeout`, `rolled_back`).
- Rate limit is a Postgres sliding-window counter per JWT subject.
- Each `migrations/` directory is a Go package exporting `embed.FS`. Cross-service tests use exported `*test` packages, so `internal/` stays private.
- Go 1.26 toolchain, required by transitive dependencies.
- Local host ports: lb 8088, web 3010, because other local projects use 8080 and 3000.
- Load gate: 1000 VUs with think time and p95 < 1.5 s. `STRESS=1` measures saturation without a latency gate.
- In AWS, services build the DSN from `DB_HOST` + the RDS-managed secret, with `sslmode=verify-full`.

## ADR-013 — Final review fixes and deferred items (2026-10-25)
**Fixed:**
- Idempotency claims finish detached from request cancellation, so a client disconnect can no longer cause a double effect. Deposits also dedupe on the key hash.
- Settlement runs reserve → ledger → finalize, so the ledger and accounts cannot diverge.
- `X-Forwarded-For` is trusted only from configured proxies.
- Rejected webhooks no longer write per-request audit rows.
- Duplicate waiters are bounded: 409 `request_in_progress`. The readiness probe has its own pool.
- The simulator requires `SIMULATOR_ENABLED=1` and is bound to loopback.
- KMS key policies cover Logs and SNS. Schemas are created on startup. The RDS CA bundle ships in the image.

**Deferred (pre-deploy hardening, required before any real AWS use):**
- Per-service DB roles with least privilege. Today every service gets the RDS master secret, so the append-only triggers could be disabled by that role. Audit should use a role without UPDATE/DELETE/ALTER.
- One IAM task role per service instead of one shared role.

## ADR-014 — Sprint 2 scope decisions (2026-10-25)
- **Overdraft is removed from the product**, not deferred. ADR-007's "future opt-in feature" is withdrawn: a purchase can never exceed the collateral-backed limit.
- Outbound notifications move out of Sprint 2 into a sprint of their own.
- Freeze/unfreeze moves from Sprint 5 into Sprint 2, because `card_frozen` is a Sprint 2 decline reason.
- Card network simulated through HMAC-signed endpoints. Holds live 7 days. No partial approvals, incremental authorizations, partial captures, captures without an authorization, standalone refunds or currency conversion. Captures are for at most the authorized amount. Reversals are allowed within 3 days. Refunds after payment leave a credit balance. Velocity is 10 attempts per minute. The purchase limit applies per transaction. Handled declines return 201 with a reason.
- Full design: `docs/superpowers/specs/2026-10-25-sprint2-card-transactions-design.md`.

## ADR-015 — Sprint 2 implementation rulings (2026-10-25)
- Every ledger money operation (journal, hold, capture, release, refund) first locks the account's `collateral_held` balance row as a per-account mutex. This prevents deadlocks between hold paths and journal paths.
- Velocity runs inside the authorization transaction (`ratelimit.AllowTx`). A separate transaction needed a second pooled connection per in-flight authorization and deadlocked the pool under concurrency.
- Authorization holds use the deterministic reference `auth:<network_ref>`; capture journals use `capture:<authorization_id>` and refunds `refund:<network_ref>`. Retries after a crash therefore re-use the same ledger records.
- A declined hold stores nothing in the ledger; the decision lives on the authorization row.
- The account id doubles as the card token until card numbers exist.
- `buf lint` allows shared response envelopes (`RPC_REQUEST_RESPONSE_UNIQUE`, `RPC_RESPONSE_STANDARD_NAME`).
- The k6 script is now `tests/load/flows.js` and covers both sprints.

## ADR-016 — Sprint 2 final review fixes (2026-10-25)
**Context.** The final review found that accounts called the ledger while holding its own transaction and row locks. A lost response could then leave the two services divergent:
- orphan holds;
- a refused capture already charged;
- refunds credited twice;
- one bad row blocking hold expiry for everyone.

**Decision.** Every card network operation uses the Sprint 1 settlement pattern: **reserve (short committed tx) → ledger (idempotent, outside any accounts lock) → finalize (short tx)**.
- Authorizations are reserved as `pending`.
- Captures and reversals claim their `network_ref` and a `pending_op` before the ledger is touched, so a reused ref fails first.
- Refunds are reserved as `pending` rows that count toward the cap.
- After a reservation commits, the saga runs on a context detached from the caller's cancellation, bounded by 15 s.
- `ResolvePending`, run every minute, handles anything left after 2 minutes:
  - it fails unanswered authorizations and releases their holds, because the network treats them as declined;
  - it resumes captures, reversals, refunds and settling deposits, because they were explicitly requested.
- Expiry runs one row per transaction, and failures are marked and retried hourly.
- The ledger expires active holds older than 8 days as a final safety net.

**Other fixes.**
- Network HMAC signatures now cover the method and path.
- `network_ref_reuse` returns 422.
- A capture replay must carry the same amount.
- Account policy is read `FOR SHARE` inside the reservation.
- Velocity windows are swept.
- The request that performs a transition answers fresh; every other copy is a replay.

**Deferred.**
- `GetBalances` reads balances and holds in two statements, so available credit can be briefly overstated for display.
- Replays return the current state rather than a stored original response.

## ADR-017 — Security/optimization audit fixes (2026-10-25)
**Context.** An audit of the running stack (68 black-box probes, fault injection, SQL invariants) confirmed four defects:
- H1: the gateway stored and replayed 5xx/504 responses for the key TTL, so a retry after a transient outage could never succeed with the same key.
- H2: a reserved capture/reversal/refund the ledger refused permanently (e.g. hold expired by the ledger safety net) stayed `pending_op` forever: follow-ups answered `operation_in_progress` and the reconciler retried every minute.
- H3: `ResolvePending` released the hold before failing the authorization; a late duplicate finalizing in between could leave an approved authorization with no hold.
- H4: an idempotency claim kept its transaction (and a pool connection) open for the whole request; 60 slow requests starved the gateway pool, including routes that only needed the rate limiter.

**Decision.**
- Idempotency claims are committed `in_progress` rows with a `lease_until` and an `owner` (migration `gateway/000003`). No transaction stays open while the handler runs. Duplicates poll the row (holding a goroutine, not a connection) up to the wait timeout, then answer 409 `request_in_progress`. A row whose lease expired is taken over by the next identical request; `Complete`/`Abort` are guarded by `owner`, so a stale owner cannot overwrite a takeover. This supersedes the row-lock design of ADR-004.
- 5xx responses are not stored: the claim is aborted and the retry runs the handler again. Downstream operations are idempotent on their own anchors (deposit key hash, `external_ref`, `network_ref`, journal references), so a retry after a timeout whose effect did land resolves to that effect. 2xx and 4xx are stored and replayed as before. ADR-004's "always stored" is withdrawn for 5xx.
- A ledger error with code `FailedPrecondition`, `InvalidArgument`, `OutOfRange`, `NotFound` or `AlreadyExists` is terminal for a reserved operation: the authorization becomes `failed` with its pending op cleared (event `authorization.failed`); a refund becomes `failed` (migration `accounts/000008`, event `refund.failed`) and stops counting toward the cap. The caller still receives the ledger's reason.
- `ResolvePending` fails stale pending authorizations first and releases holds second. `hold_released_at` records the release so a crash between the two steps is retried on the next pass.
- Query optimizations: the rate limiter is one statement (`INSERT … ON CONFLICT DO UPDATE … WHERE`, exact under concurrency); `GetBalances` reads balances and holds in one statement (closes the ADR-016 deferral); settlement never calls the ledger while holding deposit/account locks; `Authorize` no longer pre-loads the account nor re-reads the row it just inserted (an unknown card surfaces as the foreign-key violation, mapped to `card_not_found`).

**Consequences.** A request whose handler outlives the lease (30 s, three times the gateway timeout) loses its claim and its result is discarded (`ErrClaimLost` logged). Concurrent duplicates cost one cheap primary-key read every 25 ms while they wait.

## ADR-018 — Repayments, spendable credit balance, accepted return risk (2026-10-25)
**Context.** Sprint 3 adds customer repayments. In the Sprint 3 discovery the user redefined the credit balance.

**Decision.**
- **Spendable credit balance (supersedes the ADR-014 strict limit).** A credit balance (negative receivable) is spent before the credit line: 700 in favor plus a 500 purchase leaves 200 in favor and nothing owed. The ledger rule is signed: `Σ active holds + receivable + amount ≤ limit`; `available = max(0, limit − receivable − holds)` may exceed the credit limit. A credit balance is never paid out in cash.
- **Payments copy the collateral-deposit flow.**
  - The customer creates an intent: minimum 100 minor units, account currency, capped at `max(receivable,0) − Σ open payments`.
  - The bank settles or fails it through a webhook bound to method and path, and settlement runs as reserve → ledger → finalize.
  - Credit is restored only on settlement, and intents with no answer expire after 7 days.
- **Returns: accepted risk.**
  - Within 5 days of settlement a return re-raises the debt, which may exceed the collateral; the bank accepts that exposure.
  - The card is then restricted (`payment_returned`, a system restriction the customer cannot lift, evaluated after `currency_not_supported`). The restriction lifts right after a payment settles, or within a minute via the sweep, once exposure is back within the limit.
  - Purchases can never push exposure over the limit.

**Implementation rulings.**
- The payments migration is `000009`, because `000008` belongs to the audit session.
- The restriction is re-evaluated after the finalize commit, never with a ledger call inside accounts locks.
- The idempotency invariant follows the audit session's lease design.
- The k6 flow retries lease-based `409 request_in_progress`.

**Sprint 3 final review fixes (2026-10-25).**
- **Fail-safe restriction.** A payment return restricts the card inside its own finalize transaction and bumps `accounts.exposure_epoch`. A lift happens after commit, only if exposure is within the limit and the epoch is unchanged.
- **Late settlements are applied.** A bank settlement arriving after the intent expired is still applied, because the money already moved.
- **Small debts can be paid off.** The payment minimum is `min(100, outstanding)`.
- **Payment reconciliation never aborts the pass.**
- **422 on reference reuse.** `external_ref_reused` and `return_ref_reused` answer 422.

**Deferred minors.**
- The payment cap can race a settlement in a millisecond window; an overpayment becomes a spendable credit balance.
- The simulator's bank-payments proxy returns 500 on malformed input.

## ADR-019 — Billing: statements, interest, late fees, delinquency, simulated clock (2026-10-25)
**Context.** Sprint 4 adds monthly statements and the cost of carrying a balance. The user's decisions are recorded in `docs/superpowers/specs/2026-10-25-sprint4-statements-design.md` (§3, interpretations §4, builder calls §5/§5b).

**Decision.**
- **New `billing` service** owns cycles, statements, interest, late fees and delinquency. Statements are deterministic JSON in S3 (`statements/<account>/<cycle>.json`) plus a Postgres row with totals, buckets and the SHA-256 of the object. One statement per cycle, also with no activity.
- **Cycle**: monthly on the activation day (29–31 → 28), due date close + 21 days, UTC days. A payment received (created) any time on the due date is on time; a payment still pending holds off delinquency until the bank answers.
- **Interest**: 1.9% monthly = 22.80% APR (`BILLING_APR_BPS=2280`), daily balance × APR / 365 in integer micro-units, floored at close. Standard grace period; interest on interest and fees only while past due; posted at `close − 1s`.
- **Minimum**: `min(new balance, max(floor, 1% principal + cycle interest + cycle fees + past-due minimum))`; floor and late fee per currency (`BILLING_FIXED_AMOUNTS`, $25 for USD).
- **Delinquency is derived, never counted**: past-due amount and DPD come from the statements and the payments/waivers credited since; a late payment cures it. DPD 1 → late fee (once per statement, capped at its minimum); DPD 15 → `past_due` decline; DPD 60 → charge-off: collateral applied to the debt (`collateral_applied`, fees → interest → principal), limit lowered, `charged_off` permanently. Later statements are balance-only.
- **Buckets come from the ledger**: `receivable` stays the single debt; billing replays the journal (by `effective_at`) to split principal / interest / fees. Payments allocate fees → interest → principal; a return restores exactly what its payment reduced.
- **Ledger**: `journal_entries.effective_at` (business date) next to `posted_at`; per-account `interest_income` / `fee_income`; `PostCharge`, `PostCollateralApplication`, `ListEntries`, and **sealed periods** (`SealPeriod`): a posting never gets an `effective_at` before the last closed statement, so a saga that reserved before a close lands in the next cycle and closed statements always reconcile.
- **Simulated clock**: a per-account offset in `accounts` (only grows, 1–400 days per call). It drives billing and every posting's business date (persisted at reserve so reconciler retries post the same date). Infrastructure timers (idempotency, velocity, webhook tolerance, hold/intent expiry, reversal/return windows) stay on real time. The gateway route exists only with `DEV_CLOCK_ENABLED=1`, and the gateway refuses to start with it unless `APP_ENV=dev`.
- **Over the collateral**: interest and fees may exceed it (accepted); the card is then restricted `over_limit` until exposure is back within the limit.
- **Waivers**: a `role: support` JWT reverses all or part of an interest or fee charge (reason required, Σ waived ≤ charged); a waiver counts toward the past-due minimum.
- **Decline order**: account_closed, charged_off, collateral_not_confirmed, currency_not_supported, payment_returned | over_limit, past_due, card_frozen, velocity_limit_exceeded, purchase_limit_exceeded, insufficient_credit.

**Implementation rulings.**
- Catch-ups of one account serialize on a **lease** in its `billing_accounts` row (autocommit UPDATE, 2-minute expiry). Two earlier designs, a row lock held in a transaction, waiting or `NOWAIT`, pinned a pool connection per catch-up and deadlocked the pool under concurrency (found by tests and by the 1000-VU load).
- Charge-off waits (`operation_in_flight`, retried) while any authorization is pending or has an operation in flight, so no capture lands after the collateral is applied; its amount is fixed once so retries post the same journal.
- Migrations: accounts `000012`–`000013`, ledger `000004`, billing `000001`–`000003`.

**Sprint 4 final review fixes (2026-10-25).**
- **On-time payments that settle after a close count.** A payment received before a close but posted after it (or still pending) is not in that statement's balance, so it now pays that statement's minimum and grace, unless it already sits in the previous statement's on-time window (`engine.anchor`). Before, it fell outside every window: late fee, lost grace, possible block.
- **Late fee capped at the unpaid minimum** (spec I-2), e.g. 1.00 when 24.00 of 25.00 was paid.
- **Due date is not a past-due day** for interest (spec B-6).
- **Clock advance idempotent inside accounts** (`clock_advances`, migration `000014`): the gateway releases an idempotency key on a 5xx, so a retry no longer advances twice.


## ADR-020 — Close, cancel and payouts (2026-10-25)
**Context.** Sprint 5 ends the customer relationship. The user's decisions are in `docs/superpowers/specs/2026-10-25-sprint5-close-design.md` (§3, §4, §6c).

**Decision.**
- **States.** `pending_collateral → cancelled` (instant; pending deposits voided, a late bank settlement gets 409 `deposit_voided` and posts nothing). `active → closing → closed`. From the close request every new authorization declines `account_closing`; captures, reversals and refunds of earlier approvals still work; payments are accepted while debt is owed; deposits are refused.
- **Close saga** (`accounts.close_stage`, driven by the request, dev clock advances and a per-minute sweep):
  1. `in_flight` until no authorization, payment, settling deposit, refund or charge-off is in flight;
  2. `final_statement`: billing cuts an off-cycle final statement (interest up to then, due + 21 days), after which billing charges nothing;
  3. `awaiting_due`: debt paid → skip; otherwise on the final due cutoff (or at once if charged off), unless a payment received before it is still pending;
  4. `applying`: `min(debt, collateral)` applied (`closeout:<id>:<n>`, dated at the cutoff);
  5. `awaiting_returns`: until no settled payment is inside its 5-day return window (user decision; money never leaves twice);
  6. `payout`: remaining collateral + credit balance, released only when the bank confirms;
  7. `closed`.
  A return during the saga re-raises the debt and is re-applied from the collateral.
- **Payouts.** One open payout per account; attempts are 1..n and each attempt id is the external reference. `failed` is final for its attempt (a late `settled` gets 409 `attempt_failed`); support retries with a new attempt. The ledger posts one journal `payout:<attempt>` (debit `collateral_held` + `receivable`, credit `cash_clearing`). A refund landing on a closed account creates an additional payout. The credit balance is paid out only at close (refines ADR-018).
- **Destination.** Given in the close request; stored AES-256-GCM encrypted (`pkg/secretbox`, `DESTINATION_KEY`; KMS in AWS) plus last 4 digits. The bank fetches the plain number through a bank-signed instruction route; everything else (events, audit, responses, logs) carries `****<last4>`, and the bank's webhook must echo the last 4.
- **Reopen.** One open account per customer and currency (partial unique index); a new one is refused while a closed account of that customer and currency still has debt.
- **Past due never exceeds what is owed** (`engine.CapToOwed`): debt cleared by collateral or refunds leaves nothing past due.

**Implementation rulings.**
- The final statement is cut once in-flight work has resolved (captures of earlier approvals land on it), not at the request.
- `RETURN_WINDOW` is configurable for the dev load test only; accounts refuses another value unless `APP_ENV=dev`.
- Migrations: accounts `000015`, billing `000004`.

## ADR-021 — Audit hardening: chain, numbering, checkpoints, replay, reconciliation, retention, auditor (2026-10-25)
**Context.** Sprint 6 makes the trail verifiable, complete, reconcilable and usable by an auditor. Decisions: `docs/superpowers/specs/2026-10-25-sprint6-audit-design.md`.

**Decision.**
- **Gap-free numbering.** Each producer's relay stamps `source` + contiguous `source_seq` from `outbox_sequence` inside its publish transaction. Verify reports a missing range per source once the event after it is older than 2 minutes (SQS may reorder).
- **Hash chain.** `hash = sha256(prev_hash ‖ "\n" ‖ canonical)`; canonical = the redacted event as normalized JSON (sorted keys, exact numbers, `occurred_at` truncated to µs). Genesis = 64 zeros. Inserts lock `chain_head`, so `chain_seq` is contiguous; redeliveries (same id) add nothing. Rows written before Sprint 6 are chained once at startup (the trigger allows only that update).
- **Redaction** before hashing, case-insensitive at any depth: `account_number`, `holder_name`, `destination_ciphertext`, `token`, `access_token`, `secret`, `password` → `"[redacted]"`. Exports re-redact and mask JWT-like strings (pre-Sprint-6 rows were never redacted and stay immutable).
- **Checkpoints.** Daily (and on demand) the head is signed with Ed25519 (`AUDIT_SIGNING_KEY`), linked to the previous checkpoint's sha256, and written to an S3 Object Lock COMPLIANCE bucket (7 years). A full re-hash of the database is caught at the next checkpoint.
- **Verification.** The daily job and `auditctl verify-chain` walk the whole chain and record it (`audit_verifications`). The API verifies from the last recorded walk after re-checking its anchor hash: fast at any size; tampering before the anchor that does not re-hash is caught by the next full walk.
- **Snapshots and replay.** `account.changed` is emitted by a trigger on every account change; `snapshot.account` (account + ledger balances + hash) daily and on demand (in the background, 8 workers, one run at a time). Replay compares the latest trail state of an account with live state read through the reporting role.
- **Reconciliation.** The 36 invariants of `tests/load/verify.sql` (embedded copy kept identical by a test) run daily and on demand; the report goes to the locked bucket and `audit_reconciliations`; `reconciliation.completed` / `reconciliation.violation` events go through the audit service's own outbox.
- **Retention.** Monthly (the 1st, UTC): the chain prefix recorded more than 13 months ago is archived as NDJSON + signed manifest to the locked bucket, read back and re-hashed (`verified_at`), then purged only through `audit_purge_archived(through)`. The trigger lets a DELETE through only with `audit.purge` set *and* a verified archive covering the row. The chain verifies across a purge from the manifest's last hash. `auditctl archive|verify|purge` for operators.
- **Auditor.** JWT `role: auditor`, read-only `/v1/audit/*`: search (account, type, actor, dates; paged by `chain_seq`), verify, checkpoint, reconciliation, replay, export (+ download), snapshot. Every call emits `audit.queried` (download included) or `audit.exported`; a refused call emits `audit.access_denied`. Auditors never purge.

**Implementation rulings.**
- `account.changed` comes from a plpgsql trigger (writes to `TG_TABLE_SCHEMA.outbox`), so no code path can forget it.
- Exports are capped at 100,000 events (409 beyond; narrow the filter).
- `auditctl` lives in `services/audit/cmd/auditctl` (Go internal-package rule).
- Migrations: accounts `000017`–`000019`, ledger/billing `000005`, gateway `000004`, audit `000002`–`000005`.
