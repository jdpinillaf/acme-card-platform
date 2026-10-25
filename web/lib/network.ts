// Builds the only gateway paths the simulator's network proxy may call.
export type NetworkOp = "authorize" | "capture" | "reversal" | "refunds";

const SAFE_ID = /^[A-Za-z0-9-]{1,64}$/;

export function networkPath(op: NetworkOp, authId?: string): string {
  if (op === "authorize") {
    if (authId !== undefined) throw new Error("authorize takes no authorization id");
    return "/v1/network/authorizations";
  }
  if (op !== "capture" && op !== "reversal" && op !== "refunds") throw new Error(`unknown network op: ${op}`);
  if (!authId || !SAFE_ID.test(authId)) throw new Error("invalid authorization id");
  return `/v1/network/authorizations/${authId}/${op}`;
}
