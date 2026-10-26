// The simulator's demo partner endpoint: acme-card's notifications service POSTs signed events
// here (http://web:3000/api/partner/webhook/<key>). It verifies them like a real partner must and
// lists what it received (GET) for panel 10.
import { simulatorGuard } from "@/lib/guard.ts";
import { partnerStore, recordPartnerEvent, validPartnerKey, verifyPartnerSignature } from "@/lib/partner.ts";

type Ctx = { params: Promise<{ key: string }> };

export async function POST(request: Request, ctx: Ctx) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const { key } = await ctx.params;
  const secret = validPartnerKey(key) ? partnerStore.secrets.get(key) : undefined;
  if (!secret) return new Response(null, { status: 404 });
  const body = await request.text();
  const ok = verifyPartnerSignature(secret, body, request.headers.get("x-acme-timestamp") ?? "", request.headers.get("x-acme-signature") ?? "",
    Math.floor(Date.now() / 1000));
  let eventType = "";
  try {
    eventType = (JSON.parse(body) as { type?: string }).type ?? "";
  } catch {}
  recordPartnerEvent(key, { event_id: request.headers.get("x-acme-event-id") ?? "", event_type: eventType,
    delivery_id: request.headers.get("x-acme-delivery-id") ?? "", verified: ok, received_at: new Date().toISOString() });
  return new Response(null, { status: ok ? 204 : 401 });
}

export async function GET(_request: Request, ctx: Ctx) {
  const blocked = simulatorGuard();
  if (blocked) return blocked;
  const { key } = await ctx.params;
  return Response.json({ events: validPartnerKey(key) ? partnerStore.events.get(key) ?? [] : [] });
}
