// Dev only: the page hands the demo partner its secret right after registering it (support API).
import { simulatorGuard } from "@/lib/guard.ts";
import { partnerStore, validPartnerKey } from "@/lib/partner.ts";

export async function POST(request: Request) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const { key, secret } = (await request.json()) as { key?: string; secret?: string };
  if (!key || !validPartnerKey(key) || !secret || !/^[0-9a-f]{64}$/.test(secret)) {
    return Response.json({ error: { code: "invalid_body", message: "key and a 64-hex secret are required" } }, { status: 400 });
  }
  partnerStore.secrets.set(key, secret);
  return Response.json({ ok: true });
}
