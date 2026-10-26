# Sprint 6 — Audit Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A verifiable, complete, reconcilable audit trail:
- a hash chain, plus signed checkpoints in write-once S3;
- per-source gap-free numbering;
- daily snapshots and replay;
- automated reconciliation;
- retention with archive-then-purge;
- an auditor API and panel.

**Architecture:**
- **pkg/outbox:** the relay numbers events per source at publish time.
- **audit service:** hashes and chains each event on insert (single chain-head lock) and redacts sensitive keys. It gains a gRPC API for query, verify, checkpoints, reconciliation, replay, exports and retention, plus daily jobs.
- **accounts:** emits `account.changed` via a DB trigger and `snapshot.account` daily or on demand.
- **gateway:** exposes the auditor role routes.

**Tech Stack:** Go 1.26, gRPC/buf, pgx v5, crypto/ed25519, aws-sdk-go-v2 S3 (Object Lock), testcontainers (Postgres + LocalStack), k6, Next.js.

**Spec:** `docs/superpowers/specs/2026-10-25-sprint6-audit-design.md`. Read §3, §4 and §4b (§4b overrides §4).

## Global Constraints

**Chain**
- Columns:
  - `chain_seq` is contiguous from 1;
  - `prev_hash` and `hash` are 64-hex;
  - genesis `prev_hash` is `0`×64.
- `hash = sha256(prev_hash ‖ "\n" ‖ canonical)`, where canonical is the JSON object (sorted keys) of:
  - `id, event_type, aggregate_type, aggregate_id, actor, idempotency_key_hash, http_status`;
  - `before`, `after`, as stored after redaction, normalized by jsonb;
  - `occurred_at` (RFC3339Nano UTC), `source`, `source_seq`, `chain_seq`.
- Inserts take `chain_head` FOR UPDATE.

**Sources**
- The service names `accounts`, `ledger`, `billing` and `gateway` are passed to `outbox.NewRelay`.
- Each relay assigns `source_seq` from the table `outbox_sequence(source, next_seq)` in its own schema. This happens inside the publish transaction, so it is contiguous and rolls back with the transaction.

**Checkpoints**
- Payload: `{chain_seq, hash, created_at, prev_checkpoint_sha256}`.
- Signed with Ed25519, `AUDIT_SIGNING_KEY` (base64 64-byte private key; dev key in `.env.dev`).
- Stored in S3 bucket `AUDIT_BUCKET` (default `acme-card-audit-dev`) with Object Lock COMPLIANCE, retention 7 years.
- Keys: `checkpoints/<YYYY-MM-DD>/<chain_seq>.json` plus `.sig`.

**Redaction**
- Keys (case-insensitive, any depth): `account_number`, `holder_name`, `destination_ciphertext`, `token`, `access_token`, `secret`, `password`. The value becomes `"[redacted]"`, and the row gets `redacted=true`.

**Retention**
- Hot: 13 months.
- Archive: `archive/<YYYY-MM>/events.ndjson` plus `manifest.json` (first and last chain_seq, first prev_hash, last hash, count, sha256 of the ndjson, signature) in the Object-Lock bucket.
- `audit_purge_archived(through bigint)` deletes rows with `chain_seq ≤ through` only when an `audit_archives` row covers them with `verified_at` set, using `SET LOCAL audit.purge='on'`, which the trigger honours. An ordinary DELETE is still refused.

**Snapshots**
- `snapshot.account` (aggregate `account`). `after` = `{account: <masked row>, balances: {sub_account: normal balance}, snapshot_hash}`, with the hash computed over account + balances.

**Replay fields**
- From the account: `state, frozen, collateral_minor, credit_limit_minor, purchase_limit_minor, restricted_reason, delinquency, close_stage, clock_offset_seconds`.
- Ledger balances per sub-account.

**Auditor API**
- JWT `role: auditor`. Explicit events `audit.queried`, `audit.exported` and `audit.access_denied` (aggregate `audit`, actor = subject).

**Compatibility**
- Do not rename existing events or change their payloads.
- The HTTP middleware keeps skipping GETs.
- `TestCustomerJourney` must stay green in every task.

**Process**
- Commits end with the Co-Authored-By and Claude-Session lines.
- Stage explicit paths only.
- Migrations:
  - accounts `000017`, ledger `000005`, billing `000005`, gateway `000004`, audit `000002`+;
  - ADR-021.
- TDD.

## Review Focus
1. A row tampered with, deleted or reordered (triggers disabled): verify names the first bad `chain_seq`. Covered in Task 3.
2. Two audit consumers inserting concurrently: the chain stays contiguous and verifies. Covered in Task 3.
3. An SQS redelivery of an event already stored: no second row, no chain fork. Covered in Task 3.
4. A purge racing new inserts: only archived rows go, and the chain stays verifiable from the manifest. Covered in Task 7.
5. Secrets in exports or archives: none (grep). Covered in Tasks 3 and 7.

---

### Task 1: Contracts and producer numbering
**Files:**
- `proto/acme/audit/v1/audit.proto`: new service `AuditService` with these RPCs:
  - `QueryEvents{account_id, event_type, actor, from, to, page_token, page_size} → {events, next_page_token}`
  - `Verify{} → VerifyReport{chain_valid, first_bad_chain_seq, checked, checkpoints_valid, gaps[{source, from_seq, to_seq}], stuck_outbox[{source, count, oldest}]}`
  - `CreateCheckpoint{} → Checkpoint`
  - `RunReconciliation{} → ReconciliationReport{run_id, checks[{name, violations}], s3_key}` and `GetReconciliation{} → ReconciliationReport`
  - `Replay{account_id} → ReplayReport{account_id, differences[{field, audit, live}]}`
  - `CreateExport{filter, meta} → Export{id, s3_key, count, sha256}` and `GetExport{id} → {export, body}`
  - `RunRetention{now} → RetentionReport`
- `accounts.proto`: `rpc SnapshotAccounts(SnapshotAccountsRequest{account_ids}) returns (SnapshotAccountsResponse{count})`.
- `pkg/events`: `Event` gains `Source string`, `SourceSeq int64`.
- `pkg/outbox`:
  - `NewRelay(pool, pub, batch, source string)`;
  - `RelayOnce` assigns contiguous `source_seq` under `SELECT … FROM outbox_sequence WHERE source=$1 FOR UPDATE`.
- Migrations `outbox_sequence` in accounts `000017`, ledger `000005`, billing `000005` and gateway `000004`, each `INSERT (source, 1)`.
- Update every `NewRelay` call site (4 mains, the e2e stack, tests).
- Test: `pkg/outbox/outbox_integration_test.go`.

- [ ] **Step 1: Failing tests.**
  - `TestRelayNumbersContiguously`: 3 relays in parallel publish 300 events → `source_seq` values are exactly 1..300, each once.
  - `TestRelayRollbackLeavesNoGap`: the publisher fails once → retry → still contiguous.
- [ ] **Step 2:** Run `go test -tags=integration ./pkg/outbox/`. Expected: FAIL.
- [ ] **Step 3:** Implement. Run `buf lint && buf generate && go build ./...`.
- [ ] **Step 4:** Rerun the tests plus `go test -tags=integration ./tests/integration/ -run TestCustomerJourney`. Expected: PASS.
- [ ] **Step 5:** Commit `feat: audit contracts and gap-free per-source event numbering`.

### Task 2: accounts — `account.changed`, snapshots, sensitive reads
**Files:**
- Migration accounts `000018_account_changed_trigger`: a plpgsql AFTER INSERT/UPDATE trigger on `accounts` that inserts into `outbox` the envelope JSON (`id` gen_random_uuid, type `account.changed`, aggregate, actor `accounts`, `after = to_jsonb(NEW) - 'destination_ciphertext' - 'close_lease_owner' - 'close_lease_until' - 'close_checked_at'`, occurred_at now()). Skip when only lease or checked columns changed.
- `snapshot.go`: `SnapshotAccounts` and `RunSnapshots(ctx, every)` (once per UTC day, tracked in `snapshot_runs(day)`).
- `payouts.go`: `GetPayoutInstruction` emits `payout.instruction_read` (attempt_id, actor `bank`).
- billing `service.go`: `GetStatement` emits `statement.read`.

- [ ] **Step 1: Failing tests.**
  - `TestAccountChangedOnEveryMutation`: open, deposit, freeze, close step, clock advance → one `account.changed` per row change. The payload has the new values and no `destination_ciphertext`.
  - `TestLeaseOnlyChangeEmitsNothing`.
  - `TestSnapshotAccounts`: the snapshot event carries balances equal to the ledger, and `snapshot_hash` = sha256 of the canonical payload.
  - `TestRunSnapshotsOncePerDay`.
  - `TestInstructionReadAudited`.
  - `TestStatementReadAudited`.
- [ ] **Step 2–4:** RED → implement → GREEN (accounts and billing integration suites + TestCustomerJourney).
- [ ] **Step 5:** Commit `feat(accounts): account.changed trigger, daily snapshots, sensitive-read events`.

### Task 3: audit store — redaction, hash chain, dedupe
**Files:**
- Migration audit `000002_chain`:
  - columns `source, source_seq, chain_seq bigint UNIQUE, prev_hash, hash, redacted`;
  - `chain_head(id=1, chain_seq, hash)`;
  - backfill existing rows in `recorded_at, id` order (the trigger is disabled inside the migration only);
  - index `(source, source_seq)` UNIQUE where source is not null.
- `services/audit/internal/store/chain.go`: `Canonical(ev) []byte`, `Hash(prev, canonical) string`, `Redact(raw json.RawMessage) (json.RawMessage, bool)`.
- `store.go` `Record`: redact, then lock the head, dedupe by id (no chain advance on a duplicate), insert with chain fields, then update the head.

- [ ] **Step 1: Failing tests.**
  - `TestRedactKeysAnyDepth`: unit test. `authorization` objects are kept; `holder_name`/`account_number` nested in arrays are redacted.
  - `TestChainContiguousUnderConcurrency`: 4 goroutines × 250 Records → `chain_seq` 1..1000, each hash = recomputed.
  - `TestDuplicateDeliveryDoesNotFork`: the same event twice → one row, head unchanged.
  - `TestBackfillChainsExistingRows`: rows inserted before migration 2 are chained in order.
- [ ] **Step 2–4:** RED → GREEN.
- [ ] **Step 5:** Commit `feat(audit): redaction and hash chain`.

### Task 4: Verify, gaps, stuck outboxes, checkpoints
**Files:**
- `services/audit/internal/audit/verify.go`: walks the chain in pages of 5,000 recomputing hashes, finds per-source gaps in `source_seq`, and counts stuck outbox rows, unpublished > 5 min, by reading `<schema>.outbox` for the 4 producer schemas.
- `checkpoint.go`: signs, writes S3 with Object Lock retention, links to the previous checkpoint, and verifies all checkpoints against the recomputed chain.
- `services/audit/internal/audit/server.go`: the gRPC server.
- `services/audit/cmd/main.go`: serves gRPC on `:9005`; daily checkpoint job.
- `pkg/config`: `AUDIT_SIGNING_KEY`, `AUDIT_BUCKET`, `AUDIT_GRPC_ADDR`.
- `deploy/localstack/init-aws.sh`: bucket with object lock enabled.
- `deploy/docker-compose.yml`: audit GRPC_ADDR.
- Terraform `modules/s3`: audit bucket with `object_lock_enabled` + default retention COMPLIANCE 7 years.

- [ ] **Step 1: Failing tests** (Postgres + LocalStack S3).
  - `TestVerifyDetectsTamper`: change `after` of chain_seq 37 (trigger disabled in the test transaction) → `first_bad_chain_seq=37`.
  - `TestVerifyDetectsDeletion`.
  - `TestVerifyDetectsReorder`.
  - `TestGapDetection`: insert source_seq 1,2,4 for `ledger` → gap 3..3; a late delivery of 3 closes it.
  - `TestStuckOutbox`.
  - `TestCheckpointSignAndLink`: 2 checkpoints, both verify; tampering with the chain before checkpoint 1 → `checkpoints_valid=false`; an Object Lock retention is set on the object.
- [ ] **Step 2–4:** RED → GREEN.
- [ ] **Step 5:** Commit `feat(audit): chain verification, gap and outbox-lag detection, signed checkpoints`.

### Task 5: Replay and reconciliation
**Files:**
- `replay.go`: reads the latest `account.changed`/`snapshot.account` for the account plus the latest balances per sub-account from `ledger.journal_posted`/snapshot, then compares with the live state via accounts `GetAccount` + ledger `GetBalances` gRPC (the audit service gains accounts and ledger clients).
- `reconcile.go`: `tests/load/verify.sql` is embedded (copy to `services/audit/internal/audit/verify.sql`, kept identical by a test). Each check is run, a report goes to `reconciliation/<date>/<run>.json`, `reconciliation.completed` is emitted, and `reconciliation.violation` is emitted per check > 0 through the audit service's own outbox (new migration audit `000003_outbox`; relay source `audit`).
- Daily jobs in main.

- [ ] **Step 1: Failing tests.**
  - `TestReplayCleanAccount`: full flow → no differences.
  - `TestReplayDetectsLiveCorruption`: update `accounts.credit_limit_minor` directly → one difference with field, audit value and live value.
  - `TestReplayLedgerBalance`.
  - `TestReconciliationCleanAndViolation`: a corrupted row → violation event + S3 report.
  - `TestEmbeddedVerifySQLMatchesLoadFile`.
- [ ] **Step 2–4:** RED → GREEN.
- [ ] **Step 5:** Commit `feat(audit): replay against live state and daily reconciliation`.

### Task 6: Gateway — auditor role and routes
**Files:** `services/gateway/internal/server/audit_api.go` + test, `server.go` (group `/v1/audit` with `RequireRole(RoleAuditor)` and a denial hook emitting `audit.access_denied` into the gateway outbox), `pkg/auth` (`RoleAuditor`), `cmd/devtoken --role auditor`, gateway main (audit client).

**Routes (auditor):**
- `GET /v1/audit/events`, `GET /v1/audit/verify`
- `POST /v1/audit/checkpoints`
- `GET|POST /v1/audit/reconciliation`
- `GET /v1/audit/replay/{accountID}`
- `POST /v1/audit/exports`, `GET /v1/audit/exports/{exportID}`
- `POST /v1/audit/snapshots` (calls accounts `SnapshotAccounts`)

Each successful call emits `audit.queried` (or `audit.exported`) with subject, route and filter.

- [ ] **Step 1: Failing tests.**
  - Auth matrix: customer and support get 403 plus an `audit.access_denied` event; auditor gets 200.
  - Strict JSON.
  - The queried/exported events are emitted.
  - The `/v1/audit/events` filters pass through.
  - The simulator-coverage test stays red until Task 8.
- [ ] **Step 2–4:** RED → GREEN.
- [ ] **Step 5:** Commit `feat(gateway): auditor role and audit API`.

### Task 7: Exports and retention
**Files:**
- `export.go`: NDJSON of the filtered events (already redacted), its sha256, stored in S3 `exports/<id>.ndjson`; a row in `audit_exports`.
- `retention.go`: monthly job doing archive, then verify (recompute the hash range and compare with the manifest), then purge through `audit_purge_archived`.
- Migration audit `000004_retention`: `audit_archives`, `audit_exports`, `audit_purge_archived` function, trigger update honouring `audit.purge`.
- `cmd/auditctl` (archive|verify|purge|checkpoint|verify-chain).

- [ ] **Step 1: Failing tests.**
  - `TestExportHasNoSecrets`: an export of a full close flow → no account number, no ciphertext, no JWT pattern; sha256 matches.
  - `TestRetentionArchiveVerifyPurge`: rows older than 13 months (fixture occurred_at/recorded_at) are archived, then purged; the chain verifies from the manifest's last hash; newer rows are untouched.
  - `TestPurgeRefusedWithoutArchive`.
  - `TestPlainDeleteStillRefused`.
  - `TestPurgeRacesInserts`: concurrent Records during a purge → nothing unarchived is removed.
- [ ] **Step 2–4:** RED → GREEN.
- [ ] **Step 5:** Commit `feat(audit): exports and archive-then-purge retention`.

### Task 8: Simulator — "9 · Audit"
`web/lib/ui-actions.json` + panel:
- auditor token;
- search form (account, type, actor, dates) with a results table;
- buttons: Verify chain, Create checkpoint, Run reconciliation, Snapshot now, Replay account, Export and download.

`web/lib/audit.ts` + test for query string building. Then `/browse` the flow.

- [ ] Steps: failing web test → implement → `make web-test && go test ./services/gateway/...` (coverage green) → browser check → commit `feat(web): audit panel`.

### Task 9: Testing flow — e2e, load gate, docs
- `tests/integration/sprint6_test.go`:
  - auditor e2e: search; verify valid; tamper (direct SQL) → verify names the seq; checkpoint; reconciliation clean; replay clean; export without secrets.
  - The e2e stack gains the audit gRPC server.
- `tests/load`: after k6, a script (`tests/load/audit_gate.sh`) calls snapshot, verify, replay over 50 random accounts and reconciliation via the auditor API. Expected: chain valid, gaps 0, stuck 0, differences 0, violations 0.
- Run the load in an isolated compose project.
- Docs: ADR-021, SPRINTS, README (auditor API, keys), `docs/testing/sprint6.md`, PROMPTS.
- Full suite.
- [ ] Commit `test: Sprint 6 e2e and load audit gate; docs (ADR-021)`.
