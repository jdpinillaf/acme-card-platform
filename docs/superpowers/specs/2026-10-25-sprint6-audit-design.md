# Sprint 6 — Audit hardening

Status: builder-checked · 2026-10-25 · builds on Sprints 1–5 (ADR-001…020)

## 1. Intent

Today every state change and every HTTP request already lands in an append-only `audit_events` table (outbox → SNS → SQS → audit). Triggers block UPDATE, DELETE and TRUNCATE. What is missing is proof:
- nobody can show that the log is complete;
- nobody can show it was not rewritten by someone with database access;
- nobody can show it agrees with the live books.

This sprint makes the audit trail **verifiable**, **complete**, **reconcilable** and **usable** by an auditor, with a retention policy.

## 2. Goals, non-goals, anti-goals

**Goals**
- A read-only `auditor` role that can search, verify and export the trail. Its own queries are audited.
- **Tamper evidence**: a hash chain over every audit event, plus daily signed checkpoints stored outside the database in write-once S3 (Object Lock).
- **Completeness**: every producer numbers its events without gaps; a detector reports missing numbers.
- **Snapshots**: a daily full-state snapshot per account (state, balances, delinquency, close stage), hashed, stored in the trail.
- **Replay**: rebuild each account's state from the trail and compare it with the live state, as a command and as a daily job.
- **Reconciliation**: the `verify.sql` invariants run daily as a job. The result goes to S3 and the panel, and any violation emits an alert event.
- **Retention**: 13 months hot in Postgres, 7 years archived in S3. Nothing is purged before it is archived and verified.
- **Sensitive reads audited explicitly**: bank payout instructions and statement downloads.
- A simulator panel for everything. The load test ends with chain verification, zero gaps and a clean replay.

**Non-goals**
- Alert delivery (email, webhooks): that is Sprint 7. Here alerts are events.
- An auditor UI beyond the simulator panel.
- External timestamping authorities or a blockchain anchor (S3 Object Lock + signature is the anchor).
- Real AWS deployment (Terraform described only, as always).

**Anti-goals**
- An audit event that can be altered, removed or reordered without the verifier noticing.
- A lost event (outbox → audit) that nobody detects.
- Full payout account numbers, ciphertexts, JWTs or secrets in the trail, exports or archives.
- Audit queries that are not themselves audited.
- A purge that deletes something not yet safely archived.

## 3. Decisions made by the user (all proposals accepted)

| # | Topic | Decision |
|---|---|---|
| 1 | Who reads | New `auditor` role (JWT), read-only: search by account, event type, actor, dates; export |
| 2 | Meta-audit | Every auditor query and export is audited |
| 3 | Tamper evidence | Hash chain: each event stores the previous hash; a verifier detects any gap or change |
| 4 | Anchor | Daily checkpoint of the chain head, signed, in S3 with Object Lock (WORM) |
| 5 | Lost events | Every service numbers its events; a detector reports gaps |
| 6 | Snapshots | Daily full-state snapshot per account (balances, state, delinquency, close), with its hash |
| 7 | Replay | Command + daily job rebuilding each account from its events and reporting differences |
| 8 | Reconciliation | The integrity checks run daily; result to S3 + event + panel; violations raise an alert event |
| 9 | Retention | 13 months in Postgres, 7 years archived in S3; nothing deleted earlier |
| 10 | Personal data | Holder name also masked; legal retention wins over deletion requests |
| 11 | Sensitive reads | Bank payout instruction and statement downloads audited, beyond HTTP request logging |
| 12 | Simulator | Audit panel: search, verify chain, reconciliation, export |
| 13 | Load | Ends with chain valid, zero gaps, replay without differences |

## 4. Interpretations (builder's calls)

- **I-1 Producer sequence without gaps.** The outbox `seq` (bigserial) has gaps on rollback, so it cannot prove completeness. The relay assigns `source` (service name) and `source_seq` at publish time, under a per-source lock in the relay transaction, so numbers are contiguous per source. The audit store records both; the gap detector lists missing `source_seq` per source.
- **I-2 Chain order.** The chain follows audit insertion order (`chain_seq`, contiguous, assigned under a single chain-head lock in the audit store). `hash = SHA-256(prev_hash ‖ canonical(event))`, where canonical covers id, type, aggregate, actor, key hash, status, before, after, occurred_at, source, source_seq and chain_seq. The genesis prev_hash is 64 zeros. Existing rows are chained once by the migration, in recorded order.
- **I-3 Checkpoints.** A checkpoint is `{chain_seq, hash, created_at, prev_checkpoint_hash}`, signed with Ed25519 (`AUDIT_SIGNING_KEY`; KMS asymmetric in AWS). It is written as `checkpoints/<date>/<chain_seq>.json` to an Object-Lock bucket (COMPLIANCE, 7 years). It runs daily and on demand by an auditor. Verification checks the signatures, that the checkpoints link to each other, and that each checkpoint hash equals the recomputed chain at its `chain_seq`.
- **I-4 Snapshots are events.** accounts emits one `snapshot.account` event per non-cancelled account per day (UTC). The event carries the account row (masked), the ledger balances and the billing stage. Its hash is in `after.snapshot_hash`. Because it is an event, it is chained and gap-checked like everything else.
- **I-5 Replay.** For each account, take the latest account-aggregate event state (`after` of account events) and the ledger balances from the latest `ledger.journal_posted` balances. Compare them with the live account and ledger. Differences are reported per field; a snapshot newer than the last change is used as the starting point.
- **I-6 Reconciliation.** The invariant queries are embedded in the audit service, which in dev reads the other schemas of the single Postgres; in AWS, a read-only reporting role. The service writes `reconciliation/<date>/<run>.json` to S3, emits `reconciliation.completed` and, per violated check, `reconciliation.violation` (the alert).
- **I-7 Retention.**
  - A monthly archive job exports every audit event older than 13 months, chain order, as NDJSON plus a manifest (first and last `chain_seq`, hashes, count, signature) to the Object-Lock bucket.
  - Rows are removed only through `audit_purge_archived(through_chain_seq)`. It checks that a verified archive manifest covers the range, and it is the only path the trigger allows (`SET LOCAL audit.purge`).
  - The chain stays verifiable across a purge from the manifest's last hash.
- **I-8 PII.** Producers already mask payout destinations; holder names never leave the ciphertext. The audit store additionally rejects (and counts) any event carrying keys named `account_number`, `destination_ciphertext`, `authorization` or `token`, by redacting their value to `"[redacted]"` before hashing.
- **I-9 Auditor API** (gateway, JWT `role: auditor`, every call audited by the HTTP audit plus an explicit `audit.queried` / `audit.exported` event):
  - `GET /v1/audit/events?account_id&event_type&actor&from&to&page_token` (≤ 500 per page);
  - `GET /v1/audit/verify` (chain + checkpoints + gaps);
  - `POST /v1/audit/checkpoints` (on demand);
  - `GET /v1/audit/reconciliation` (latest) and `POST /v1/audit/reconciliation` (run now);
  - `GET /v1/audit/replay/{accountId}`;
  - `POST /v1/audit/exports {filter}` → NDJSON in S3 + `GET /v1/audit/exports/{id}` (download).
- **I-10 Sensitive reads.** accounts emits `payout.instruction_read` and billing emits `statement.read`, with actor and ids only.


## 4b. Builder-check calls (user: "continúa con lo sugerido")
- **B-1 Replay source.**
  - An AFTER UPDATE/INSERT trigger on `accounts.accounts` enqueues `account.changed` (aggregate `account`) into the accounts outbox. It carries the full row (minus `destination_ciphertext` and lease columns), so every mutation is covered without touching the ~20 call sites. Existing event names and payloads are unchanged (the customer-journey test depends on them).
  - Replay compares `state, frozen, collateral_minor, credit_limit_minor, purchase_limit_minor, restricted_reason, delinquency, close_stage, clock_offset_seconds` from the latest `account.changed`/`snapshot.account` with the live row. It also compares each ledger sub-account balance from the latest `ledger.journal_posted` balances (or the snapshot) with the live ledger.
  - Accounts with no such event yet start from their first snapshot. "Billing stage" in I-4 means `delinquency`.
- **B-2 Redaction.**
  - The redaction is an exact, case-insensitive key match at any depth in `before`/`after`, on these keys: `account_number`, `holder_name`, `destination_ciphertext`, `token`, `access_token`, `secret`, `password`.
  - `authorization` is a domain object and is **not** redacted.
  - The value becomes `"[redacted]"` before hashing, and the row records `redacted = true`. Events are never rejected, so no gap appears.
- **B-3 HTTP audit unchanged.** GETs stay unaudited by the middleware (the journey test relies on it). The audit API emits explicit events:
  - `audit.queried` for every successful read;
  - `audit.exported` for every export;
  - `audit.access_denied` for every refused call on `/v1/audit/*`, from a gateway hook on the role check.
- **B-4 Completeness scope.** The scope is published events (gaps in `source_seq`) plus stuck outboxes: unpublished rows older than 5 min, per service, reported by verify. Gateway HTTP-audit write failures stay log-only (unchanged).
- **B-5 Purge.**
  - The monthly retention job archives, verifies and then calls `audit_purge_archived` by itself, as the audit service's DB role.
  - `cmd/auditctl` offers `archive|verify|purge` for operators.
  - The auditor role never purges.
- **B-6 Snapshot day.** Snapshots run once per real UTC day (worker) and on demand (`SnapshotAccounts` RPC, used by tests and the load gate). They do not run on simulated clocks.

## 5. Test gates
- Chain: tampering with any field, deleting a row (with triggers disabled), or reordering is detected at the exact `chain_seq`. Concurrent inserts keep a contiguous chain.
- Gaps: dropping one published event before audit receives it is reported as a gap for that source; replaying the SQS message closes it.
- Checkpoints: signature and link verification; a checkpoint that does not match the recomputed chain fails verification.
- Snapshots and replay: a corrupted live balance or account field is reported by replay; a clean system reports none.
- Reconciliation: every check runs; a corrupted row produces a violation event and appears in the S3 report.
- Retention: archive → verify → purge removes only covered rows, keeps the chain verifiable; purging without an archive is refused; the trigger still refuses ordinary DELETE.
- Auditor role: customer/support tokens get 403; every auditor call produces `audit.queried`/`audit.exported`; exports contain no secrets (grep).
- Load: after 1000 VUs, verify returns chain valid, zero gaps, replay clean, reconciliation 0.

## 6. Left to the builder
- Table/column names, proto shapes, page sizes, S3 key layout details, CLI flags, panel layout.
