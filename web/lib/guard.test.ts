import { test } from "node:test";
import assert from "node:assert/strict";
import { simulatorGuard } from "./guard.ts";

test("simulator routes are disabled unless SIMULATOR_ENABLED=1", async () => {
  for (const v of [undefined, "", "0", "true", "yes"]) {
    const res = simulatorGuard({ SIMULATOR_ENABLED: v });
    assert.ok(res, `value ${v} must block`);
    assert.equal(res!.status, 404);
  }
  assert.equal(simulatorGuard({ SIMULATOR_ENABLED: "1" }), null);
});
