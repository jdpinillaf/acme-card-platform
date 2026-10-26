import { test } from "node:test";
import assert from "node:assert/strict";
import { auditEventsQuery, gatewayTarget, tokenRole } from "./audit.ts";

test("auditEventsQuery keeps only set filters, in a stable order", () => {
  assert.equal(auditEventsQuery({}), "");
  assert.equal(auditEventsQuery({ account_id: " acc-1 ", event_type: "", actor: "cust-1" }), "?account_id=acc-1&actor=cust-1");
  assert.equal(auditEventsQuery({ event_type: "payout.created", page_size: 50 }), "?event_type=payout.created&page_size=50");
});

test("auditEventsQuery turns date inputs into RFC 3339 UTC", () => {
  assert.equal(auditEventsQuery({ from: "2026-10-01T00:00:00Z", to: "2026-10-25T12:30:00Z" }),
    "?from=2026-10-01T00%3A00%3A00.000Z&to=2026-10-25T12%3A30%3A00.000Z");
});

test("auditEventsQuery escapes values and rejects bad input", () => {
  assert.equal(auditEventsQuery({ actor: "a&b=c" }), "?actor=a%26b%3Dc");
  assert.throws(() => auditEventsQuery({ from: "yesterday" }));
  assert.throws(() => auditEventsQuery({ page_size: 0 }));
  assert.throws(() => auditEventsQuery({ page_size: 501 }));
});

test("tokenRole mints only support or auditor", () => {
  assert.equal(tokenRole("support"), "support");
  assert.equal(tokenRole("auditor"), "auditor");
  assert.equal(tokenRole("admin"), undefined);
  assert.equal(tokenRole(undefined), undefined);
});

test("gatewayTarget forwards the query string but never another origin", () => {
  assert.equal(gatewayTarget("http://gw:8080", ["v1", "audit", "events"], "?account_id=acc-1&actor=x"),
    "http://gw:8080/v1/audit/events?account_id=acc-1&actor=x");
  assert.equal(gatewayTarget("http://gw:8080", ["v1", "accounts", "a/b"], ""), "http://gw:8080/v1/accounts/a%2Fb");
  assert.equal(gatewayTarget("http://gw:8080", ["v1", "x"], "?q=1#frag"), "http://gw:8080/v1/x?q=1");
});
