# billing

Monthly statements, interest, late fees, delinquency and charge-off (Sprint 4, ADR-019).

- `internal/engine` — pure rules: cycle dates, journal replay into principal / interest / fee buckets, daily interest, minimum payment, derived delinquency, deterministic statement JSON.
- `internal/billing` — gRPC service: `CatchUp` runs every close, fee, block and charge-off due up to the account's (simulated) time; statements go to S3 (`STATEMENTS_BUCKET`) and Postgres; interest/fee/waiver postings are reserve → ledger → finalize sagas idempotent on their reference. A sweep catches up every active account each minute.
- Config: `BILLING_APR_BPS` (2280 = 1.9% monthly), `BILLING_FIXED_AMOUNTS` (per-currency floor and late fee), `STATEMENTS_BUCKET`, `ACCOUNTS_GRPC_ADDR`, `LEDGER_GRPC_ADDR`.
