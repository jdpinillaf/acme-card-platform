import { test } from "node:test";
import assert from "node:assert/strict";
import { declineText, errorText, money, parseMoney, parseSession, txActions, MERCHANTS } from "./portal.ts";

test("parseMoney reads dollars with dot or comma decimals into minor units", () => {
  assert.equal(parseMoney("120"), 12000);
  assert.equal(parseMoney("120.5"), 12050);
  assert.equal(parseMoney("120,05"), 12005);
  assert.equal(parseMoney(" 1.234,56 "), 123456);
  assert.equal(parseMoney("0.01"), 1);
});

test("parseMoney rejects empty, negative, zero and over-precise amounts", () => {
  for (const s of ["", "abc", "-5", "0", "1.234", "1e3"]) assert.equal(parseMoney(s), null, s);
});

test("money formats minor units exactly", () => {
  assert.equal(money(12050, "USD"), "$120.50");
  assert.equal(money(-2500, "USD"), "-$25.00");
  assert.equal(money(123456789, "USD"), "$1,234,567.89");
  assert.equal(money(500, "EUR"), "5.00 EUR");
});

test("declineText explains every gateway decline reason in Spanish", () => {
  for (const r of ["account_closed", "account_closing", "charged_off", "collateral_not_confirmed", "currency_not_supported",
    "payment_returned", "over_limit", "past_due", "card_frozen", "velocity_limit_exceeded", "purchase_limit_exceeded", "insufficient_credit"])
    assert.notEqual(declineText(r), r, r);
  assert.equal(declineText("something_new"), "something_new");
});

test("errorText prefers a known code, then the gateway message, then the status", () => {
  assert.match(errorText({ status: 422, body: { error: { code: "payment_exceeds_balance", message: "x" } } }), /deuda/);
  assert.equal(errorText({ status: 409, body: { error: { code: "weird", message: "raw message" } } }), "raw message");
  assert.equal(errorText({ status: 0, body: "boom" }), "No se pudo contactar el servidor (0)");
  assert.equal(errorText({ status: 503, body: "" }), "Error 503");
});

test("txActions follows the authorization lifecycle", () => {
  const base = { status: "approved", amount_minor: 1000, captured_amount_minor: 0, refunded_amount_minor: 0 };
  assert.deepEqual(txActions(base), { capture: true, reverse: true, refundable: 0 });
  assert.deepEqual(txActions({ ...base, status: "captured", captured_amount_minor: 800, refunded_amount_minor: 300 }), { capture: false, reverse: false, refundable: 500 });
  assert.deepEqual(txActions({ ...base, status: "declined" }), { capture: false, reverse: false, refundable: 0 });
});

test("parseSession accepts only well-formed saved sessions", () => {
  const ok = { sub: "cust-abc", name: "Ana", accountId: "0b8f-1", deposits: [] };
  assert.deepEqual(parseSession(JSON.stringify(ok)), ok);
  assert.equal(parseSession(null), null);
  assert.equal(parseSession("{bad"), null);
  assert.equal(parseSession(JSON.stringify({ name: "x" })), null);
  assert.deepEqual(parseSession(JSON.stringify({ sub: "cust-1" })), { sub: "cust-1", name: "", accountId: "", deposits: [] });
});

test("merchant catalog has unique ids and 4-digit MCCs", () => {
  assert.equal(new Set(MERCHANTS.map((m) => m.id)).size, MERCHANTS.length);
  for (const m of MERCHANTS) assert.match(m.mcc, /^\d{4}$/);
});
