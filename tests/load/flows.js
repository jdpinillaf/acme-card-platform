// k6 load test — Sprints 1 and 2. Simulates up to 1000 concurrent customers through the load balancer:
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
const NETWORK_SECRET = __ENV.NETWORK_WEBHOOK_SECRET;

const activated = new Counter("cards_activated");
const replays = new Counter("idempotent_replays");
const wrongState = new Counter("wrong_final_state");
const wrongBalance = new Counter("wrong_available_after_transactions");

// STRESS=1 removes think time (closed-loop saturation); latency is then reported, not gated.
const STRESS = __ENV.STRESS === "1";
const think = () => { if (!STRESS) sleep(1 + Math.random() * 2); };

export const options = {
  scenarios: {
    customers: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: __ENV.SHORT === "1"
        ? [{ duration: "10s", target: 1000 }, { duration: "30s", target: 1000 }, { duration: "5s", target: 0 }]
        : [
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
    wrong_available_after_transactions: ["count==0"],
    checks: ["rate>0.99"],
    // Per-endpoint breakdown (reporting only).
    "http_req_duration{name:/v1/accounts}": ["p(95)<100000"],
    "http_req_duration{name:/v1/accounts/:id/collateral/deposits}": ["p(95)<100000"],
    "http_req_duration{name:/v1/webhooks/bank/deposits}": ["p(95)<100000"],
    "http_req_duration{name:/v1/accounts/:id}": ["p(95)<100000"],
    "http_req_duration{name:/v1/network/authorizations}": ["p(95)<100000"],
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
  else { wrongState.add(1); return; }
  think();

  // Sprint 2: authorize twice with the same network_ref (one hold), capture 80%, refund half,
  // and reverse a second authorization. Final available = deposit - captured + refunded.
  const net = (path, body, name) => {
    const payload = JSON.stringify(body);
    const ts = String(Math.floor(Date.now() / 1000));
    const sig = crypto.hmac("sha256", NETWORK_SECRET, `${ts}.POST ${path}.${payload}`, "hex");
    return { method: "POST", url: `${BASE}${path}`, body: payload,
      params: { headers: { "Content-Type": "application/json", "X-Acme-Timestamp": ts, "X-Acme-Signature": sig }, tags: { name } } };
  };
  const merchant = { id: "m-load", name: "Load Store", mcc: "5411" };
  const purchase = Math.floor(amount / 4);
  const ref = `n-${acc}`;
  const auths = http.batch([
    net("/v1/network/authorizations", { network_ref: ref, card_id: acc, amount_minor: purchase, currency: "USD", merchant }, "/v1/network/authorizations"),
    net("/v1/network/authorizations", { network_ref: ref, card_id: acc, amount_minor: purchase, currency: "USD", merchant }, "/v1/network/authorizations"),
  ]);
  if (!check(auths, {
    "auth 201": (rs) => rs.every((r) => r.status === 201),
    "auth approved": (rs) => rs[0].json("status") === "approved",
    "same auth id": (rs) => rs[0].json("id") === rs[1].json("id"),
  })) return;
  replays.add(auths.filter((r) => r.headers["Idempotency-Replayed"] === "true").length);
  const authId = auths[0].json("id");
  const captured = Math.floor(purchase * 0.8);
  const refunded = Math.floor(captured / 2);
  const cap = http.request(...Object.values(net(`/v1/network/authorizations/${authId}/capture`, { network_ref: `c-${authId}`, amount_minor: captured }, "/v1/network/authorizations/:id/capture")));
  check(cap, { "captured": (r) => r.status === 201 && r.json("status") === "captured" });
  const rf = http.request(...Object.values(net(`/v1/network/authorizations/${authId}/refunds`, { network_ref: `r-${authId}`, amount_minor: refunded }, "/v1/network/authorizations/:id/refunds")));
  check(rf, { "refunded": (r) => r.status === 201 });
  const second = http.request(...Object.values(net("/v1/network/authorizations", { network_ref: `${ref}-2`, card_id: acc, amount_minor: 100, currency: "USD", merchant }, "/v1/network/authorizations")));
  if (second.status === 201 && second.json("status") === "approved") {
    const rev = http.request(...Object.values(net(`/v1/network/authorizations/${second.json("id")}/reversal`, { network_ref: `v-${second.json("id")}` }, "/v1/network/authorizations/:id/reversal")));
    check(rev, { "reversed": (r) => r.status === 200 && r.json("status") === "reversed" });
  }
  const after = http.get(`${BASE}/v1/accounts/${acc}`, { headers: { Authorization: `Bearer ${tok}` }, tags: { name: "/v1/accounts/:id" } });
  if (!check(after, { "available = deposit - captured + refunded": (r) => r.json("available_credit_minor") === amount - captured + refunded })) {
    wrongBalance.add(1);
  }
}
