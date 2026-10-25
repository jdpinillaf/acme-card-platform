// Server-only signing helpers. Must stay byte-compatible with pkg/webhook and pkg/auth (Go).
import { createHmac } from "node:crypto";

/** HMAC-SHA256 over `${timestamp}.${body}`, hex. Mirrors pkg/webhook.Sign. */
export function signWebhook(secret: string, body: string, nowSec = Math.floor(Date.now() / 1000)) {
  const timestamp = String(nowSec);
  const signature = createHmac("sha256", secret).update(`${timestamp}.${body}`).digest("hex");
  return { timestamp, signature };
}

const b64url = (v: object) => Buffer.from(JSON.stringify(v)).toString("base64url");

/** HS256 JWT with authorized_accounts. Mirrors pkg/auth.Sign. Dev-only issuer. */
export function signJwt(secret: string, sub: string, accounts: string[], ttlSec: number, nowSec = Math.floor(Date.now() / 1000)) {
  if (secret.length < 32) throw new Error("JWT secret must be at least 32 bytes");
  const header = b64url({ alg: "HS256", typ: "JWT" });
  const payload = b64url({ iss: "acme-card", sub, iat: nowSec, exp: nowSec + ttlSec, authorized_accounts: accounts });
  const sig = createHmac("sha256", secret).update(`${header}.${payload}`).digest("base64url");
  return `${header}.${payload}.${sig}`;
}

/** Request-bound signature: HMAC over `${timestamp}.${METHOD} ${path}.${body}`. Mirrors pkg/webhook.SignRequest. */
export function signRequest(secret: string, method: string, path: string, body: string, nowSec = Math.floor(Date.now() / 1000)) {
  return signWebhook(secret, `${method} ${path}.${body}`, nowSec);
}
