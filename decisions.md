# Decisions

This file records the decisions we make in acme-card, and how we reached each one. It explains the reasoning behind them. The formal, append-only log of each decision lives in [`docs/DECISIONS.md`](docs/DECISIONS.md) (ADR-001 onward), and the raw prompts are in [`PROMPTS.md`](PROMPTS.md).

## How we decide

1. **Research first.** Each topic starts with our own research into how identity and transactions should work in a fintech setting: who is allowed to move money, how a movement is proven to have happened exactly once, and how it is audited later.
2. **Plan with what we know.** We write a plan from that research (sprint specs in `docs/superpowers/specs/`, sprint list in `docs/SPRINTS.md`).
3. **Complete the plan with AI.** AI fills the gaps in the plan and offers several solutions, each with its trade-offs.
4. **Choose for the moment.** We pick the option that fits best at that point in the project. That is not always the most complete option. A choice can be replaced later when the project needs it, and the replacing ADR says which one it supersedes (for example, ADR-017 supersedes the row-lock design of ADR-004).
5. **Record it.** The choice goes into `docs/DECISIONS.md` as context → decision → consequences, and its reasoning is summarized here.

## How we work with the AI

We follow a spec-driven process: nothing is built until it is written down as a spec, then as a plan, and every step is checked by tests. The AI is a partner in each phase, but every decision is ours.

### 1. Discovery: the AI asks, we answer

- Every sprint opens with a short prompt from us ("let's plan sprint 3"), written in our own words and often in Spanish.
- The AI reads the codebase and the earlier ADRs, then comes back with numbered questions. Each question offers options and the AI's recommendation (for example: overdraft yes/no, rate-limit backend, what happens to a credit balance, how payments are confirmed).
- We answer each point. Some answers accept a recommendation ("I accept point 8"), some change it, and some knowingly accept a risk ("returns: accept the risk").
- The AI asks a second, shorter round of clarifications for anything still ambiguous. Those answers are recorded too.
- Every prompt is kept word for word in `PROMPTS.md`, so it is always possible to trace why something was built the way it was.

### 2. Spec: what we are building

- The answers become a design spec in `docs/superpowers/specs/<date>-<sprint>-design.md`: scope, states, rules, error codes, what is out of scope, and what must be tested.
- The spec states limits and numbers explicitly (windows, timeouts, retry schedules, caps), so they can be tested rather than debated later.

### 3. Plan: how we build it

- The spec becomes a step-by-step plan in `docs/superpowers/plans/`. It is split into tasks, and each task lists the files it touches, what it produces and consumes, and the tests that prove it.
- Before work starts, a pre-flight check confirms that each task's inputs match the outputs of the tasks before it.

### 4. Build: one task at a time, test first

- Tasks are carried out one by one by AI sub-agents, each from a written brief (`.superpowers/sdd/<plan>/task-N-brief.md`).
- TDD is mandatory: the failing test is written and run before any production code.
- Each task ends with its test command and output logged (`task-N-tests.log`) and a line in the ledger (`progress.md`) with the commit range.
- When the plan is silent or unclear, the agent makes a small **ruling** and writes it in the ledger with its "cost if wrong". Rulings that matter end up in the sprint's ADR.

### 5. Review: before anything is merged

- Every sprint gets a final code review. Its findings are fixed on the same branch and recorded ("final review fixes" in ADR-013, ADR-016, ADR-021).
- Results are written to `docs/TESTING.md` and `docs/testing/<sprint>.md`. A gate that does not pass is reported as not passing, with its cause.
- Results are written to `docs/TESTING.md` and `docs/testing/<sprint>.md`. A gate that does not pass is reported as not passing, with its cause.
- Each sprint lives on its own branch (`sprint-N`) and is merged into `main` only when we say it is done.

## Language: Go

We use Go for every service that holds business logic or money (ADR-001). The web flow simulator stays in Next.js, but it is a dev tool and not part of the money path (ADR-011).

Why Go and not TypeScript/Node or Python:

- **Numbers.** In JavaScript, every number is a float64 by default, so money arithmetic needs care everywhere. Go has native `int64`, and we store every amount as integer minor units plus an ISO-4217 currency. No floats anywhere in the ledger (ADR-003).
- **Concurrency.** Goroutines and channels make concurrent requests, background workers (sweepers, reconcilers, outbox relays, notification workers) and waiting duplicates cheap. A duplicate request waiting on an idempotency key holds a goroutine, not a database connection (ADR-017).
- **Small binary, fast builds.** Each service compiles to a single static binary. It builds faster and ships in a smaller image than a Node or Python service with its runtime and dependencies.

## Idempotency

Idempotency is the main focus of the project. The core test is many concurrent attempts of the same transaction in a short time: the result must be exactly one effect (one deposit, one charge).

### How a key is set

- Every mutating REST request (`POST`, `PUT`, `PATCH`, `DELETE`) must send an `Idempotency-Key` header. Without one → `400 idempotency_key_required`. Keys longer than 255 characters → `400 idempotency_key_invalid`.
- The key is scoped to who sent it: stored as `sha256(subject ‖ 0 ‖ key)`, where the subject is the JWT `sub` or the webhook source. Two customers sending the same key never collide.
- The request itself is also fingerprinted as `sha256(method | path | body)`, so a key is bound to one exact request.
- Bodies are capped at 1 MiB.

Code: `pkg/idempotency` (`hash.go`, `store.go`, `httpmw.go`).

### How long a key lives

- **TTL: 24 hours** (`IDEMPOTENCY_TTL`, default `24h`). Within that window, the same key replays the original response.
- A gateway sweeper deletes expired keys **every hour**.
- **Lease: 30 seconds** (three times the gateway timeout). This is how long one request owns a key while it is running. If the owner dies or runs past the lease, the next identical request takes the key over. A stale owner cannot overwrite the takeover, because `Complete` and `Abort` are guarded by owner.

### How we avoid duplicates

- **Insert first.** The first request inserts an `in_progress` row and commits it straight away. No transaction stays open while the handler runs, so slow requests cannot drain the connection pool (ADR-017, which replaced the row lock of ADR-004).
- **Duplicates wait, then replay.** A concurrent duplicate re-reads the row every 25 ms for up to 5 s. If the first request finishes in that time, the duplicate gets the stored status and body, with the header `Idempotency-Replayed: true`.
- **What is stored.** 2xx and 4xx responses are stored and replayed, together with an outcome (`success`, `client_error`, `rolled_back`, …). 5xx responses are **not** stored: the key is released, so a retry after an outage can still succeed.
- **Second line of defense.** Every downstream operation is idempotent on its own anchor as well: the deposit key hash, `external_ref` for bank webhooks, `network_ref` for card network events, and journal references such as `deposit:<id>` in the ledger. A retry after a timeout whose effect had already landed resolves to that effect and does not create a new one.
- **Secrets are not replayed.** A route can redact what gets stored. For example, a webhook subscription's secret is shown once and replays as `"[shown once]"`.

### Many requests from the same webhook

- **Inbound bank webhooks** (deposits, payments) are HMAC-SHA256 signed over `timestamp.body`, with ±5 minutes tolerance. They do not use `Idempotency-Key`. They are deduplicated by `external_ref`, so the bank can resend as often as it wants and we settle once (ADR-009, ADR-012).
- A webhook with an amount or currency mismatch → `400`. The deposit stays `pending`, so a correct resend can still settle it.
- Rejected webhooks (bad signature, stale timestamp) are deduplicated to one audit row per (source, 5-minute window), so a flood cannot fill the audit log (ADR-006, ADR-013).
- **Outbound partner webhooks** (Sprint 7) are delivered at least once and signed with `X-Acme-Signature`. They carry `X-Acme-Event-Id` so the partner can deduplicate. Each event is processed once per channel even under SQS redelivery and concurrent workers. Retries back off at 1 min, 5 min, 30 min, 2 h and 6 h (6 attempts, about 8.6 h), then the delivery is marked `failed`.

### When we block

| Situation | Response |
|---|---|
| Missing or over-long key | `400` |
| Same key, different request (method, path or body) | `422 idempotency_key_reuse` |
| Same key still running after the 5 s wait | `409 request_in_progress` with `Retry-After: 1` |
| Idempotency store unreachable | `503 idempotency_unavailable` (fail closed: never run unprotected) |
| Reused bank or return reference with different data | `422 external_ref_reused` / `return_ref_reused` |
| Too many attempts per subject | Velocity limit: Postgres sliding window, 10 authorization attempts per minute → `velocity_limit_exceeded` |

## Audit

We built a full auditor over every data movement in the database (ADR-005, ADR-021).

- **Nothing is missed.** Each service writes its domain events to an `outbox` table in the same transaction as the state change. A relay ships them through SNS → SQS to the `audit` service. Account changes are emitted by a database trigger, so no code path can forget one.
- **Nothing is altered.** `audit_events` is append-only: its role has no UPDATE or DELETE, and triggers reject both. Events are hash-chained (`sha256(prev_hash ‖ canonical)`) and numbered without gaps per source, so a missing or edited event is detected.
- **Provable later.** Every day, the chain head is signed with Ed25519 and written to an S3 Object Lock bucket (7 years). Verification walks the chain, and daily reconciliation runs 36 invariants against live data.
- **Private.** Sensitive fields (account numbers, names, tokens, secrets) are redacted before hashing.
- **Retention.** After 13 months, events are archived to the locked bucket, verified, and only then purged.
- **Auditor role.** A read-only JWT role can search, verify, replay, export and snapshot. Every auditor call is itself audited.

## Tests and environments

### Environments

| Environment | What it is | Command |
|---|---|---|
| Local stack | docker-compose: one Postgres (one schema per service), LocalStack for SNS, SQS, S3 and KMS, the six Go services, two gateway replicas behind an nginx load balancer (standing in for the AWS ALB), and the Next.js simulator | `make up` / `make down` / `make logs` |
| Test containers | Integration tests start their own Postgres and LocalStack with testcontainers, so they never touch the dev data | `make test-integration` |
| AWS (described, not applied) | Terraform in `infra/terraform` with `envs/dev` and `envs/prod`: TLS 1.3, KMS, RDS Multi-AZ, S3 Object Lock. Only `terraform validate` runs (ADR-008) | `make tf-validate` |

Dev tools: `make devtoken` mints a JWT, `make banksim` sends a signed bank webhook, `make auditctl` runs the audit operator CLI, and the simulator at `:3000` (plus the cardholder portal at `/portal`) drives every flow from a browser. The simulated account clock only exists when `DEV_CLOCK_ENABLED=1`, and the gateway refuses to start with it unless `APP_ENV=dev`.

### Test layers

| Layer | What it proves | Command |
|---|---|---|
| Unit | State machines, money math, hashing, middleware; always with `-race` | `make test` |
| Integration | The full flow of every sprint against real Postgres and LocalStack; the ledger is balanced after every transaction | `make test-integration` |
| Concurrency | Same `Idempotency-Key` ×50 → exactly one effect and identical responses; same bank webhook ×50 → one journal entry | part of integration |
| Security | Bad or stale HMAC → 401; account not in the JWT → 403; unknown JSON field → 400 | part of integration |
| Audit | Every mutation writes at least one audit event; `UPDATE`/`DELETE` on `audit_events` fails | part of integration |
| Customer journey | `TestCustomerJourney`: the full customer lifecycle with per-step timings and intruder probes (`docs/testing/journey.md`) | part of integration |
| Load | k6 with 1000 virtual users on mixed flows: zero duplicate journal entries per key, p95 reported | `make load` |
| Invariants | `tests/load/verify.sql`: 36 SQL checks on the live data after load (balanced ledger, no orphan holds, …) | `make load-verify` |
| Simulator | HMAC and JWT byte-compatible with Go | `make web-test` |

Load numbers come from a developer laptop. They prove correctness under concurrency, not production capacity.

## How we improve security and architecture

We do not treat a sprint as done once its tests pass. On top of the per-sprint reviews, we periodically run dedicated passes that attack the running system.

- **Security baseline from day one (ADR-006).** JWT with an `authorized_accounts` claim: the gateway denies any account not in it. Strict JSON (unknown fields rejected). Signed webhooks with a timestamp window. Rate and velocity limits.
- **Audit passes against the running stack (ADR-017).** 68 black-box probes, fault injection (stopping or pausing services in the middle of a request), and the SQL invariants. Each confirmed defect gets an ID (H1–H4), is fixed test first, and is re-probed. Findings deliberately left out of scope are listed, not hidden.
- **Fixes that changed the architecture.** That pass replaced the idempotency row lock with leases (no open transaction per request), stopped storing 5xx responses, made refused ledger operations terminal instead of stuck, and reordered hold release to close a race.
- **Data protection.** Payout destinations are encrypted with AES-256-GCM (KMS in AWS); events, logs and responses only carry `****<last4>`. Secrets are shown once and never replayed. Audit data is redacted before hashing.
- **Performance tied to correctness.** Locks are never held across a ledger call. The rate limiter is a single SQL statement that stays exact under concurrency. Velocity runs inside the authorization transaction after a pool deadlock was found under load. A/B load runs separate real regressions from a busy host.
- **Every change is traceable.** Each fix lands as an ADR that names what it supersedes, together with the test that proves it.
