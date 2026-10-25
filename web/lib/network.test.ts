import { test } from "node:test";
import assert from "node:assert/strict";
import { networkPath } from "./network.ts";

test("networkPath builds only the four allowed network routes", () => {
  assert.equal(networkPath("authorize"), "/v1/network/authorizations");
  assert.equal(networkPath("capture", "abc-123"), "/v1/network/authorizations/abc-123/capture");
  assert.equal(networkPath("reversal", "abc"), "/v1/network/authorizations/abc/reversal");
  assert.equal(networkPath("refunds", "abc"), "/v1/network/authorizations/abc/refunds");
});

test("networkPath rejects unknown ops and unsafe ids", () => {
  assert.throws(() => networkPath("evil" as never));
  assert.throws(() => networkPath("capture"));
  assert.throws(() => networkPath("capture", "../accounts"));
  assert.throws(() => networkPath("capture", "a/b"));
  assert.throws(() => networkPath("authorize", "abc"));
});
