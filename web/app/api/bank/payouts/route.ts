// Simulated bank for payouts (Sprint 5): fetch the payout instruction (the only place the full
// destination number is returned), then settle or fail the attempt. Signs server-side with
// BANK_WEBHOOK_SECRET bound to method + path. Body: { op, attempt_id, amount_minor?, currency?, last4?, repeat?, tamper? }.
import { env } from "@/lib/env.ts";
import { signRequest } from "@/lib/crypto.ts";
import { forward } from "@/lib/forward.ts";
import { simulatorGuard } from "@/lib/guard.ts";
import { payoutBankRequest, type PayoutBankOp } from "@/lib/payouts.ts";

type Body = { op: PayoutBankOp; attempt_id: string; amount_minor?: number; currency?: string; last4?: string; repeat?: number; tamper?: boolean };

export async function POST(request: Request) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  let b: Body;
  try {
    b = (await request.json()) as Body;
  } catch {
    return Response.json({ error: { code: "invalid_body", message: "body must be JSON" } }, { status: 400 });
  }
  let req: { method: "GET" | "POST"; path: string };
  try {
    req = payoutBankRequest(b.op, b.attempt_id);
  } catch (e) {
    return Response.json({ error: { code: "invalid_request", message: String(e) } }, { status: 400 });
  }
  const payload =
    req.method === "GET"
      ? ""
      : JSON.stringify({ attempt_id: b.attempt_id, amount_minor: Number(b.amount_minor), currency: b.currency,
          destination_last4: b.last4, status: b.op === "settle" ? "settled" : "failed" });
  const n = Math.min(Math.max(Number(b.repeat ?? 1), 1), 100);
  const results = await Promise.all(
    Array.from({ length: n }, () => {
      const { timestamp, signature } = signRequest(b.tamper ? "wrong-secret" : env.bankSecret, req.method, req.path, payload);
      return forward(`${env.gatewayUrl}${req.path}`, {
        method: req.method,
        headers: { "content-type": "application/json", "x-acme-timestamp": timestamp, "x-acme-signature": signature },
        body: req.method === "GET" ? undefined : payload,
      });
    }),
  );
  return Response.json(results);
}
