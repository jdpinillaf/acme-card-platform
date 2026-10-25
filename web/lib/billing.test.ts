import { test } from "node:test";
import assert from "node:assert/strict";
import { daysToNextClose, daysToDuePlus, formatMinor, supportRole } from "./billing.ts";

const summary = { account_time: "2027-02-05T10:00:00Z", cycle_end: "2027-02-10T00:00:00Z", due_date: "2027-03-03T00:00:00Z" };

test("daysToNextClose rounds up to reach the close", () => {
  assert.equal(daysToNextClose(summary), 5);
  assert.equal(daysToNextClose({ ...summary, account_time: "2027-02-10T00:00:00Z" }), 1);
});

test("daysToDuePlus lands N days after the due date", () => {
  // due 2027-03-03, +1 day = 2027-03-04 00:00: 26 days and 14h from 2027-02-05 10:00 → 27
  assert.equal(daysToDuePlus(summary, 1), 27);
  assert.equal(daysToDuePlus(summary, 15), 41);
  assert.equal(daysToDuePlus({ ...summary, due_date: undefined }, 1), null);
  assert.equal(daysToDuePlus({ ...summary, account_time: "2027-04-01T00:00:00Z" }, 1), 1);
});

test("daysTo* stay within the 1-400 advance bounds", () => {
  assert.equal(daysToNextClose({ ...summary, cycle_end: "2030-01-01T00:00:00Z" }), 400);
});

test("formatMinor prints exact minor units", () => {
  assert.equal(formatMinor(2500, "USD"), "25.00 USD");
  assert.equal(formatMinor(-5, "USD"), "-0.05 USD");
  assert.equal(formatMinor(10000000, "COP"), "100000.00 COP");
});

test("supportRole only accepts the support role", () => {
  assert.equal(supportRole("support"), "support");
  assert.equal(supportRole("admin"), undefined);
  assert.equal(supportRole(undefined), undefined);
});
