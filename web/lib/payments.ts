// Builds the only gateway paths the simulator's bank-payments proxy may call.
export type PaymentWebhookOp = "settle" | "fail" | "return";

const SAFE_ID = /^[A-Za-z0-9-]{1,64}$/;

export function paymentWebhookPath(op: PaymentWebhookOp, paymentId?: string): string {
  if (op === "settle" || op === "fail") {
    if (paymentId !== undefined) throw new Error(`${op} takes the payment id in the body`);
    return "/v1/webhooks/bank/payments";
  }
  if (op !== "return") throw new Error(`unknown payment op: ${op}`);
  if (!paymentId || !SAFE_ID.test(paymentId)) throw new Error("invalid payment id");
  return `/v1/webhooks/bank/payments/${paymentId}/return`;
}
