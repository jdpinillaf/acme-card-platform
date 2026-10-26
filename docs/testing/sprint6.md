# Sprint 6 — test results (2026-10-25)

Branch `sprint-6`. Design: `docs/superpowers/specs/2026-10-25-sprint6-audit-design.md`; decisions: ADR-021.

## Automated suites
- **Unit**:
  - redaction at any depth and case; canonical form survives a jsonb round trip; hash chain from genesis;
  - gateway auditor API: role matrix (customer/support 403 + `audit.access_denied`, anonymous 401), every route, strict JSON, bad dates, meta-audit counts;
  - web: auditor query builder, token roles, proxy target with query string.
- **Integration (testcontainers)**:
  - **Outbox:** contiguous per-source numbering; a rolled-back publish leaves no gap.
  - **Accounts:** `account.changed` trigger (lease-only changes emit nothing); `snapshot.account` with balances and hash; once-per-day run; background run; account with no ledger yet; `payout.instruction_read`.
  - **Billing:** `statement.read`.
  - **Audit store:** chain of concurrent consumers stays contiguous; SQS redelivery adds nothing; backfill of pre-chain rows.
  - **Audit service:**
    - verify names the first bad `chain_seq` after a tamper, a deletion or a reorder; per-source gaps after the grace; stuck outboxes;
    - checkpoints: signature, link, Object-Lock retention (LocalStack), a full re-hash caught at the checkpoint;
    - incremental verify after a recorded full walk; a re-hash before its anchor is caught;
    - replay clean and with a corrupted live row; reconciliation (embedded `verify.sql` identical to `tests/load/verify.sql`);
    - query filters and paging;
    - export without secrets (including a legacy unredacted row) and sha256;
    - retention: archive → verify → purge, chain verifiable from the manifest, purged checkpoint anchor checked against the archive, forged archive detected; purge refused without a verified archive or with a hand-set flag; plain DELETE/UPDATE/TRUNCATE still refused; purge racing 200 inserts.
- **End to end** (`tests/integration/sprint6_test.go`) through the gateway: a full life with a close and payout, then as auditor: customer and support refused (audited); search returns the chained trail without the destination; verify, checkpoint, reconciliation and replay clean; export without secrets matching its sha256; a tampered row is named by verify.

## Browser (simulator, gstack headless browser, dev data: 650k events, 29.7k accounts)
- Panel 9 as auditor: search by account (chained rows, hashes, source numbers), verify, checkpoint, run/last reconciliation (0 violations, 36 checks), snapshot now, replay (no differences), export (2 events + sha256) and download. No console errors.
- **Findings, fixed:**
  - Verify chain → 504: a full walk of 650k events takes ~10 s. Full walks now run daily / in `auditctl` and are recorded; the API verifies from the last one.
  - Snapshot now → 504 (29.7k accounts): now in the background (202).
  - An unfiltered export of the dev trail is refused (409, over 100,000 events); the panel shows the error.
- Startup on the dev data chains the 640k pre-Sprint-6 rows once: ~2.5 min, consumption resumes after.

## Audit gate (`tests/load/audit_gate.sh`)
Snapshot all → wait for them in the trail → checkpoint → verify (+ full walk via `auditctl`) → replay 50 random accounts → reconciliation. Paced under the 60 requests/minute per-subject limit.
- **Dev stack:** first run found every all-accounts snapshot aborting at ~11k of 29.7k (23 accounts whose ledger was never opened → NotFound). Fixed; then all PASS: 29,682 snapshots, full walk 740,338 events, 50 replays 0 differences, 0 violations.

## Load — 1000 VUs, isolated compose project `acme-card-s6` (fresh volumes, `RETURN_WINDOW=1m`)

| Metric | Result |
|---|---|
| Requests | 85,684; **0 failed** |
| Checks | **100%** (85,330 / 85,330) |
| Statements generated | 3,554 accounts, 3,554 statements |
| Accounts closed end to end | 154 |
| wrong balance / final state / billing state / close state | 0 / 0 / 0 / 0 |
| p95 overall | **1.23 s** (gate 1.5 s) |

**Audit gate after the load.** First run: checkpoints invalid, "anchored chain_seq 0 not found" — the startup checkpoint had anchored the empty chain. Fixed (empty chains are not anchored; a genesis anchor verifies). Rerun, all PASS:
- 3,554 snapshots in the trail; chain valid, 4 checkpoints valid, 0 gaps, 0 stuck outboxes;
- full walk 245,662 events;
- replay of 50 random accounts: 0 differences;
- reconciliation: 0 violations / 36 checks; `verify.sql`: 0 violations.
