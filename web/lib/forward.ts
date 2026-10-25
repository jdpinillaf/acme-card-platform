// Result envelope every simulator API returns, so the UI can log status, replay flag and timing.
export type CallResult = {
  status: number;
  replayed: boolean;
  ms: number;
  body: unknown;
};

export async function forward(url: string, init: RequestInit): Promise<CallResult> {
  const start = performance.now();
  try {
    const res = await fetch(url, { ...init, cache: "no-store" });
    const text = await res.text();
    let body: unknown = text;
    try {
      body = JSON.parse(text);
    } catch {}
    return { status: res.status, replayed: res.headers.get("idempotency-replayed") === "true", ms: Math.round(performance.now() - start), body };
  } catch (e) {
    return { status: 0, replayed: false, ms: Math.round(performance.now() - start), body: { error: { code: "network_error", message: String(e) } } };
  }
}
