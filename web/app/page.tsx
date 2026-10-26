"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { daysToDuePlus, daysToNextClose, formatMinor } from "@/lib/billing.ts";
import { maskAccount } from "@/lib/payouts.ts";
import { auditEventsQuery, type AuditFilter } from "@/lib/audit.ts";
import { channelIcon, newItems } from "@/lib/notifications.ts";

type CallResult = { status: number; replayed: boolean; ms: number; body: any };
type LogEntry = { id: number; at: string; label: string; method: string; path: string; key?: string; results: CallResult[] };
type Account = {
  id: string; customer_id: string; currency: string; state: string; frozen: boolean;
  credit_limit_minor: number; available_credit_minor: number; collateral_minor: number;
  outstanding_minor: number; credit_balance_minor: number; restricted_reason?: string;
  account_time?: string; activated_at?: string; delinquency?: string;
  close_stage?: string; close_requested_at?: string; destination_last4?: string;
};
type Payout = { id: string; amount_minor: number; currency: string; status: string; attempt: number; attempt_id: string; destination_last4: string };
type Statement = {
  id: string; cycle: number; currency: string; period_start: string; period_end: string; due_date?: string;
  previous_balance_minor: number; purchases_minor: number; refunds_minor: number; payments_minor: number; returns_minor: number;
  interest_minor: number; fees_minor: number; waivers_minor: number; collateral_applied_minor: number;
  new_balance_minor: number; minimum_due_minor: number; sha256: string;
};
type Summary = {
  account_time: string; cycle_start?: string; cycle_end?: string; minimum_due_minor: number; due_date?: string;
  past_due_minor: number; dpd: number; grace: boolean; stage: string; currency: string;
};
type Charge = { id: string; kind: string; cycle: number; amount_minor: number; waived_minor: number; effective_at?: string; status: string };
type AuditEvent = {
  id: string; event_type: string; aggregate_type: string; aggregate_id: string; actor?: string; occurred_at: string;
  source?: string; source_seq?: number; chain_seq: number; hash: string; redacted: boolean; after?: unknown;
};
type InboxItem = { id: number; channel: string; type: string; text: string; created_at: string };
type NotifPref = { type: string; push: boolean; sms: boolean; email: boolean; mandatory: boolean };
type Partner = { id: string; name: string; url: string; event_types: string[]; active: boolean; key?: string };
type PartnerDelivery = { id: string; event_type: string; status: string; attempts: number; last_error: string };
type PartnerEvent = { event_id: string; event_type: string; delivery_id: string; verified: boolean; received_at: string };
type Deposit = { id: string; amount_minor: number; currency: string; status: string; external_ref?: string };
type Payment = { id: string; amount_minor: number; currency: string; status: string; external_ref?: string; return_ref?: string };
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
  const [payments, setPayments] = useState<Payment[]>([]);
  const [payAmount, setPayAmount] = useState(1000);
  const [summary, setSummary] = useState<Summary | null>(null);
  const [statements, setStatements] = useState<Statement[]>([]);
  const [doc, setDoc] = useState<{ statement: Statement; document: unknown } | null>(null);
  const [charges, setCharges] = useState<Charge[]>([]);
  const [advanceDays, setAdvanceDays] = useState(30);
  const [dueOffset, setDueOffset] = useState(1);
  const [waive, setWaive] = useState<{ amount: number; reason: string }>({ amount: 2500, reason: "goodwill" });
  const [supportToken, setSupportToken] = useState("");
  const [destination, setDestination] = useState({ bank_code: "BANK001", account_number: "000123456789", holder_name: "Jane Doe" });
  const [closeReason, setCloseReason] = useState("customer request");
  const [payouts, setPayouts] = useState<Payout[]>([]);
  const [auditorToken, setAuditorToken] = useState("");
  const [auditFilter, setAuditFilter] = useState<AuditFilter>({ page_size: 50 });
  const [auditEvents, setAuditEvents] = useState<AuditEvent[]>([]);
  const [auditNext, setAuditNext] = useState("");
  const [auditStatus, setAuditStatus] = useState<{ label: string; ok: boolean; detail: string } | null>(null);
  const [auditExport, setAuditExport] = useState<{ id: string; count: number; sha256: string } | null>(null);
  const [inbox, setInbox] = useState<InboxItem[]>([]);
  const [toasts, setToasts] = useState<InboxItem[]>([]);
  const [prefs, setPrefs] = useState<NotifPref[]>([]);
  const [partners, setPartners] = useState<Partner[]>([]);
  const [partnerKeys, setPartnerKeys] = useState<Record<string, string>>({});
  const [partnerDeliveries, setPartnerDeliveries] = useState<Record<string, PartnerDelivery[]>>({});
  const [partnerEvents, setPartnerEvents] = useState<PartnerEvent[]>([]);
  const [partnerTypes, setPartnerTypes] = useState("authorization.approved,authorization.declined,payment.settled,card.frozen");
  const inboxRef = useRef<InboxItem[]>([]);

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

  async function refreshPayments(acc = account) {
    if (!acc) return;
    const [r] = await gateway("list payments", "GET", `/v1/accounts/${acc.id}/payments?page_size=50`);
    if (r.status === 200) setPayments(r.body.payments);
  }

  async function createPayment() {
    if (!account) return;
    const results = await gateway("create payment", "POST", `/v1/accounts/${account.id}/payments`, { amount_minor: payAmount }, { times: repeat });
    const ok = results.find((r) => r.status === 201);
    if (ok) setAccount(ok.body.account);
    await refreshPayments();
  }

  async function bankPayment(op: "settle" | "fail" | "return", p: Payment) {
    setBusy(true);
    try {
      const ref = op === "return" ? `ret-${p.id.slice(-8)}` : `bank-${p.id.slice(-8)}`;
      const res = await fetch("/api/bank/payments", { method: "POST", body: JSON.stringify({ op, payment: p, ref, repeat, tamper }) });
      const results = (await res.json()) as CallResult[];
      const path = op === "return" ? `/v1/webhooks/bank/payments/${p.id}/return` : "/v1/webhooks/bank/payments";
      push({ label: `bank payment ${op}${tamper ? " (tampered)" : ""}`, method: "POST", path, results });
      const ok = results.find((r) => r.status === 200);
      if (ok) setAccount(ok.body.account);
    } finally {
      setBusy(false);
    }
    await refreshPayments();
  }

  async function refreshBilling(acc = account) {
    if (!acc) return;
    const [r] = await gateway("billing summary", "GET", `/v1/accounts/${acc.id}/billing`);
    if (r.status === 200) setSummary(r.body);
  }

  async function refreshStatements(acc = account) {
    if (!acc) return;
    const [r] = await gateway("list statements", "GET", `/v1/accounts/${acc.id}/statements`);
    if (r.status === 200) setStatements(r.body.statements);
  }

  async function viewStatement(st: Statement) {
    if (!account) return;
    const [r] = await gateway(`statement ${st.cycle}`, "GET", `/v1/accounts/${account.id}/statements/${st.id}`);
    if (r.status === 200) setDoc(r.body);
  }

  async function refreshCharges(acc = account) {
    if (!acc) return;
    const [r] = await gateway("list charges", "GET", `/v1/accounts/${acc.id}/charges`);
    if (r.status === 200) setCharges(r.body.charges);
  }

  async function refreshAllBilling() {
    await refresh();
    await Promise.all([refreshBilling(), refreshStatements(), refreshCharges()]);
  }

  async function advance(days: number | null, label: string) {
    if (!account || !days) return;
    const results = await gateway(`advance clock ${label} (+${days}d)`, "POST", `/v1/dev/accounts/${account.id}/clock/advance`, { days }, { times: repeat });
    const ok = results.find((r) => r.status === 200);
    if (ok) {
      setAccount(ok.body.account);
      setSummary(ok.body.billing);
    }
    await Promise.all([refreshStatements(), refreshCharges(), refreshTx(), refreshPayments()]);
  }

  async function waiveCharge(c: Charge) {
    if (!account) return;
    let tok = supportToken;
    if (!tok) {
      const res = await fetch("/api/token", { method: "POST", body: JSON.stringify({ sub: "agent-sim", accounts: [], role: "support" }) });
      tok = (await res.json()).token;
      setSupportToken(tok);
      push({ label: "mint support token", method: "LOCAL", path: "/api/token", results: [{ status: res.status, replayed: false, ms: 0, body: { sub: "agent-sim", role: "support" } }] });
    }
    await gateway(`waive ${c.kind} (support)`, "POST", `/v1/support/accounts/${account.id}/charges/${c.id}/waivers`,
      { amount_minor: waive.amount, reason: waive.reason }, { tok, times: repeat });
    await refreshAllBilling();
  }

  async function ensureSupportToken() {
    if (supportToken) return supportToken;
    const res = await fetch("/api/token", { method: "POST", body: JSON.stringify({ sub: "agent-sim", accounts: [], role: "support" }) });
    const tok = (await res.json()).token as string;
    setSupportToken(tok);
    push({ label: "mint support token", method: "LOCAL", path: "/api/token", results: [{ status: res.status, replayed: false, ms: 0, body: { sub: "agent-sim", role: "support" } }] });
    return tok;
  }

  async function ensureAuditorToken() {
    if (auditorToken) return auditorToken;
    const res = await fetch("/api/token", { method: "POST", body: JSON.stringify({ sub: "auditor-sim", accounts: [], role: "auditor" }) });
    const tok = (await res.json()).token as string;
    setAuditorToken(tok);
    push({ label: "mint auditor token", method: "LOCAL", path: "/api/token", results: [{ status: res.status, replayed: false, ms: 0, body: { sub: "auditor-sim", role: "auditor" } }] });
    return tok;
  }

  async function auditCall(label: string, method: string, path: string, body?: object) {
    const tok = await ensureAuditorToken();
    const [r] = await gateway(label, method, path, body, { tok });
    if (r.status >= 400 || r.status === 0)
      setAuditStatus({ label, ok: false, detail: `${r.status} ${r.body?.error?.message ?? r.body?.title ?? "request failed"}` });
    return r;
  }

  async function auditSearch(pageToken = "") {
    let q: string;
    try {
      q = auditEventsQuery(auditFilter);
    } catch (e) {
      setAuditStatus({ label: "search", ok: false, detail: String(e) });
      return;
    }
    if (pageToken) q += `${q ? "&" : "?"}page_token=${encodeURIComponent(pageToken)}`;
    const r = await auditCall("audit search", "GET", `/v1/audit/events${q}`);
    if (r.status !== 200) return;
    setAuditEvents((prev) => (pageToken ? [...prev, ...r.body.events] : r.body.events));
    setAuditNext(r.body.next_page_token ?? "");
    setAuditStatus({ label: "search", ok: true, detail: `${r.body.events.length} events${r.body.next_page_token ? " (more available)" : ""}` });
  }

  async function auditVerify() {
    const r = await auditCall("verify chain", "GET", "/v1/audit/verify");
    if (r.status !== 200) return;
    const b = r.body;
    const ok = b.chain_valid && b.checkpoints_valid && b.gaps.length === 0 && b.stuck_outbox.length === 0;
    setAuditStatus({ label: "verify", ok, detail: ok ? `chain intact · ${b.checked} events${b.incremental ? ` since full run through #${b.full_through_chain_seq} (${new Date(b.full_verified_at).toLocaleString()})` : " (full walk)"} · ${b.checkpoints} checkpoints`
      : `chain ${b.chain_valid ? "ok" : `broken at ${b.first_bad_chain_seq}`} · checkpoints ${b.checkpoints_valid ? "ok" : b.detail} · ${b.gaps.length} gaps · ${b.stuck_outbox.length} stuck outboxes` });
  }

  async function auditCheckpoint() {
    const r = await auditCall("create checkpoint", "POST", "/v1/audit/checkpoints", {});
    if (r.status === 201) setAuditStatus({ label: "checkpoint", ok: true, detail: `anchored chain_seq ${r.body.chain_seq} · ${r.body.s3_key}` });
  }

  async function auditReconcile(run: boolean) {
    const r = await auditCall(run ? "run reconciliation" : "last reconciliation", run ? "POST" : "GET", "/v1/audit/reconciliation", run ? {} : undefined);
    if (r.status === 200 || r.status === 201)
      setAuditStatus({ label: "reconciliation", ok: r.body.total_violations === 0, detail: `${r.body.total_violations} violations · ${r.body.checks.length} checks · ${r.body.ran_at}` });
  }

  async function auditSnapshot() {
    const r = await auditCall("snapshot accounts", "POST", "/v1/audit/snapshots", {});
    if (r.status === 202) setAuditStatus({ label: "snapshot", ok: true, detail: `${r.body.count} account snapshots queued (written in the background)` });
  }

  async function auditReplay() {
    const id = auditFilter.account_id?.trim() || account?.id;
    if (!id) return;
    const r = await auditCall("replay account", "GET", `/v1/audit/replay/${encodeURIComponent(id)}`);
    if (r.status === 200)
      setAuditStatus({ label: "replay", ok: r.body.differences.length === 0, detail: r.body.differences.length === 0 ? `trail matches live state (${id})`
        : r.body.differences.map((d: { field: string; audit: string; live: string }) => `${d.field}: audit ${d.audit} ≠ live ${d.live}`).join(" · ") });
  }

  async function auditCreateExport() {
    const f: Record<string, string> = {};
    for (const k of ["account_id", "event_type", "actor"] as const) if (auditFilter[k]?.trim()) f[k] = auditFilter[k]!.trim();
    for (const k of ["from", "to"] as const) if (auditFilter[k]) f[k] = new Date(auditFilter[k]!).toISOString();
    const r = await auditCall("create export", "POST", "/v1/audit/exports", { filter: f });
    if (r.status === 201) setAuditExport({ id: r.body.id, count: r.body.count, sha256: r.body.sha256 });
  }

  async function auditDownload() {
    if (!auditExport) return;
    const r = await auditCall("download export", "GET", `/v1/audit/exports/${auditExport.id}`);
    if (r.status !== 200) return;
    const text = typeof r.body === "string" ? r.body : JSON.stringify(r.body) + "\n";
    const url = URL.createObjectURL(new Blob([text], { type: "application/x-ndjson" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `audit-export-${auditExport.id}.ndjson`;
    a.click();
    URL.revokeObjectURL(url);
  }

  // Popups: poll the account's inbox quietly (not logged) and show each new item for 6 seconds.
  useEffect(() => {
    if (!account || !token) return;
    inboxRef.current = [];
    setInbox([]);
    let first = true;
    const poll = async () => {
      try {
        const res = await fetch(`/api/gateway/v1/accounts/${account.id}/notifications`, { headers: { authorization: `Bearer ${token}` } });
        const [r] = (await res.json()) as CallResult[];
        if (r?.status !== 200) return;
        const fresh = newItems(inboxRef.current, r.body.items as InboxItem[]);
        if (fresh.length === 0) return;
        inboxRef.current = [...inboxRef.current, ...fresh];
        setInbox([...inboxRef.current].reverse());
        if (first) return; // what was already there on load is history, not news
        setToasts((t) => [...t, ...fresh]);
        for (const it of fresh) setTimeout(() => setToasts((t) => t.filter((x) => x.id !== it.id)), 6000);
      } catch {} finally {
        first = false;
      }
    };
    poll();
    const id = setInterval(poll, 3000);
    return () => clearInterval(id);
  }, [account?.id, token]);

  async function refreshInbox() {
    if (!account) return;
    const [r] = await gateway("list notifications", "GET", `/v1/accounts/${account.id}/notifications`);
    if (r.status === 200) setInbox(r.body.items);
  }

  async function refreshPrefs() {
    if (!account) return;
    const [r] = await gateway("notification preferences", "GET", `/v1/accounts/${account.id}/notification-preferences`);
    if (r.status === 200) setPrefs(r.body.preferences);
  }

  async function setPref(p: NotifPref, channel: "push" | "sms" | "email", on: boolean) {
    if (!account) return;
    const [r] = await gateway(`set ${p.type} ${channel}=${on}`, "PUT", `/v1/accounts/${account.id}/notification-preferences`,
      { type: p.type, push: p.push, sms: p.sms, email: p.email, [channel]: on });
    if (r.status === 200) setPrefs((all) => all.map((x) => (x.type === p.type ? { ...x, ...r.body } : x)));
  }

  async function registerPartner() {
    const tok = await ensureSupportToken();
    const key = crypto.randomUUID();
    const [r] = await gateway("register demo partner (support)", "POST", "/v1/support/webhook-subscriptions",
      { name: `demo-${key.slice(0, 8)}`, url: `http://web:3000/api/partner/webhook/${key}`, event_types: partnerTypes.split(",").map((t) => t.trim()).filter(Boolean) }, { tok });
    if (r.status !== 201) return;
    await fetch("/api/partner/secret", { method: "POST", body: JSON.stringify({ key, secret: r.body.secret }) });
    setPartnerKeys((k) => ({ ...k, [r.body.id]: key }));
    await listPartners(tok);
  }

  async function listPartners(tok?: string) {
    const [r] = await gateway("list partners (support)", "GET", "/v1/support/webhook-subscriptions", undefined, { tok: tok ?? (await ensureSupportToken()) });
    if (r.status === 200) setPartners(r.body.subscriptions);
  }

  async function disablePartner(p: Partner) {
    await gateway(`disable partner ${p.name} (support)`, "DELETE", `/v1/support/webhook-subscriptions/${p.id}`, undefined, { tok: await ensureSupportToken() });
    await listPartners();
  }

  async function partnerDeliveriesFor(p: Partner) {
    const [r] = await gateway(`deliveries of ${p.name} (support)`, "GET", `/v1/support/webhook-subscriptions/${p.id}/deliveries`, undefined, { tok: await ensureSupportToken() });
    if (r.status === 200) setPartnerDeliveries((d) => ({ ...d, [p.id]: r.body.deliveries }));
    const key = partnerKeys[p.id];
    if (key) {
      const res = await fetch(`/api/partner/webhook/${key}`);
      setPartnerEvents(((await res.json()) as { events: PartnerEvent[] }).events);
    }
  }

  async function refreshPayouts(acc = account) {
    if (!acc) return;
    const [r] = await gateway("list payouts", "GET", `/v1/accounts/${acc.id}/payouts`);
    if (r.status === 200) setPayouts(r.body.payouts);
  }

  async function closeAccount(asSupport: boolean) {
    if (!account) return;
    const tok = asSupport ? await ensureSupportToken() : token;
    const path = asSupport ? `/v1/support/accounts/${account.id}/close` : `/v1/accounts/${account.id}/close`;
    const body = asSupport ? { destination, reason: closeReason } : { destination };
    const results = await gateway(asSupport ? "support close" : "close account", "POST", path, body, { tok, times: repeat });
    const ok = results.find((r) => r.status === 202);
    if (ok) setAccount(ok.body);
    await refreshPayouts();
  }

  async function cancelAccount() {
    if (!account) return;
    const [r] = await gateway("cancel account", "POST", `/v1/accounts/${account.id}/cancel`, undefined, { times: repeat });
    if (r.status === 200) setAccount(r.body);
  }

  async function bankPayout(op: "instruction" | "settle" | "fail", p: Payout) {
    setBusy(true);
    try {
      const res = await fetch("/api/bank/payouts", { method: "POST", body: JSON.stringify({ op, attempt_id: p.attempt_id, amount_minor: p.amount_minor,
        currency: p.currency, last4: p.destination_last4, repeat, tamper }) });
      const results = (await res.json()) as CallResult[];
      // The bank sees the full number; the simulator log shows it masked.
      for (const r of results) if (r.body && typeof r.body === "object" && "account_number" in r.body) r.body = { ...r.body, account_number: maskAccount(String(r.body.account_number)) };
      const path = op === "instruction" ? `/v1/bank/payout-instructions/${p.attempt_id}` : "/v1/webhooks/bank/payouts";
      push({ label: `bank payout ${op}${tamper ? " (tampered)" : ""}`, method: op === "instruction" ? "GET" : "POST", path, results });
    } finally {
      setBusy(false);
    }
    await refresh();
    await refreshPayouts();
  }

  async function retryPayout(p: Payout) {
    if (!account) return;
    const tok = await ensureSupportToken();
    await gateway("retry payout (support)", "POST", `/v1/support/accounts/${account.id}/payouts/${p.id}/retry`, undefined, { tok });
    await refreshPayouts();
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
        <h1>acme-card · flow simulator <a href="/portal" style={{ fontSize: 14, fontWeight: 400 }}>cardholder portal →</a></h1>
        <span className="muted">open account → collateral → card active → authorize · capture · reverse · refund → pay · settle · fail · return → advance clock · statements · waive → close · payout</span>
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
              <button data-action="create-account" className="primary" disabled={busy} onClick={createAccount}>Create account</button>
              <button data-action="refresh-account" disabled={!account || busy} onClick={() => refresh()}>Refresh</button>
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
                <div className="stats">
                  <div className="stat"><span className="muted">Outstanding</span><b>{fmt(account.outstanding_minor ?? 0, account.currency)}</b></div>
                  <div className="stat"><span className="muted">Credit balance</span><b>{fmt(account.credit_balance_minor ?? 0, account.currency)}</b></div>
                  <div className="stat"><span className="muted">Restriction</span><b>{account.restricted_reason ?? "none"}</b></div>
                </div>
                <div className="stats">
                  <div className="stat"><span className="muted">Account time</span><b>{account.account_time?.slice(0, 16).replace("T", " ") ?? "—"}</b></div>
                  <div className="stat"><span className="muted">Delinquency</span><b>{account.delinquency ?? "none"}</b></div>
                  <div className="stat"><span className="muted">Activated</span><b>{account.activated_at?.slice(0, 10) ?? "—"}</b></div>
                </div>
                <div className="row">
                  {account.frozen
                    ? <button data-action="unfreeze-card" disabled={busy} onClick={() => control("unfreeze card", "POST", `/v1/accounts/${account.id}/unfreeze`)}>Unfreeze card</button>
                    : <button data-action="freeze-card" disabled={busy} onClick={() => control("freeze card", "POST", `/v1/accounts/${account.id}/freeze`)}>Freeze card</button>}
                  {account.frozen && <span className="badge s-failed">frozen</span>}
                  {account.restricted_reason && <span className="badge s-failed">restricted: {account.restricted_reason}</span>}
                  <label className="inline">Purchase limit (minor, 0 = none)
                    <input type="number" min={0} value={purchaseLimit} onChange={(e) => setPurchaseLimit(Number(e.target.value))} />
                  </label>
                  <button data-action="set-purchase-limit" disabled={busy} onClick={() => control("set purchase limit", "PUT", `/v1/accounts/${account.id}/limits`, { purchase_limit_minor: purchaseLimit })}>Set limit</button>
                </div>
              </>
            )}
          </section>

          <section className="panel">
            <h2>3 · Collateral deposits</h2>
            <div className="row">
              <label>Amount (minor units)<input type="number" min={1} value={amount} onChange={(e) => setAmount(Number(e.target.value))} /></label>
              <button data-action="create-deposit" className="primary" disabled={!account || busy} onClick={createDeposit}>Create deposit intent</button>
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
                        <button data-action="bank-settle-deposit" disabled={busy} onClick={() => bank(d, "settled")}>Settle</button>
                        <button data-action="bank-fail-deposit" disabled={busy} onClick={() => bank(d, "failed")}>Fail</button>
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
              <button data-action="network-authorize" className="primary" disabled={!account || busy} onClick={authorize}>Authorize purchase</button>
              <button data-action="refresh-transactions" disabled={!account || busy} onClick={() => refreshTx()}>Refresh transactions</button>
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
                          <button data-action="network-capture" disabled={busy || t.status !== "approved"} onClick={() => network("capture", "capture", { network_ref: `c-${t.id}`, amount_minor: rowAmount[t.id] ?? t.amount_minor }, t.id)}>Capture</button>
                          <button data-action="network-reverse" disabled={busy || t.status !== "approved"} onClick={() => network("reversal", "reversal", { network_ref: `v-${t.id}` }, t.id)}>Reverse</button>
                          <button data-action="network-refund" disabled={busy || t.status !== "captured"} onClick={() => network("refund", "refunds", { network_ref: `r-${crypto.randomUUID()}`, amount_minor: rowAmount[t.id] ?? t.captured_amount_minor }, t.id)}>Refund</button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
          <section className="panel">
            <h2>6 · Payments</h2>
            <div className="row">
              <label>Amount (minor, min 100)<input type="number" min={100} value={payAmount} onChange={(e) => setPayAmount(Number(e.target.value))} /></label>
              <button data-action="create-payment" className="primary" disabled={!account || busy} onClick={createPayment}>Create payment</button>
              <button data-action="refresh-payments" disabled={!account || busy} onClick={() => refreshPayments()}>Refresh payments</button>
            </div>
            <div className="muted" style={{ marginBottom: 8 }}>
              The bank answers below. Credit is restored only on Settle; Fail changes nothing; Return (within 5 days) raises the debt and restricts the card if it goes over the limit.
            </div>
            {payments.length > 0 && (
              <table>
                <thead><tr><th>Payment</th><th>Amount</th><th>Status</th><th>Bank</th></tr></thead>
                <tbody>
                  {payments.map((p) => (
                    <tr key={p.id}>
                      <td className="mono">{p.id.slice(-12)}</td>
                      <td>{fmt(p.amount_minor, p.currency)}</td>
                      <td><span className={`badge s-${p.status === "settled" ? "settled" : p.status === "pending" ? "pending" : "failed"}`}>{p.status}</span></td>
                      <td className="row">
                        <button data-action="bank-settle-payment" disabled={busy || p.status !== "pending"} onClick={() => bankPayment("settle", p)}>Settle</button>
                        <button data-action="bank-fail-payment" disabled={busy || p.status !== "pending"} onClick={() => bankPayment("fail", p)}>Fail</button>
                        <button data-action="bank-return-payment" disabled={busy || p.status !== "settled"} onClick={() => bankPayment("return", p)}>Return</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
          <section className="panel">
            <h2>7 · Billing (statements, interest, late fees)</h2>
            <div className="muted" style={{ marginBottom: 8 }}>
              Dev clock: moves this account's time forward and runs billing (closes, 22.80% APR interest, $25 late fee, block at 15 days past due, collateral applied at 60).
            </div>
            <div className="row">
              <label className="inline">Days <input type="number" min={1} max={400} value={advanceDays} onChange={(e) => setAdvanceDays(Number(e.target.value))} /></label>
              <button data-action="advance-clock" className="primary" disabled={!account || busy} onClick={() => advance(advanceDays, "custom")}>Advance</button>
              <button data-action="advance-clock" disabled={!account || busy} onClick={() => advance(1, "1 day")}>+1 day</button>
              <button data-action="advance-clock" disabled={!account || busy || !summary?.cycle_end} onClick={() => summary && advance(daysToNextClose(summary), "to next close")}>To next close</button>
              <label className="inline">Due + <input type="number" min={1} max={120} value={dueOffset} onChange={(e) => setDueOffset(Number(e.target.value))} /> days</label>
              <button data-action="advance-clock" disabled={!account || busy || !summary?.due_date} onClick={() => summary && advance(daysToDuePlus(summary, dueOffset), `to due + ${dueOffset}`)}>Go</button>
            </div>
            <div className="row">
              <button data-action="refresh-billing" disabled={!account || busy} onClick={() => refreshBilling()}>Refresh summary</button>
              <button data-action="refresh-statements" disabled={!account || busy} onClick={() => refreshStatements()}>Refresh statements</button>
              <button data-action="refresh-charges" disabled={!account || busy} onClick={() => refreshCharges()}>Refresh charges</button>
            </div>
            {summary && (
              <div className="stats">
                <div className="stat"><span className="muted">Minimum due</span><b>{formatMinor(summary.minimum_due_minor, summary.currency)}</b></div>
                <div className="stat"><span className="muted">Due date</span><b>{summary.due_date?.slice(0, 10) ?? "—"}</b></div>
                <div className="stat"><span className="muted">Past due · DPD</span><b>{formatMinor(summary.past_due_minor, summary.currency)} · {summary.dpd}</b></div>
                <div className="stat"><span className="muted">Stage · grace</span><b>{summary.stage} · {summary.grace ? "yes" : "no"}</b></div>
              </div>
            )}
            {statements.length > 0 && (
              <table>
                <thead><tr><th>#</th><th>Period</th><th>New balance</th><th>Interest / fees</th><th>Minimum</th><th>Due</th><th></th></tr></thead>
                <tbody>
                  {statements.map((st) => (
                    <tr key={st.id}>
                      <td>{st.cycle}</td>
                      <td className="mono">{st.period_start.slice(0, 10)} → {st.period_end.slice(0, 10)}</td>
                      <td>{formatMinor(st.new_balance_minor, st.currency)}</td>
                      <td>{formatMinor(st.interest_minor, st.currency)} / {formatMinor(st.fees_minor, st.currency)}</td>
                      <td>{formatMinor(st.minimum_due_minor, st.currency)}</td>
                      <td className="mono">{st.due_date?.slice(0, 10) ?? "balance only"}</td>
                      <td><button data-action="view-statement" disabled={busy} onClick={() => viewStatement(st)}>View</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {doc && (
              <details open>
                <summary>Statement {doc.statement.cycle} document (S3, sha256 {doc.statement.sha256.slice(0, 12)}…)</summary>
                <pre>{JSON.stringify(doc.document, null, 2)}</pre>
              </details>
            )}
            {charges.length > 0 && (
              <>
                <div className="row">
                  <label className="inline">Waive amount <input type="number" min={1} value={waive.amount} onChange={(e) => setWaive({ ...waive, amount: Number(e.target.value) })} /></label>
                  <label className="inline">Reason <input value={waive.reason} maxLength={200} onChange={(e) => setWaive({ ...waive, reason: e.target.value })} /></label>
                  <span className="muted">Waivers use a support-role token.</span>
                </div>
                <table>
                  <thead><tr><th>Charge</th><th>Cycle</th><th>Amount</th><th>Waived</th><th>Status</th><th>Support</th></tr></thead>
                  <tbody>
                    {charges.map((c) => (
                      <tr key={c.id}>
                        <td>{c.kind}</td>
                        <td>{c.cycle}</td>
                        <td>{formatMinor(c.amount_minor, account?.currency ?? "")}</td>
                        <td>{formatMinor(c.waived_minor, account?.currency ?? "")}</td>
                        <td><span className={`badge s-${c.status === "posted" ? "settled" : c.status === "pending" ? "pending" : "failed"}`}>{c.status}</span></td>
                        <td><button data-action="waive-charge" disabled={busy || c.status !== "posted" || c.waived_minor >= c.amount_minor} onClick={() => waiveCharge(c)}>Waive</button></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </>
            )}
          </section>
          <section className="panel">
            <h2>8 · Close, cancel and payouts</h2>
            <div className="muted" style={{ marginBottom: 8 }}>
              Close blocks the card at once; the account closes after in-flight work, the final statement and its due date (debt is taken from the collateral), and the 5-day payment return window. The rest is paid out to the bank account below once the bank confirms. Cancel ends an account that never activated.
            </div>
            <div className="row">
              <label>Bank code<input value={destination.bank_code} onChange={(e) => setDestination({ ...destination, bank_code: e.target.value })} /></label>
              <label>Account number<input value={destination.account_number} onChange={(e) => setDestination({ ...destination, account_number: e.target.value })} /></label>
              <label>Holder<input value={destination.holder_name} onChange={(e) => setDestination({ ...destination, holder_name: e.target.value })} /></label>
            </div>
            <div className="row">
              <button data-action="close-account" className="primary" disabled={!account || busy} onClick={() => closeAccount(false)}>Close account</button>
              <label className="inline">Support reason <input value={closeReason} maxLength={200} onChange={(e) => setCloseReason(e.target.value)} /></label>
              <button data-action="support-close-account" disabled={!account || busy} onClick={() => closeAccount(true)}>Close (support)</button>
              <button data-action="cancel-account" disabled={!account || busy} onClick={cancelAccount}>Cancel account</button>
              <button data-action="refresh-payouts" disabled={!account || busy} onClick={() => refreshPayouts()}>Refresh payouts</button>
            </div>
            {account?.close_stage && (
              <div className="row">
                <span className={`badge s-${account.state === "closed" ? "settled" : "pending"}`}>{account.state}</span>
                <span className="mono">stage: {account.close_stage}</span>
                <span className="mono muted">destination ****{account.destination_last4}</span>
              </div>
            )}
            {payouts.length > 0 && (
              <table>
                <thead><tr><th>Payout</th><th>Amount</th><th>Attempt</th><th>Status</th><th>Bank</th><th>Support</th></tr></thead>
                <tbody>
                  {payouts.map((p) => (
                    <tr key={p.id}>
                      <td className="mono">{p.id.slice(-12)}</td>
                      <td>{formatMinor(p.amount_minor, p.currency)}</td>
                      <td>{p.attempt}</td>
                      <td><span className={`badge s-${p.status === "settled" ? "settled" : p.status === "pending" ? "pending" : "failed"}`}>{p.status}</span></td>
                      <td className="row">
                        <button data-action="bank-fetch-instruction" disabled={busy || p.status !== "pending"} onClick={() => bankPayout("instruction", p)}>Instruction</button>
                        <button data-action="bank-settle-payout" disabled={busy || p.status !== "pending"} onClick={() => bankPayout("settle", p)}>Settle</button>
                        <button data-action="bank-fail-payout" disabled={busy || p.status !== "pending"} onClick={() => bankPayout("fail", p)}>Fail</button>
                      </td>
                      <td><button data-action="retry-payout" disabled={busy || p.status !== "failed"} onClick={() => retryPayout(p)}>Retry</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
          <section className="panel">
            <h2>9 · Audit</h2>
            <div className="muted" style={{ marginBottom: 8 }}>
              Acts as an auditor (read-only role). Every search, verification and export here is itself recorded in the trail. Payloads are already redacted: no account numbers, ciphertexts or tokens.
            </div>
            <div className="row">
              <label>Account<input value={auditFilter.account_id ?? ""} placeholder={account?.id ?? "account id"} onChange={(e) => setAuditFilter({ ...auditFilter, account_id: e.target.value })} /></label>
              <label>Event type<input value={auditFilter.event_type ?? ""} placeholder="account.changed" onChange={(e) => setAuditFilter({ ...auditFilter, event_type: e.target.value })} /></label>
              <label>Actor<input value={auditFilter.actor ?? ""} placeholder={sub} onChange={(e) => setAuditFilter({ ...auditFilter, actor: e.target.value })} /></label>
              <label>From<input type="datetime-local" value={auditFilter.from ?? ""} onChange={(e) => setAuditFilter({ ...auditFilter, from: e.target.value })} /></label>
              <label>To<input type="datetime-local" value={auditFilter.to ?? ""} onChange={(e) => setAuditFilter({ ...auditFilter, to: e.target.value })} /></label>
            </div>
            <div className="row">
              <button data-action="audit-search" className="primary" disabled={busy} onClick={() => auditSearch()}>Search</button>
              <button data-action="audit-verify" disabled={busy} onClick={auditVerify}>Verify chain</button>
              <button data-action="audit-checkpoint" disabled={busy} onClick={auditCheckpoint}>Create checkpoint</button>
              <button data-action="audit-run-reconciliation" disabled={busy} onClick={() => auditReconcile(true)}>Run reconciliation</button>
              <button data-action="audit-last-reconciliation" disabled={busy} onClick={() => auditReconcile(false)}>Last reconciliation</button>
              <button data-action="audit-snapshot" disabled={busy} onClick={auditSnapshot}>Snapshot now</button>
              <button data-action="audit-replay" disabled={busy || !(auditFilter.account_id?.trim() || account)} onClick={auditReplay}>Replay account</button>
              <button data-action="audit-export" disabled={busy} onClick={auditCreateExport}>Export</button>
              <button data-action="audit-download-export" disabled={busy || !auditExport} onClick={auditDownload}>Download export</button>
            </div>
            {auditStatus && (
              <div className="row">
                <span className={`badge s-${auditStatus.ok ? "settled" : "failed"}`}>{auditStatus.label}</span>
                <span className="mono">{auditStatus.detail}</span>
              </div>
            )}
            {auditExport && <div className="row mono muted">export {auditExport.id.slice(0, 8)} · {auditExport.count} events · sha256 {auditExport.sha256.slice(0, 16)}…</div>}
            {auditEvents.length > 0 && (
              <table>
                <thead><tr><th>#</th><th>When</th><th>Event</th><th>Aggregate</th><th>Actor</th><th>Source</th><th>Hash</th></tr></thead>
                <tbody>
                  {auditEvents.map((e) => (
                    <tr key={e.id} title={JSON.stringify(e.after ?? {})}>
                      <td className="mono">{e.chain_seq}</td>
                      <td className="mono">{new Date(e.occurred_at).toLocaleString()}</td>
                      <td>{e.event_type}{e.redacted && <span className="badge s-pending" style={{ marginLeft: 6 }}>redacted</span>}</td>
                      <td className="mono">{e.aggregate_type}:{e.aggregate_id.slice(-12)}</td>
                      <td className="mono">{e.actor ?? ""}</td>
                      <td className="mono">{e.source ? `${e.source}#${e.source_seq}` : ""}</td>
                      <td className="mono muted">{e.hash.slice(0, 10)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {auditNext && <div className="row"><button disabled={busy} onClick={() => auditSearch(auditNext)}>Load more</button></div>}
          </section>
          <section className="panel">
            <h2>10 · Notifications</h2>
            <div className="muted" style={{ marginBottom: 8 }}>
              Push, SMS and email are simulated: what they &quot;send&quot; shows up as a popup at the bottom right. Every channel is on by default; past due, card blocked and suspected fraud keep at least one channel. Partners (support) get signed webhooks of the events they subscribe to.
            </div>
            <div className="row">
              <button data-action="refresh-notification-preferences" disabled={!account || busy} onClick={refreshPrefs}>Preferences</button>
              <button data-action="refresh-notifications" disabled={!account || busy} onClick={refreshInbox}>Inbox ({inbox.length})</button>
            </div>
            {prefs.length > 0 && (
              <table>
                <thead><tr><th>Notification</th><th>Push</th><th>SMS</th><th>Email</th></tr></thead>
                <tbody>
                  {prefs.map((p) => (
                    <tr key={p.type}>
                      <td>{p.type}{p.mandatory && <span className="badge s-pending" style={{ marginLeft: 6 }} title="mandatory: at least one channel stays on">🔒 mandatory</span>}</td>
                      {(["push", "sms", "email"] as const).map((ch) => (
                        <td key={ch}><input type="checkbox" data-action="set-notification-preference" checked={p[ch]} disabled={busy}
                          onChange={(e) => setPref(p, ch, e.target.checked)} /></td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {inbox.length > 0 && (
              <table>
                <thead><tr><th></th><th>When</th><th>Type</th><th>Text</th></tr></thead>
                <tbody>
                  {inbox.slice(0, 15).map((it) => (
                    <tr key={it.id}><td>{channelIcon(it.channel)}</td><td className="mono">{new Date(it.created_at).toLocaleTimeString()}</td><td>{it.type}</td><td>{it.text}</td></tr>
                  ))}
                </tbody>
              </table>
            )}
            <div className="row" style={{ marginTop: 8 }}>
              <label className="inline">Partner events <input value={partnerTypes} onChange={(e) => setPartnerTypes(e.target.value)} style={{ width: 360 }} /></label>
              <button data-action="register-demo-partner" disabled={busy} onClick={registerPartner}>Register demo partner</button>
              <button data-action="list-partners" disabled={busy} onClick={() => listPartners()}>Partners</button>
            </div>
            {partners.length > 0 && (
              <table>
                <thead><tr><th>Partner</th><th>Events</th><th>Status</th><th></th></tr></thead>
                <tbody>
                  {partners.map((p) => (
                    <tr key={p.id}>
                      <td className="mono">{p.name}</td>
                      <td className="mono">{p.event_types.join(", ")}</td>
                      <td><span className={`badge s-${p.active ? "settled" : "failed"}`}>{p.active ? "active" : "disabled"}</span></td>
                      <td className="row">
                        <button data-action="partner-deliveries" disabled={busy} onClick={() => partnerDeliveriesFor(p)}>Deliveries</button>
                        <button data-action="disable-partner" disabled={busy || !p.active} onClick={() => disablePartner(p)}>Disable</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {Object.entries(partnerDeliveries).map(([id, ds]) => (
              <div key={id} className="mono muted" style={{ marginTop: 6 }}>
                {ds.length === 0 ? "no deliveries yet" : ds.slice(0, 10).map((d) => `${d.event_type} · ${d.status} · ${d.attempts} attempt(s)${d.last_error ? ` · ${d.last_error}` : ""}`).join("\n")}
              </div>
            ))}
            {partnerEvents.length > 0 && (
              <div className="mono" style={{ marginTop: 6 }}>
                {partnerEvents.slice(0, 10).map((e) => (
                  <div key={e.delivery_id}>{e.verified ? "✅ signature ok" : "❌ bad signature"} · {e.event_type} · {new Date(e.received_at).toLocaleTimeString()}</div>
                ))}
              </div>
            )}
          </section>
        </div>

        <section className="panel log">
          <h2>Request log</h2>
          {log.length === 0 && <div className="muted">Calls appear here with status, replay flag and latency.</div>}
          {log.map((e) => <LogRow key={e.id} e={e} />)}
        </section>
      </div>
      <div style={{ position: "fixed", right: 16, bottom: 16, display: "flex", flexDirection: "column", gap: 8, zIndex: 50, maxWidth: 360 }} aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className="panel" data-testid="notification-popup" style={{ padding: "10px 12px", boxShadow: "0 4px 16px rgba(0,0,0,.18)" }}>
            <b>{channelIcon(t.channel)} {t.channel}</b> <span className="muted">{t.type}</span>
            <div>{t.text}</div>
          </div>
        ))}
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
