// Builds the only gateway requests the simulator's bank-payouts proxy may make (Sprint 5).
export type PayoutBankOp = "instruction" | "settle" | "fail";

const SAFE_ID = /^[A-Za-z0-9-]{1,64}$/;

export function payoutBankRequest(op: PayoutBankOp, attemptId: string): { method: "GET" | "POST"; path: string } {
  if (!SAFE_ID.test(attemptId ?? "")) throw new Error("invalid attempt id");
  if (op === "instruction") return { method: "GET", path: `/v1/bank/payout-instructions/${attemptId}` };
  if (op === "settle" || op === "fail") return { method: "POST", path: "/v1/webhooks/bank/payouts" };
  throw new Error(`unknown payout op: ${op}`);
}

/** Destination account numbers are shown masked everywhere in the simulator. */
export function maskAccount(n: string): string {
  return `****${n.slice(-4)}`;
}
