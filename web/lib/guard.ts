// The simulator can mint arbitrary JWTs and sign bank webhooks. It must be switched on explicitly
// (SIMULATOR_ENABLED=1, dev only); otherwise every simulator API answers 404.
export function simulatorGuard(env: Record<string, string | undefined> = process.env): Response | null {
  if (env.SIMULATOR_ENABLED === "1") return null;
  return Response.json({ error: { code: "not_found", message: "simulator disabled" } }, { status: 404 });
}
