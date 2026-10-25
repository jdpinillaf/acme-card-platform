// Audit panel helpers (Sprint 6): query strings for the auditor API and the token roles the
// simulator may mint.
export type AuditFilter = { account_id?: string; event_type?: string; actor?: string; from?: string; to?: string; page_size?: number };

const ORDER = ["account_id", "event_type", "actor", "from", "to", "page_size"] as const;

/** Builds `?…` for GET /v1/audit/events; dates (any parseable input) become RFC 3339 UTC. */
export function auditEventsQuery(f: AuditFilter): string {
  const q = new URLSearchParams();
  for (const k of ORDER) {
    const v = f[k];
    if (v === undefined || v === "") continue;
    if (k === "page_size") {
      const n = Number(v);
      if (!Number.isInteger(n) || n < 1 || n > 500) throw new Error("page_size must be 1..500");
      q.set(k, String(n));
    } else if (k === "from" || k === "to") {
      const d = new Date(String(v));
      if (Number.isNaN(d.getTime())) throw new Error(`invalid ${k} date`);
      q.set(k, d.toISOString());
    } else {
      const s = String(v).trim();
      if (s) q.set(k, s);
    }
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

/** The token route mints only these roles; anything else is a plain customer token. */
export function tokenRole(role: unknown): "support" | "auditor" | undefined {
  return role === "support" || role === "auditor" ? role : undefined;
}

/** Gateway URL for the proxy: escaped path segments plus the caller's query string (no fragment). */
export function gatewayTarget(base: string, path: string[], search: string): string {
  const q = search.split("#")[0];
  return `${base}/${path.map(encodeURIComponent).join("/")}${q.startsWith("?") && q.length > 1 ? q : ""}`;
}
