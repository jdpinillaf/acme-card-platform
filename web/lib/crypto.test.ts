import { test } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { signWebhook, signJwt } from "./crypto.ts";

test("webhook signature matches the Go gateway (pkg/webhook) vector", () => {
  const { timestamp, signature } = signWebhook("bank-dev-secret", '{"deposit_id":"d1","amount_minor":500}', 1700000000);
  assert.equal(timestamp, "1700000000");
  assert.equal(signature, "1580e226fa3dcafac4d0ea5fa4cacca0389c401db07fea6e6bae01be4ce14a3a");
});

test("JWT is HS256 with sub, authorized_accounts and exp", () => {
  const secret = "0123456789abcdef0123456789abcdef";
  const token = signJwt(secret, "cust-1", ["acc-1"], 60, 1700000000);
  const [h, p, s] = token.split(".");
  assert.deepEqual(JSON.parse(Buffer.from(h, "base64url").toString()), { alg: "HS256", typ: "JWT" });
  const payload = JSON.parse(Buffer.from(p, "base64url").toString());
  assert.equal(payload.sub, "cust-1");
  assert.deepEqual(payload.authorized_accounts, ["acc-1"]);
  assert.equal(payload.iat, 1700000000);
  assert.equal(payload.exp, 1700000060);
  const expected = createHmac("sha256", secret).update(`${h}.${p}`).digest("base64url");
  assert.equal(s, expected);
});

test("JWT rejects short secrets", () => {
  assert.throws(() => signJwt("short", "s", [], 60));
});
