# Customer journey — end to end with timings and intruder probes (2026-10-25)

`tests/integration/journey_test.go` (`TestCustomerJourney`) walks one customer through Sprints 1–4
over the real gateway, gRPC services, Postgres and LocalStack audit, and at every stage lets an
"intruder" (a second, legitimate customer; a forged token; an unsigned or tampered webhook) try
what an attacker would. Every step is timed; v1 reports the numbers and asserts none of them.

## Run

```bash
make test-integration                                   # whole suite, journey included
JOURNEY_REPORT=/abs/path/journey.md \
  go test -tags=integration -race -count=1 -run TestCustomerJourney -v ./tests/integration/
```

`JOURNEY_REPORT` (absolute path) receives the markdown table below; the same table is in the
`-v` log. The journey stack uses two test-only knobs (`startStackWith`): card velocity 30/min
(default 10; the journey authorizes more than 10 times on one card within a minute) and gateway
rate limit 500/min per subject (default in tests 100,000).

## Phases

| Phase | Lifecycle | Intruder |
|---|---|---|
| onboarding | open → deposit intent → bank settles → card active (limit = collateral) | same key replay / other body; `alg=none`, expired, foreign-secret tokens; token without accounts; existence oracle; unknown field and amount 0 on the intent route and on the webhook; bad signature; stale timestamp; 2 MiB body with a bad signature; amount mismatch; webhook replay; `external_ref` reuse |
| spending | 30,000 purchase; 5,000 reversal; 5,000 refund; declines (currency, credit, purchase limit, frozen) | over-refund; over-capture; double capture; double reversal; velocity (31st attempt); intruder mutations (pay, limit, freeze, clock); unsigned / wrong-secret network calls; tampered page token on payments and transactions |
| billing-1 | statement 1 (25,000; min 2,500; no interest); pay minimum; bank returns it with exposure over the limit → `payment_returned`; pay again → lifted | below-minimum and over-balance payments; 20 concurrent creates with one key; same key string under another tenant; wrong-amount settlement; double return |
| billing-2 | statement 2 with interest; pay in full → grace; statement 3 at 0; 10,000 purchase; statement 4; miss minimum → `past_due`, 25.00 fee, decline; support waiver cures | customer on the support route; support token on a customer route; waiver replay |
| charge-off | 200 days unpaid → `charged_off`, collateral applied (= limit reduction), decline | payment after charge-off (observed, not asserted) |
| closing-checks | every statement document matches its S3 hash; exactly one `http.request` audit event per mutating authenticated call (GETs are not audited by design) | statement of customer A under customer B's path (IDOR); waiver of A's fee under B's path; burst by one subject → first 429 at call 501 (a boundary straddle may push it to ≤ 540) |

Every response body, success or error, is scanned for internals (database and RPC errors, Go
traces, internal column names such as `pending_op`/`lease_until`, secrets, host paths); none
leaked.

## First green run — timings (laptop, in-process stack, `-race`)

| Phase | Step | Kind | Status | ms |
|---|---|---|---|---|
| onboarding | open account | http | 201 | 75 |
| onboarding | open again, same key | http | 201 | 11 |
| onboarding | open same key, other body | http | 422 | 25 |
| onboarding | audit: account.opened | async | - | 338 |
| onboarding | token: alg none | http | 401 | 1 |
| onboarding | token: expired | http | 401 | 0 |
| onboarding | token: foreign secret | http | 401 | 0 |
| onboarding | token: valid, no accounts | http | 403 | 3 |
| onboarding | tenancy: intruder reads customer account | http | 403 | 4 |
| onboarding | tenancy: intruder reads random account | http | 403 | 5 |
| onboarding | deposit intent 100,000 | http | 201 | 19 |
| onboarding | deposit: unknown field | http | 400 | 5 |
| onboarding | deposit: amount 0 | http | 400 | 11 |
| onboarding | webhook: unknown field | http | 400 | 4 |
| onboarding | webhook: amount 0 | http | 400 | 6 |
| onboarding | webhook: bad signature | http | 401 | 2 |
| onboarding | webhook: stale timestamp | http | 401 | 1 |
| onboarding | webhook: oversize body, bad signature | http | 413 | 23 |
| onboarding | webhook: amount mismatch | http | 400 | 16 |
| onboarding | webhook: settle deposit | http | 200 | 55 |
| onboarding | account active after webhook | async | - | 12 |
| onboarding | webhook: replay | http | 200 | 15 |
| onboarding | second deposit intent | http | 201 | 12 |
| onboarding | webhook: same external_ref, other deposit | http | 422 | 9 |
| onboarding | audit: collateral.confirmed | async | - | 48 |
| onboarding | audit: card.activated | async | - | 0 |
| spending | authorize 30,000 | http | 201 | 49 |
| spending | capture 30,000 | http | 201 | 26 |
| spending | audit: authorization.captured | async | - | 23 |
| spending | authorize 5,000 (to reverse) | http | 201 | 26 |
| spending | reversal | http | 200 | 28 |
| spending | reversal twice | http | 409 | 13 |
| spending | refund 5,000 | http | 201 | 39 |
| spending | refund over captured | http | 409 | 13 |
| spending | authorize 1,000 (capture over) | http | 201 | 59 |
| spending | capture over authorized | http | 400 | 13 |
| spending | reversal (cleanup) | http | 200 | 36 |
| spending | capture twice | http | 409 | 5 |
| spending | decline: currency | http | 201 | 13 |
| spending | decline: insufficient credit | http | 201 | 21 |
| spending | set purchase limit 1,000 | http | 200 | 23 |
| spending | decline: purchase limit | http | 201 | 25 |
| spending | clear purchase limit | http | 200 | 24 |
| spending | freeze | http | 200 | 27 |
| spending | decline: frozen | http | 201 | 16 |
| spending | unfreeze | http | 200 | 14 |
| spending | velocity: 31 authorizations | http | 201 | 997 |
| spending | tenancy: intruder pays | http | 403 | 18 |
| spending | tenancy: intruder sets limit | http | 403 | 12 |
| spending | tenancy: intruder freezes | http | 403 | 9 |
| spending | tenancy: intruder advances clock | http | 403 | 10 |
| spending | network: unsigned | http | 401 | 1 |
| spending | network: bank secret | http | 401 | 3 |
| spending | payments: tampered page token | http | 400 | 5 |
| spending | transactions: tampered page token | http | 400 | 13 |
| spending | transactions list | http | 200 | 18 |
| billing-1 | close cycle 1 (+31d) | http | 200 | 103 |
| billing-1 | audit: statement.generated | async | - | 0 |
| billing-1 | pay 50 (below 1.00) | http | 400 | 19 |
| billing-1 | pay 1,000,000 (over balance) | http | 409 | 18 |
| billing-1 | pay 2,500 ×20 concurrent, same key | http | 201 | 170 |
| billing-1 | other tenant, same key string | http | 201 | 126 |
| billing-1 | bank settles wrong amount | http | 400 | 13 |
| billing-1 | bank settles 2,500 | http | 200 | 84 |
| billing-1 | outstanding 22,500 after settlement | async | - | 13 |
| billing-1 | audit: payment.settled | async | - | 89 |
| billing-1 | authorize 76,000 (spend freed credit) | http | 201 | 21 |
| billing-1 | capture 76,000 | http | 201 | 30 |
| billing-1 | bank returns the payment | http | 200 | 64 |
| billing-1 | restricted after return (exposure 101,000 > limit) | async | - | 12 |
| billing-1 | decline: payment_returned | http | 201 | 32 |
| billing-1 | return twice, other ref | http | 409 | 7 |
| billing-1 | pay 2,500 again | http | 201 | 20 |
| billing-1 | pay 2,500 again: bank settles | http | 200 | 43 |
| billing-1 | restriction lifted | async | - | 6 |
| billing-1 | audit: card.restricted | async | - | 1 |
| billing-1 | audit: card.unrestricted | async | - | 69 |
| billing-2 | close cycle 2 (+30d) | http | 200 | 125 |
| billing-2 | audit: interest.charged | async | - | 0 |
| billing-2 | pay in full 100345 | http | 201 | 20 |
| billing-2 | pay in full 100345: bank settles | http | 200 | 54 |
| billing-2 | paid in full | async | - | 8 |
| billing-2 | close cycle 3 (+31d) | http | 200 | 116 |
| billing-2 | authorize 10,000 | http | 201 | 44 |
| billing-2 | capture 10,000 | http | 201 | 48 |
| billing-2 | close cycle 4 (+31d) | http | 200 | 92 |
| billing-2 | miss the minimum: due + 15d (+36d) | http | 200 | 181 |
| billing-2 | decline: past_due | http | 201 | 13 |
| billing-2 | support boundary: customer waives | http | 403 | 1 |
| billing-2 | support boundary: support reads account | http | 403 | 6 |
| billing-2 | support waives the fee | http | 201 | 134 |
| billing-2 | waiver replay, same key | http | 201 | 10 |
| billing-2 | audit: fee.charged | async | - | 1 |
| billing-2 | audit: charge.waived | async | - | 24 |
| charge-off | stop paying (+200d) | http | 200 | 623 |
| charge-off | charged off | async | - | 7 |
| charge-off | decline: charged_off | http | 201 | 16 |
| charge-off | payment after charge-off | http | 409 | 14 |
| charge-off | audit: account.charged_off | async | - | 0 |
| closing-checks | statement 1.json | http | 200 | 5 |
| closing-checks | statement 2.json | http | 200 | 7 |
| closing-checks | statement 3.json | http | 200 | 14 |
| closing-checks | statement 4.json | http | 200 | 6 |
| closing-checks | statement 5.json | http | 200 | 8 |
| closing-checks | statement 6.json | http | 200 | 10 |
| closing-checks | statement 7.json | http | 200 | 6 |
| closing-checks | statement 8.json | http | 200 | 4 |
| closing-checks | statement 9.json | http | 200 | 6 |
| closing-checks | statement 10.json | http | 200 | 7 |
| closing-checks | statement 11.json | http | 200 | 14 |
| closing-checks | IDOR: customer statement under intruder account | http | 404 | 10 |
| closing-checks | IDOR: waiver of customer fee under intruder account | http | 404 | 21 |
| closing-checks | audit: http.request coverage | async | - | 4 |
| closing-checks | rate limit burst (until 429) | http | 429 | 2463 |

HTTP steps: 95, p50 15 ms, p95 170 ms, max 2463 ms

Async rows measure delivery: account state visible after the bank webhook, and outbox → SNS → SQS →
`audit_events` for each domain event (0–106 ms here). HTTP p95 is dominated by the clock advances,
which run whole billing cycles per call, and by the two deliberate multi-call steps (velocity: 31
authorizations; rate limit: 501 calls). An earlier run without the sibling knob test in the same
process measured p50 5 ms, p95 66 ms.

## Security probe matrix (observed)

| Probe | Response |
|---|---|
| JWT `alg=none`, expired, signed with another secret | 401 `unauthorized`, same body for all three |
| Valid JWT without the account in `authorized_accounts` | 403 `account_not_authorized` |
| Intruder reads an existing vs a random account | 403, byte-identical bodies (no existence oracle) |
| Intruder pays / sets limit / freezes / advances the clock | 403; state unchanged |
| Customer on the support waiver route; support token on a customer route | 403 / 403 |
| Webhook: bad signature, stale timestamp (10 min), wrong secret, unsigned | 401 |
| Webhook: 2 MiB body with a bad signature | 413 `body_too_large` (size is checked before the signature) |
| Webhook: unknown JSON field / `amount_minor: 0` / amount mismatch | 400 `invalid_body` / 400 `amount_mismatch` / 400 `amount_mismatch`; nothing applied |
| Webhook replay (same `external_ref`) | 200 replay, one settlement, one `collateral.confirmed` |
| Same `external_ref` on another deposit | 422 `external_ref_reused` |
| Same `Idempotency-Key`, other body | 422 `idempotency_key_reuse` |
| Same key ×20 concurrent | 201 ×20, one payment |
| Same key string, other subject | independent (201, not replayed) |
| Unknown JSON field | 400 `invalid_body` |
| Tampered `page_token` on payments and transactions | 400 `invalid_page_token` |
| Capture > authorized / twice; refund > captured; reversal twice | 400 `invalid_capture_amount` / 409 `already_captured` / 409 `refund_exceeds_captured` / 409 `already_reversed` |
| Payment < 1.00 / > balance | 400 `invalid_amount` / 409 `payment_exceeds_balance` |
| Statement of A requested under B's account path | 404 |
| Waiver of A's fee under B's account path (support) | 404, A's fee unchanged |
| Burst by one fresh subject | first 429 `velocity_limit_exceeded` at call 501, numeric `Retry-After` |

## Findings

No defect. Observations worth knowing (all by design, confirmed in code):

- A payment return always restricts the card fail-safe and lifts at once when exposure is within
  the limit, so `card.restricted` and `card.unrestricted` can be emitted back to back. The journey
  spends the freed credit before the return to exercise the sustained restriction (ADR-018).
- `billing.grace` reports whether the *next* cycle is interest-free: it is `false` right after an
  unpaid statement and `true` after paying in full.
- The collateral application appears on the charge-off cycle's statement; later zero-balance
  cycles keep generating statements (11 after +200 days).
- After the charge-off applied the collateral the debt was covered: a payment attempt answers
  409 `nothing_to_pay`.
- Reversal answers 200 (state change on an existing authorization); authorize, capture and refund
  answer 201.
- Webhook 401 bodies carry the verifier's reason as `error.code` (e.g. signature or timestamp);
  informational, no secret material.
- GET requests are not audited (`http.request` is emitted for mutating requests only), and a
  403 from the support-role check happens before the audit middleware. The completeness check
  counts exactly those calls.

## Not covered

- Forced 5xx responses (an upstream outage) cannot be produced by the in-process harness; the
  gateway's "5xx is not stored or replayed" rule is covered by the ADR-017 tests in
  `pkg/idempotency` and by the live fault-injection probes recorded in `docs/TESTING.md`.
- Sprint 5 (close / cancel / payout) is not in the journey yet.
