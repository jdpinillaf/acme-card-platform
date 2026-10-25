// Dev-only token issuer: mints an HS256 JWT for the simulator (mirrors cmd/devtoken).
import { env } from "@/lib/env.ts";
import { signJwt } from "@/lib/crypto.ts";

export async function POST(request: Request) {
  const { sub, accounts } = (await request.json()) as { sub?: string; accounts?: string[] };
  if (!sub) return Response.json({ error: { code: "invalid_body", message: "sub is required" } }, { status: 400 });
  try {
    const token = signJwt(env.jwtSecret, sub, (accounts ?? []).filter(Boolean), 24 * 3600);
    return Response.json({ token });
  } catch (e) {
    return Response.json({ error: { code: "misconfigured", message: String(e) } }, { status: 500 });
  }
}
