"use client";

import { useCallback, useState } from "react";

type CallResult = { status: number; replayed: boolean; ms: number; body: any };
type LogEntry = { id: number; at: string; label: string; method: string; path: string; key?: string; results: CallResult[] };
type Account = {
  id: string; customer_id: string; currency: string; state: string; frozen: boolean;
  credit_limit_minor: number; available_credit_minor: number; collateral_minor: number;
};
type Deposit = { id: string; amount_minor: number; currency: string; status: string; external_ref?: string };
type Tx = {
  id: string; network_ref: string; amount_minor: number; currency: string; status: string; decline_reason?: string;
  captured_amount_minor: number; refunded_amount_minor: number; merchant: { id: string; name: string; mcc: string };
};

const newKey = () => crypto.randomUUID();
const fmt = (minor: number, cur: string) => `${(minor / 100).toLocaleString(undefined, { minimumFractionDigits: 2 })} ${cur}`;

let seq = 0;

export default function Simulator() {
  const [sub, setSub] = useState(() => `cust-${Math.random().toString(36).slice(2, 8)}`);
  const [token, setToken] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [account, setAccount] = useState<Account | null>(null);
  const [deposits, setDeposits] = useState<Deposit[]>([]);
  const [amount, setAmount] = useState(50000);
  const [repeat, setRepeat] = useState(1);
  const [tamper, setTamper] = useState(false);
  const [stale, setStale] = useState(false);
  const [busy, setBusy] = useState(false);
  const [log, setLog] = useState<LogEntry[]>([]);
  const [last, setLast] = useState<{ method: string; path: string; body?: string; key: string; label: string } | null>(null);
  const [txs, setTxs] = useState<Tx[]>([]);
  const [buy, setBuy] = useState({ amount: 12000, currency: "USD", merchantId: "m-001", merchantName: "Corner Bookstore", mcc: "5942" });
  const [rowAmount, setRowAmount] = useState<Record<string, number>>({});
  const [purchaseLimit, setPurchaseLimit] = useState(0);

  const push = (e: Omit<LogEntry, "id" | "at">) =>
    setLog((l) => [{ ...e, id: ++seq, at: new Date().toLocaleTimeString() }, ...l].slice(0, 200));

  const mint = useCallback(async (accounts: string[]) => {
    const res = await fetch("/api/token", { method: "POST", body: JSON.stringify({ sub, accounts }) });
    const j = await res.json();
    if (j.token) setToken(j.token);
    push({ label: "mint token", method: "LOCAL", path: "/api/token", results: [{ status: res.status, replayed: false, ms: 0, body: { sub, accounts } }] });
    return j.token as string;
  }, [sub]);

  async function gateway(label: string, method: string, path: string, body?: object | string, opts: { key?: string; tok?: string; times?: number } = {}) {
    const key = method === "GET" ? undefined : opts.key ?? newKey();
    const raw = typeof body === "string" ? body : body ? JSON.stringify(body) : undefined;
    const headers: Record<string, string> = { "x-sim-repeat": String(opts.times ?? 1) };
    const t = opts.tok ?? token;
    if (t) headers.authorization = `Bearer ${t}`;
    if (key) headers["idempotency-key"] = key;
    setBusy(true);
    try {
      const res = await fetch(`/api/gateway/${path.replace(/^\//, "")}`, { method, headers, body: raw });
      const results = (await res.json()) as CallResult[];
      push({ label, method, path, key, results });
      if (key) setLast({ method, path, body: raw, key, label });
      return results;
    } finally {
      setBusy(false);
    }
  }

  async function refresh(acc = account, tok = token) {
    if (!acc) return;
    const [r] = await gateway("get account", "GET", `/v1/accounts/${acc.id}`, undefined, { tok });
    if (r.status === 200) setAccount(r.body);
  }

  async function createAccount() {
    const tok = token || (await mint([]));
    const results = await gateway("create account", "POST", "/v1/accounts", { currency }, { tok, times: repeat });
    const ok = results.find((r) => r.status === 201);
    if (ok) {
      setAccount(ok.body);
      setDeposits([]);
      await mint([ok.body.id]); // token must list the new account in authorized_accounts
    }
  }

  async function createDeposit() {
    if (!account) return;
    const results = await gateway("create deposit", "POST", `/v1/accounts/${account.id}/collateral/deposits`, { amount_minor: amount }, { times: repeat });
    const ok = results.find((r) => r.status === 201);
    if (ok) setDeposits((d) => (d.some((x) => x.id === ok.body.id) ? d : [ok.body, ...d]));
  }

  async function bank(dep: Deposit, status: "settled" | "failed") {
    setBusy(true);
    try {
      const external_ref = `bank-${dep.id.slice(-8)}`;
      const res = await fetch("/api/bank/webhook", {
        method: "POST",
        body: JSON.stringify({ deposit_id: dep.id, external_ref, amount_minor: dep.amount_minor, currency: dep.currency, status, repeat, tamper, stale }),
      });
      const results = (await res.json()) as CallResult[];
      push({ label: `bank webhook (${status}${tamper ? ", tampered" : ""}${stale ? ", stale" : ""})`, method: "POST", path: "/v1/webhooks/bank/deposits", results });
      const ok = results.find((r) => r.status === 200);
      if (ok) {
        setDeposits((d) => d.map((x) => (x.id === dep.id ? ok.body.deposit : x)));
        setAccount(ok.body.account);
      }
    } finally {
      setBusy(false);
    }
  }

  async function refreshTx(acc = account) {
    if (!acc) return;
    const [r] = await gateway("list transactions", "GET", `/v1/accounts/${acc.id}/transactions?page_size=50`);
    if (r.status === 200) setTxs(r.body.transactions);
  }

  async function network(label: string, op: "authorize" | "capture" | "reversal" | "refunds", payload: object, authId?: string) {
    setBusy(true);
    try {
      const res = await fetch("/api/network", { method: "POST", body: JSON.stringify({ op, authId, payload, repeat, tamper }) });
      const results = (await res.json()) as CallResult[];
      const path = authId ? `/v1/network/authorizations/${authId}/${op}` : "/v1/network/authorizations";
      push({ label: `network ${label}${tamper ? " (tampered)" : ""}`, method: "POST", path, results });
    } finally {
      setBusy(false);
    }
    await refresh();
    await refreshTx();
  }

  async function authorize() {
    if (!account) return;
    await network("authorize", "authorize", {
      network_ref: `n-${crypto.randomUUID()}`, card_id: account.id, amount_minor: buy.amount, currency: buy.currency,
      merchant: { id: buy.merchantId, name: buy.merchantName, mcc: buy.mcc },
    });
  }

  async function control(label: string, method: string, path: string, body?: object) {
    if (!account) return;
    const [r] = await gateway(label, method, path, body);
    if (r.status === 200) setAccount(r.body);
  }

  async function replayLast(times: number) {
    if (!last) return;
    await gateway(`${last.label} (same key ×${times})`, last.method, last.path, last.body, { key: last.key, times });
    await refresh();
  }

  async function tamperedBody() {
    if (!last) return;
    await gateway(`${last.label} (same key, different body)`, last.method, last.path, JSON.stringify({ amount_minor: 1 }), { key: last.key });
  }

  return (
    <main>
      <header>
        <h1>acme-card · flow simulator</h1>
        <span className="muted">open account → deposit collateral → bank settles → card active → authorize · capture · reverse · refund</span>
      </header>

      <div className="grid">
        <div className="col">
          <section className="panel">
            <h2>1 · Identity (dev JWT)</h2>
            <div className="row">
              <label>Customer (sub)<input className="wide" value={sub} onChange={(e) => setSub(e.target.value)} /></label>
              <button onClick={() => mint(account ? [account.id] : [])}>Mint token</button>
            </div>
            <div className="mono muted">{token ? `${token.slice(0, 48)}…` : "no token yet"}</div>
          </section>

          <section className="panel">
            <h2>2 · Account</h2>
            <div className="row">
              <label>Currency
                <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
                  {["USD", "COP", "EUR", "MXN"].map((c) => <option key={c}>{c}</option>)}
                </select>
              </label>
              <button className="primary" disabled={busy} onClick={createAccount}>Create account</button>
              <button disabled={!account || busy} onClick={() => refresh()}>Refresh</button>
            </div>
            {account && (
              <>
                <div className="row">
                  <span className={`badge s-${account.state}`}>{account.state}</span>
                  <span className="mono muted">{account.id}</span>
                </div>
                <div className="stats">
                  <div className="stat"><span className="muted">Collateral</span><b>{fmt(account.collateral_minor, account.currency)}</b></div>
                  <div className="stat"><span className="muted">Credit limit</span><b>{fmt(account.credit_limit_minor, account.currency)}</b></div>
                  <div className="stat"><span className="muted">Available</span><b>{fmt(account.available_credit_minor, account.currency)}</b></div>
                </div>
                <div className="row">
                  {account.frozen
                    ? <button disabled={busy} onClick={() => control("unfreeze card", "POST", `/v1/accounts/${account.id}/unfreeze`)}>Unfreeze card</button>
                    : <button disabled={busy} onClick={() => control("freeze card", "POST", `/v1/accounts/${account.id}/freeze`)}>Freeze card</button>}
                  {account.frozen && <span className="badge s-failed">frozen</span>}
                  <label className="inline">Purchase limit (minor, 0 = none)
                    <input type="number" min={0} value={purchaseLimit} onChange={(e) => setPurchaseLimit(Number(e.target.value))} />
                  </label>
                  <button disabled={busy} onClick={() => control("set purchase limit", "PUT", `/v1/accounts/${account.id}/limits`, { purchase_limit_minor: purchaseLimit })}>Set limit</button>
                </div>
              </>
            )}
          </section>

          <section className="panel">
            <h2>3 · Collateral deposits</h2>
            <div className="row">
              <label>Amount (minor units)<input type="number" min={1} value={amount} onChange={(e) => setAmount(Number(e.target.value))} /></label>
              <button className="primary" disabled={!account || busy} onClick={createDeposit}>Create deposit intent</button>
            </div>
            <div className="row">
              <label className="inline">Concurrent sends <input type="number" min={1} max={100} value={repeat} onChange={(e) => setRepeat(Number(e.target.value))} /></label>
              <label className="inline"><input type="checkbox" checked={tamper} onChange={(e) => setTamper(e.target.checked)} /> bad signature</label>
              <label className="inline"><input type="checkbox" checked={stale} onChange={(e) => setStale(e.target.checked)} /> stale timestamp</label>
            </div>
            {deposits.length > 0 && (
              <table>
                <thead><tr><th>Deposit</th><th>Amount</th><th>Status</th><th>Bank</th></tr></thead>
                <tbody>
                  {deposits.map((d) => (
                    <tr key={d.id}>
                      <td className="mono">{d.id.slice(-12)}</td>
                      <td>{fmt(d.amount_minor, d.currency)}</td>
                      <td><span className={`badge s-${d.status}`}>{d.status}</span></td>
                      <td className="row">
                        <button disabled={busy} onClick={() => bank(d, "settled")}>Settle</button>
                        <button disabled={busy} onClick={() => bank(d, "failed")}>Fail</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>

          <section className="panel">
            <h2>4 · Idempotency lab</h2>
            <div className="muted" style={{ marginBottom: 8 }}>
              Last keyed request: {last ? <span className="mono">{last.label} · {last.key.slice(0, 8)}…</span> : "none yet"}
            </div>
            <div className="row">
              <button disabled={!last || busy} onClick={() => replayLast(1)}>Repeat with same key</button>
              <button disabled={!last || busy} onClick={() => replayLast(20)}>Burst ×20 same key</button>
              <button disabled={!last || busy} onClick={tamperedBody}>Same key, different body</button>
            </div>
          </section>
          <section className="panel">
            <h2>5 · Merchant / network</h2>
            <div className="row">
              <label>Amount (minor)<input type="number" min={1} value={buy.amount} onChange={(e) => setBuy({ ...buy, amount: Number(e.target.value) })} /></label>
              <label>Currency
                <select value={buy.currency} onChange={(e) => setBuy({ ...buy, currency: e.target.value })}>
                  {["USD", "COP", "EUR", "MXN"].map((c) => <option key={c}>{c}</option>)}
                </select>
              </label>
            </div>
            <div className="row">
              <label>Merchant id<input value={buy.merchantId} onChange={(e) => setBuy({ ...buy, merchantId: e.target.value })} /></label>
              <label>Name<input value={buy.merchantName} onChange={(e) => setBuy({ ...buy, merchantName: e.target.value })} /></label>
              <label>MCC<input style={{ width: 70 }} maxLength={4} value={buy.mcc} onChange={(e) => setBuy({ ...buy, mcc: e.target.value })} /></label>
            </div>
            <div className="row">
              <button className="primary" disabled={!account || busy} onClick={authorize}>Authorize purchase</button>
              <button disabled={!account || busy} onClick={() => refreshTx()}>Refresh transactions</button>
              <span className="muted">Uses "Concurrent sends" and "bad signature" from panel 3.</span>
            </div>
            {txs.length > 0 && (
              <table>
                <thead><tr><th>Merchant</th><th>Amount</th><th>Status</th><th>Captured / refunded</th><th>Actions</th></tr></thead>
                <tbody>
                  {txs.map((t) => (
                    <tr key={t.id}>
                      <td>{t.merchant.name}<div className="mono muted">{t.merchant.mcc}</div></td>
                      <td>{fmt(t.amount_minor, t.currency)}</td>
                      <td>
                        <span className={`badge s-${t.status === "approved" ? "pending" : t.status === "captured" ? "settled" : "failed"}`}>{t.status}</span>
                        {t.decline_reason && <div className="mono muted">{t.decline_reason}</div>}
                      </td>
                      <td>{fmt(t.captured_amount_minor, t.currency)} / {fmt(t.refunded_amount_minor, t.currency)}</td>
                      <td>
                        <div className="row">
                          <input type="number" min={1} aria-label={`Amount for ${t.merchant.name} ${t.id.slice(-6)}`} value={rowAmount[t.id] ?? t.amount_minor}
                            onChange={(e) => setRowAmount({ ...rowAmount, [t.id]: Number(e.target.value) })} />
                        </div>
                        <div className="row">
                          <button disabled={busy || t.status !== "approved"} onClick={() => network("capture", "capture", { network_ref: `c-${t.id}`, amount_minor: rowAmount[t.id] ?? t.amount_minor }, t.id)}>Capture</button>
                          <button disabled={busy || t.status !== "approved"} onClick={() => network("reversal", "reversal", { network_ref: `v-${t.id}` }, t.id)}>Reverse</button>
                          <button disabled={busy || t.status !== "captured"} onClick={() => network("refund", "refunds", { network_ref: `r-${crypto.randomUUID()}`, amount_minor: rowAmount[t.id] ?? t.captured_amount_minor }, t.id)}>Refund</button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        </div>

        <section className="panel log">
          <h2>Request log</h2>
          {log.length === 0 && <div className="muted">Calls appear here with status, replay flag and latency.</div>}
          {log.map((e) => <LogRow key={e.id} e={e} />)}
        </section>
      </div>
    </main>
  );
}

function LogRow({ e }: { e: LogEntry }) {
  const counts = new Map<string, number>();
  for (const r of e.results) {
    const k = `${r.status}${r.replayed ? " replayed" : ""}`;
    counts.set(k, (counts.get(k) ?? 0) + 1);
  }
  const first = e.results[0];
  return (
    <details style={{ borderBottom: "1px solid var(--border)", padding: "8px 0" }}>
      <summary>
        <span className="muted">{e.at}</span>{" "}
        <b>{e.label}</b>{" "}
        {[...counts].map(([k, n]) => (
          <span key={k} className={`code-${k[0]}`} style={{ marginRight: 8 }}>{n > 1 ? `${n}× ` : ""}{k}</span>
        ))}
        <span className="muted">{first?.ms}ms</span>
      </summary>
      <div className="summary muted">{e.method} {e.path}{e.key ? ` · Idempotency-Key ${e.key}` : ""}</div>
      <pre>{JSON.stringify(first?.body, null, 2)}</pre>
    </details>
  );
}
