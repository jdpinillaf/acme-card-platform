import { test } from "node:test";
import assert from "node:assert/strict";
import { paymentWebhookPath } from "./payments.ts";

test("paymentWebhookPath builds only the bank payment routes", () => {
  assert.equal(paymentWebhookPath("settle"), "/v1/webhooks/bank/payments");
  assert.equal(paymentWebhookPath("fail"), "/v1/webhooks/bank/payments");
  assert.equal(paymentWebhookPath("return", "abc-123"), "/v1/webhooks/bank/payments/abc-123/return");
});

test("paymentWebhookPath rejects unknown ops and unsafe ids", () => {
  assert.throws(() => paymentWebhookPath("evil" as never));
  assert.throws(() => paymentWebhookPath("return"));
  assert.throws(() => paymentWebhookPath("return", "../x"));
  assert.throws(() => paymentWebhookPath("settle", "abc"));
});
