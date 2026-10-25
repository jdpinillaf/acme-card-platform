// Simulated bank: signs a settlement webhook server-side (secret never leaves the server)
// and sends it `repeat` times concurrently. `tamper` corrupts the signature, `stale` backdates it.
import { env } from "@/lib/env.ts";
import { signWebhook } from "@/lib/crypto.ts";
import { forward } from "@/lib/forward.ts";

type Body = {
  deposit_id: string;
  external_ref: string;
  amount_minor: number;
  currency: string;
  status: "settled" | "failed";
  repeat?: number;
  tamper?: boolean;
  stale?: boolean;
};

export async function POST(request: Request) {
  const b = (await request.json()) as Body;
  const payload = JSON.stringify({
    deposit_id: b.deposit_id,
    external_ref: b.external_ref,
    amount_minor: Number(b.amount_minor),
    currency: b.currency,
    status: b.status,
  });
  const now = Math.floor(Date.now() / 1000) - (b.stale ? 600 : 0);
  const repeat = Math.min(Math.max(Number(b.repeat ?? 1), 1), 100);
  const results = await Promise.all(
    Array.from({ length: repeat }, () => {
      const { timestamp, signature } = signWebhook(b.tamper ? "wrong-secret" : env.bankSecret, payload, now);
      return forward(`${env.gatewayUrl}/v1/webhooks/bank/deposits`, {
        method: "POST",
        headers: { "content-type": "application/json", "x-acme-timestamp": timestamp, "x-acme-signature": signature },
        body: payload,
      });
    }),
  );
  return Response.json(results);
}
