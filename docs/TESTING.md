# Testing gates

| Layer | Tool | Command | Gate |
|------|------|---------|------|
| Unit | `go test` | `make test` | 100% of state machines, money math, hashing, middleware |
| Integration | testcontainers (Postgres, LocalStack) | `make test-integration` | Full flow per sprint; ledger balanced after every tx |
| Concurrency | `go test -race`, N goroutines | included in integration | Same `Idempotency-Key` ×50 → exactly one effect, identical responses |
| Security | unit + integration | included | Bad/stale HMAC → 401; JWT without account in `authorized_accounts` → 403; unknown JSON field → 400 |
| Audit | integration | included | Every mutation ≥1 `audit_events`; `UPDATE/DELETE audit_events` fails |
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
