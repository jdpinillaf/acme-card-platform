// Simulated card network: signs messages with NETWORK_WEBHOOK_SECRET server-side and sends them
// `repeat` times concurrently. Body: { op, authId?, payload, repeat?, tamper? }.
import { env } from "@/lib/env.ts";
import { signRequest } from "@/lib/crypto.ts";
import { forward } from "@/lib/forward.ts";
import { simulatorGuard } from "@/lib/guard.ts";
import { networkPath, type NetworkOp } from "@/lib/network.ts";

export async function POST(request: Request) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const { op, authId, payload, repeat, tamper } = (await request.json()) as {
    op: NetworkOp; authId?: string; payload: Record<string, unknown>; repeat?: number; tamper?: boolean;
  };
  let path: string;
  try {
    path = networkPath(op, authId);
  } catch (e) {
    return Response.json({ error: { code: "invalid_request", message: String(e) } }, { status: 400 });
  }
  const body = JSON.stringify(payload);
  const n = Math.min(Math.max(Number(repeat ?? 1), 1), 100);
  const results = await Promise.all(
    Array.from({ length: n }, () => {
      const { timestamp, signature } = signRequest(tamper ? "wrong-secret" : env.networkSecret, "POST", path, body);
      return forward(`${env.gatewayUrl}${path}`, {
        method: "POST",
        headers: { "content-type": "application/json", "x-acme-timestamp": timestamp, "x-acme-signature": signature },
        body,
      });
    }),
  );
  return Response.json(results);
}
