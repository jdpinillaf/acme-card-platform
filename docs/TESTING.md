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
| Post-load SQL invariants (`tests/load/verify.sql`) | 7/7 checks, 0 violations |
| Simulator (`make web-test`) | HMAC/JWT byte-compatible with Go; browser flow verified |
| Terraform | `validate` passes for dev and prod |

Load numbers come from Docker Desktop on a developer laptop. They show correctness under concurrency, not production capacity.
