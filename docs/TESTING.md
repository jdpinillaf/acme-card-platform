# Testing gates

| Layer | Tool | Command | Gate |
|------|------|---------|------|
| Unit | `go test` | `make test` | 100% of state machines, money math, hashing, middleware |
| Integration | testcontainers (Postgres, LocalStack) | `make test-integration` | Full flow per sprint; ledger balanced after every tx |
| Concurrency | `go test -race`, N goroutines | included in integration | Same `Idempotency-Key` ×50 → exactly one effect, identical responses |
| Security | unit + integration | included | Bad/stale HMAC → 401; JWT without account in `authorized_accounts` → 403; unknown JSON field → 400 |
| Audit | integration | included | Every mutation ≥1 `audit_events`; `UPDATE/DELETE audit_events` fails |
| Customer journey | integration (`TestCustomerJourney`) | included in `make test-integration` | Sprints 1–4 lifecycle with per-step timings and intruder probes; results in `docs/testing/journey.md` |
| Load | k6 (Docker) | `make load` | 1000 VUs mixed flow; zero duplicate journal entries per idempotency key; p95 reported |
| Infra | terraform | `make tf-validate` | `terraform validate` passes for dev and prod |

TDD is mandatory: the failing test is written and run before production code.

## Sprint 1 results (2026-10-25)

| Gate | Result |
|---|---|
| Unit (`make test`) | all packages pass with `-race` |
| Integration (`make test-integration`) | pass: idempotency store, outbox, audit (LocalStack), ledger, accounts, gateway, end-to-end |
| Same `Idempotency-Key` ×50 over HTTP | 1 deposit row, 49 replays, identical bodies; different body with same key → 422 |
| Same bank webhook ×50 | 1 journal entry, 1 activation, 49 replays |
| Load, 1000 VUs with 1–3 s think time | 69,882 requests, 0 failed, p95 1.24 s, 11,647 cards activated, 0 wrong states |
| Load, 1000 VUs no think time (`STRESS=1`) | 128,628 requests, 0 failed, p95 2.02 s at ~1,170 req/s on a laptop |
| Post-load SQL invariants (`tests/load/verify.sql`) | 8/8 checks, 0 violations (re-run after review fixes) |
| Simulator (`make web-test`) | HMAC/JWT byte-compatible with Go; browser flow verified |
| Terraform | `validate` passes for dev and prod |

Load numbers come from Docker Desktop on a developer laptop. They show correctness under concurrency, not production capacity.

### After final-review fixes (2026-10-25)

The latency gate (p95 < 1.5 s) **does not pass on the current host** and is not claimed as passing.
Correctness gates still pass: 0 failed requests, 0 wrong final states, 0 invariant violations.

An A/B run on the same host at the same time, with fresh databases and `SHORT=1`, isolates the cause:

| Build | Requests | Failed | p95 | Throughput |
|---|---|---|---|---|
| Before review fixes (`1b01973`) | 22,395 | 0 | 5.41 s | 407 req/s |
| After review fixes (HEAD) | 22,549 | 0 | 4.01 s | 412 req/s |

The fixes did not regress latency. The host was saturated (load average 28–47) by other local stacks running at the same time. The earlier 1.24 s figure was measured on a quieter host.

Under load the top Postgres wait is `LWLock:WALWrite` on commit, i.e. fsync on the Docker Desktop volume. Follow-ups that would cut commits per request:
- batch the audit consumer inserts into one transaction per poll;
- skip `OpenLedger` on settlement when the ledger already exists;
- merge the rate-limit and idempotency writes into one transaction.

Re-run `make load` on an idle machine before using these numbers to gate anything.

## Sprint 2 results (2026-10-25)

| Gate | Result |
|---|---|
| Ledger holds | Strict limit `holds + max(receivable,0) + amount ≤ limit`; 10×25% concurrent → 4 holds; capture vs release ×25 → exactly one wins; credit balance never raises available |
| Decline order | Each reason alone and full precedence chain (unit) |
| Authorize | 10 concurrent at 25% → 4 approved / 6 `insufficient_credit`; same `network_ref` ×50 → 1 row, 49 replays; velocity exact (10 of 50) under concurrency; declined attempts count |
| Capture / reversal / refund | Capture ≤ authorized, once; capture after 7 days refused; capture vs reverse ×25 → one winner; 3-day reversal window; partial refunds capped; closed account: capture refused, refund/reversal accepted; frozen card still captures |
| Expiry worker | Releases due holds; races with capture → always expired, never posted |
| Gateway | HMAC network routes (bank secret rejected), strict JSON, 201 for declines, replay header, freeze/limits/transactions with account authorization |
| End to end (`TestSprint2EndToEnd`) | 7 scenarios through HTTP + gRPC + LocalStack audit: PASS |
| Load, `SHORT=1` (1000 VUs, Sprint 1 + 2 flows) | 48,507 requests, 0 failed, 100% checks, p95 863 ms, 0 wrong balances |
| Load, full | 62,128 requests, 0 failed, 100% checks, 0 wrong balances; p95 3.85 s on a loaded host (load average ≈ 21), latency gate not met there |
| Invariants (`verify.sql`) | 13/13 checks, 0 violations |
| Simulator | 6/6 tests; browser QA: partial capture 100, refund 40, available 440, frozen → `card_frozen` |

### After Sprint 2 final-review fixes (2026-10-25)

| Gate | Result |
|---|---|
| Unit + integration | all packages pass with `-race` (accounts suite includes 10 resilience tests with an injected "lost response" ledger) |
| Load, full 1000 VUs (Sprint 1 + 2 flows) | 89,555 requests, 0 failed, 100% checks, **p95 1.18 s (gate met)**, 765 req/s, 0 wrong balances |
| Invariants after reconciler window | 16/16 checks, 0 violations; the 5 new cross-checks were verified to fire on deliberately corrupted data |

The fix pass also raised throughput: before it, no locks were released during ledger calls (435 req/s, p95 863 ms in the short run); after it, 913 req/s and p95 664 ms in the same short run.

## Audit fixes (2026-10-25, ADR-017)

| Gate | Result |
|---|---|
| Unit (`make test`) | all packages pass with `-race` |
| Integration, all tagged packages (`go test -tags=integration ./pkg/... ./services/...`) | pass; new tests: claim holds no pool connection, expired lease taken over, stale owner cannot complete, 5xx not stored / retry runs, 4xx replayed, capture on dead hold fails terminally, refused refund fails terminally, reconciler fails before releasing |
| End to end (`make test-integration`) | pass |
| Live probes after rebuild (68 black-box) | 64/68, same as before the fixes: the 4 remaining are the two out-of-scope findings (replay before `RequireAccount`, `hashParts` collision) and two probe-side arithmetic errors |
| Fault injection: accounts stopped / paused, `POST deposits` → 504, retry with the same key after recovery | 201 (was: 504 replayed); the detached saga plus the deposit key-hash dedupe leave exactly one deposit |
| Fault injection: capture on a hold the ledger already expired | 409 `hold_not_active`, authorization `failed` with no pending op, reversal answers `authorization_failed` (was: `operation_in_progress` forever, reconciler error every minute) |
| 60 concurrent mutating requests with accounts paused | 58×504 + 2×429, 0×503 `idempotency_unavailable` (was 4); an unrelated rate-limit-only route answered in ≤0.12 s throughout (was 8 s) |
| `tests/load/verify.sql` on the live database | 16/16, 0 violations |
