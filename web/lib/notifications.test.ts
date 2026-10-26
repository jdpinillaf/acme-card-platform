import { test } from "node:test";
import assert from "node:assert/strict";
import { channelIcon, newItems } from "./notifications.ts";
import { recordPartnerEvent, partnerStore, validPartnerKey, verifyPartnerSignature } from "./partner.ts";

test("newItems returns only unseen items, oldest first", () => {
  const prev = [{ id: 1 }, { id: 2 }];
  assert.deepEqual(newItems(prev, [{ id: 4 }, { id: 3 }, { id: 2 }]).map((i) => i.id), [3, 4]);
  assert.deepEqual(newItems([], []), []);
});

test("channelIcon covers the three channels", () => {
  assert.notEqual(channelIcon("push"), channelIcon("sms"));
  assert.notEqual(channelIcon("sms"), channelIcon("email"));
  assert.ok(channelIcon("other").length > 0);
});

// same vector as services/notifications/internal/notify/webhook_test.go (TestSignatureVector)
const SIG = "v1=56654726859e10150ca6bc5f7720da706cb8b5b10ece6873bf998c470dcfd15c";

test("verifyPartnerSignature checks the Go vector", () => {
  assert.equal(verifyPartnerSignature("partner-secret", '{"id":"e1"}', "1700000000", SIG, 1700000010), true);
});

test("verifyPartnerSignature rejects tampering and stale requests", () => {
  assert.equal(verifyPartnerSignature("partner-secret", '{"id":"e2"}', "1700000000", SIG, 1700000010), false);
  assert.equal(verifyPartnerSignature("wrong", '{"id":"e1"}', "1700000000", SIG, 1700000010), false);
  assert.equal(verifyPartnerSignature("partner-secret", '{"id":"e1"}', "1700000000", SIG, 1700000000 + 301), false);
  assert.equal(verifyPartnerSignature("partner-secret", '{"id":"e1"}', "1700000000", "v1=zz", 1700000010), false);
  assert.equal(verifyPartnerSignature("partner-secret", '{"id":"e1"}', "", SIG, 1700000010), false);
});

test("partner store keeps the latest 20 events per key and validates keys", () => {
  for (let i = 0; i < 25; i++) recordPartnerEvent("key-abcdef01", { event_id: `e${i}`, event_type: "x", delivery_id: "d", verified: true, received_at: "" });
  const list = partnerStore.events.get("key-abcdef01") ?? [];
  assert.equal(list.length, 20);
  assert.equal(list[0].event_id, "e24");
  assert.equal(validPartnerKey("../etc"), false);
  assert.equal(validPartnerKey("0192f000-0000-7000-8000-000000000001"), true);
});
