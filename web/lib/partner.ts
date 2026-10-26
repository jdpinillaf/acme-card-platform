// The simulator's demo partner (Sprint 7), server only: it verifies acme-card webhooks the way a
// real partner must (mirrors pkg/webhook: hex HMAC-SHA256 over `ts + "." + body`, sent as v1=<hex>)
// and keeps the last events it received, in memory.
import { createHmac, timingSafeEqual } from "node:crypto";

/** Verifies an acme-card partner webhook: signature over ts + "." + body, at most 5 minutes old. */
export function verifyPartnerSignature(secret: string, body: string, ts: string, sig: string, nowSec: number, toleranceSec = 300): boolean {
  const t = Number(ts);
  if (!ts || !Number.isInteger(t) || Math.abs(nowSec - t) > toleranceSec) return false;
  const got = Buffer.from(sig.replace(/^v1=/, ""), "hex");
  const want = createHmac("sha256", secret).update(`${ts}.${body}`).digest();
  return got.length === want.length && timingSafeEqual(got, want);
}

export type PartnerEvent = { event_id: string; event_type: string; delivery_id: string; verified: boolean; received_at: string };

type Store = { secrets: Map<string, string>; events: Map<string, PartnerEvent[]> };
const g = globalThis as unknown as { __acmePartner?: Store };
export const partnerStore: Store = (g.__acmePartner ??= { secrets: new Map(), events: new Map() });

const SAFE_KEY = /^[A-Za-z0-9-]{8,64}$/;

export function validPartnerKey(key: string): boolean {
  return SAFE_KEY.test(key);
}

/** Records one received event for key, keeping the latest 20. */
export function recordPartnerEvent(key: string, e: PartnerEvent): void {
  const list = [e, ...(partnerStore.events.get(key) ?? [])].slice(0, 20);
  partnerStore.events.set(key, list);
}
