import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

// Every gateway route maps to a UI action (enforced on the Go side against the real router);
// here we check every one of those actions is an actual control on the simulator page.
test("every action in ui-actions.json is a control on the page", () => {
  const actions = JSON.parse(readFileSync(new URL("./ui-actions.json", import.meta.url), "utf8")) as { action: string }[];
  const page = readFileSync(new URL("../app/page.tsx", import.meta.url), "utf8");
  const missing = actions.filter((a) => !page.includes(`data-action="${a.action}"`)).map((a) => a.action);
  assert.deepEqual(missing, []);
});
