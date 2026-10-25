import { test } from "node:test";
import assert from "node:assert/strict";
import { payoutBankRequest, maskAccount } from "./payouts.ts";

test("payoutBankRequest builds only the bank payout routes", () => {
  assert.deepEqual(payoutBankRequest("instruction", "abc-123"), { method: "GET", path: "/v1/bank/payout-instructions/abc-123" });
  assert.deepEqual(payoutBankRequest("settle", "abc-123"), { method: "POST", path: "/v1/webhooks/bank/payouts" });
  assert.deepEqual(payoutBankRequest("fail", "abc-123"), { method: "POST", path: "/v1/webhooks/bank/payouts" });
});

test("payoutBankRequest rejects unknown ops and unsafe ids", () => {
  assert.throws(() => payoutBankRequest("evil" as never, "abc"));
  assert.throws(() => payoutBankRequest("instruction", "../x"));
  assert.throws(() => payoutBankRequest("settle", ""));
});

test("maskAccount keeps only the last 4 digits", () => {
  assert.equal(maskAccount("000123456789"), "****6789");
  assert.equal(maskAccount("12"), "****12");
});
