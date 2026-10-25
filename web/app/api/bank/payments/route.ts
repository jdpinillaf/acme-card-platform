// Simulated bank for customer payments: settle | fail a payment intent, or return a settled one.
// Signs server-side with BANK_WEBHOOK_SECRET bound to method + path. Body:
// { op, payment: {id, amount_minor, currency}, ref, repeat?, tamper? }.
import { env } from "@/lib/env.ts";
import { signRequest } from "@/lib/crypto.ts";
import { forward } from "@/lib/forward.ts";
import { simulatorGuard } from "@/lib/guard.ts";
import { paymentWebhookPath, type PaymentWebhookOp } from "@/lib/payments.ts";

type Body = {
  op: PaymentWebhookOp;
  payment: { id: string; amount_minor: number; currency: string };
  ref: string;
  repeat?: number;
  tamper?: boolean;
};

export async function POST(request: Request) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const b = (await request.json()) as Body;
  let path: string;
  try {
    path = paymentWebhookPath(b.op, b.op === "return" ? b.payment.id : undefined);
  } catch (e) {
    return Response.json({ error: { code: "invalid_request", message: String(e) } }, { status: 400 });
  }
  const payload =
    b.op === "return"
      ? JSON.stringify({ return_ref: b.ref, amount_minor: Number(b.payment.amount_minor) })
      : JSON.stringify({
          payment_id: b.payment.id, external_ref: b.ref, amount_minor: Number(b.payment.amount_minor),
          currency: b.payment.currency, status: b.op === "settle" ? "settled" : "failed",
        });
  const n = Math.min(Math.max(Number(b.repeat ?? 1), 1), 100);
  const results = await Promise.all(
    Array.from({ length: n }, () => {
      const { timestamp, signature } = signRequest(b.tamper ? "wrong-secret" : env.bankSecret, "POST", path, payload);
      return forward(`${env.gatewayUrl}${path}`, {
        method: "POST",
        headers: { "content-type": "application/json", "x-acme-timestamp": timestamp, "x-acme-signature": signature },
        body: payload,
      });
    }),
  );
  return Response.json(results);
}
