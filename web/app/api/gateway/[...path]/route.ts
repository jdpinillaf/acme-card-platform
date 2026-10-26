// Same-origin proxy to the gateway. Forwards only Authorization, Idempotency-Key, the query string and the body.
// `x-sim-repeat: N` fires the same request N times concurrently (idempotency demos).
import { env } from "@/lib/env.ts";
import { simulatorGuard } from "@/lib/guard.ts";
import { forward } from "@/lib/forward.ts";
import { gatewayTarget } from "@/lib/audit.ts";

type Ctx = { params: Promise<{ path: string[] }> };

async function handle(request: Request, ctx: Ctx) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const { path } = await ctx.params;
  const url = gatewayTarget(env.gatewayUrl, path, new URL(request.url).search);
  const headers: Record<string, string> = { "content-type": "application/json" };
  for (const h of ["authorization", "idempotency-key"]) {
    const v = request.headers.get(h);
    if (v) headers[h] = v;
  }
  const body = request.method === "GET" ? undefined : await request.text();
  const repeat = Math.min(Math.max(Number(request.headers.get("x-sim-repeat") ?? 1), 1), 100);
  const results = await Promise.all(Array.from({ length: repeat }, () => forward(url, { method: request.method, headers, body })));
  return Response.json(results);
}

export const GET = handle;
export const POST = handle;
export const PUT = handle;
export const DELETE = handle;
