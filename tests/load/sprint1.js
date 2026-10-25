// k6 load test — Sprint 1. Simulates up to 1000 concurrent customers through the load balancer:
// open account → create deposit (sent twice with the same Idempotency-Key) → bank webhook
// (sent twice, concurrently) → read account. Asserts idempotent replays and final state.
import http from "k6/http";
import crypto from "k6/crypto";
import encoding from "k6/encoding";
import { check, sleep } from "k6";
import { Counter } from "k6/metrics";

const BASE = __ENV.BASE_URL || "http://localhost:8088";
const JWT_SECRET = __ENV.JWT_HS256_SECRET;
const BANK_SECRET = __ENV.BANK_WEBHOOK_SECRET;

const activated = new Counter("cards_activated");
const replays = new Counter("idempotent_replays");
const wrongState = new Counter("wrong_final_state");

// STRESS=1 removes think time (closed-loop saturation); latency is then reported, not gated.
const STRESS = __ENV.STRESS === "1";
const think = () => { if (!STRESS) sleep(1 + Math.random() * 2); };

export const options = {
  scenarios: {
    customers: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: "20s", target: 250 },
        { duration: "20s", target: 1000 },
        { duration: "60s", target: 1000 },
        { duration: "10s", target: 0 },
      ],
      gracefulRampDown: "10s",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: STRESS ? ["p(95)<10000"] : ["p(95)<1500"],
    wrong_final_state: ["count==0"],
    checks: ["rate>0.99"],
  },
};

const b64 = (o) => encoding.b64encode(JSON.stringify(o), "rawurl");

function jwt(sub, accounts) {
  const now = Math.floor(Date.now() / 1000);
  const head = b64({ alg: "HS256", typ: "JWT" });
  const body = b64({ iss: "acme-card", sub, iat: now, exp: now + 3600, authorized_accounts: accounts });
  const sig = crypto.hmac("sha256", JWT_SECRET, `${head}.${body}`, "base64rawurl");
  return `${head}.${body}.${sig}`;
}

const uuid = () => crypto.randomBytes(16) && `${Date.now().toString(16)}-${__VU}-${__ITER}-${Math.random().toString(16).slice(2, 10)}`;

function post(path, body, token, key) {
  const headers = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (key) headers["Idempotency-Key"] = key;
  return { method: "POST", url: `${BASE}${path}`, body: JSON.stringify(body), params: { headers, tags: { name: path.replace(/[0-9a-f-]{36}/g, ":id") } } };
}

export default function () {
  const sub = `load-${__VU}-${__ITER}-${Math.random().toString(36).slice(2, 8)}`;
  const amount = 10000 + Math.floor(Math.random() * 90000);

  const open = http.post(`${BASE}/v1/accounts`, JSON.stringify({ currency: "USD" }), {
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${jwt(sub, [])}`, "Idempotency-Key": uuid() },
    tags: { name: "/v1/accounts" },
  });
  if (!check(open, { "account opened": (r) => r.status === 201 })) return;
  const acc = open.json("id");
  think();
  const tok = jwt(sub, [acc]);

  // Same deposit request twice, concurrently, same key → one deposit, one replay.
  const key = uuid();
  const deps = http.batch([
    post(`/v1/accounts/${acc}/collateral/deposits`, { amount_minor: amount }, tok, key),
    post(`/v1/accounts/${acc}/collateral/deposits`, { amount_minor: amount }, tok, key),
  ]);
  if (!check(deps, {
    "deposits 201": (rs) => rs.every((r) => r.status === 201),
    "same deposit id": (rs) => rs[0].json("id") === rs[1].json("id"),
  })) return;
  replays.add(deps.filter((r) => r.headers["Idempotency-Replayed"] === "true").length);
  const dep = deps[0].json("id");
  think();

  // Bank settles twice, concurrently → applied once.
  const payload = JSON.stringify({ deposit_id: dep, external_ref: `bank-${dep}`, amount_minor: amount, currency: "USD", status: "settled" });
  const hooks = [0, 1].map(() => {
    const ts = String(Math.floor(Date.now() / 1000));
    const sig = crypto.hmac("sha256", BANK_SECRET, `${ts}.${payload}`, "hex");
    return { method: "POST", url: `${BASE}/v1/webhooks/bank/deposits`, body: payload, params: { headers: { "Content-Type": "application/json", "X-Acme-Timestamp": ts, "X-Acme-Signature": sig }, tags: { name: "/v1/webhooks/bank/deposits" } } };
  });
  const settled = http.batch(hooks);
  check(settled, { "webhooks 200": (rs) => rs.every((r) => r.status === 200) });
  think();

  const get = http.get(`${BASE}/v1/accounts/${acc}`, { headers: { Authorization: `Bearer ${tok}` }, tags: { name: "/v1/accounts/:id" } });
  const ok = check(get, {
    "active": (r) => r.json("state") === "active",
    "limit = deposit": (r) => r.json("credit_limit_minor") === amount,
    "available = deposit": (r) => r.json("available_credit_minor") === amount,
  });
  if (ok) activated.add(1);
  else wrongState.add(1);
}
