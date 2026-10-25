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
