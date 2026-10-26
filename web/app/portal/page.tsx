"use client";

// Cardholder portal: the customer's view of the card (buy, see movements, pay, freeze, statements,
// close), plus a simulation rail for what the bank, the merchants and the clock do. Uses the same
// server-side signing APIs as the engineering simulator at /.
import { useCallback, useEffect, useRef, useState } from "react";
import { daysToDuePlus, daysToNextClose } from "@/lib/billing.ts";
import {
  ACCOUNT_STATES, CHARGE_KINDS, CLOSE_STAGES, DEPOSIT_STATES, MERCHANTS, PAYMENT_STATES, TX_STATES,
  declineText, errorText, money, parseMoney, parseSession, tone, txActions,
  type Merchant, type SavedDeposit, type Session,
} from "@/lib/portal.ts";

type CallResult = { status: number; replayed: boolean; ms: number; body: any };
type Account = {
  id: string; customer_id: string; currency: string; state: string; frozen: boolean;
  credit_limit_minor: number; available_credit_minor: number; collateral_minor: number; purchase_limit_minor: number;
  outstanding_minor: number; credit_balance_minor: number; restricted_reason?: string;
  account_time?: string; activated_at?: string; delinquency?: string; close_stage?: string; destination_last4?: string;
};
type Tx = {
  id: string; network_ref: string; amount_minor: number; currency: string; status: string; decline_reason?: string;
  captured_amount_minor: number; refunded_amount_minor: number; merchant: { id: string; name: string; mcc: string };
  created_at?: string; expires_at?: string;
};
type Payment = { id: string; amount_minor: number; currency: string; status: string; created_at?: string; settled_at?: string };
type Payout = { id: string; amount_minor: number; currency: string; status: string; attempt: number; attempt_id: string; destination_last4: string };
type Summary = {
  account_time: string; cycle_start?: string; cycle_end?: string; minimum_due_minor: number; due_date?: string;
  past_due_minor: number; dpd: number; grace: boolean; stage: string; currency: string;
};
type Statement = {
  id: string; cycle: number; currency: string; period_start: string; period_end: string; due_date?: string;
  previous_balance_minor: number; purchases_minor: number; refunds_minor: number; payments_minor: number; returns_minor: number;
  interest_minor: number; fees_minor: number; waivers_minor: number; collateral_applied_minor: number;
  new_balance_minor: number; minimum_due_minor: number;
};
type Charge = { id: string; kind: string; cycle: number; amount_minor: number; waived_minor: number; effective_at?: string; status: string };
type Toast = { id: number; tone: "ok" | "err" | "warn"; text: string };
type Activity = { id: number; at: string; who: "tú" | "banco" | "comercio" | "reloj" | "soporte"; text: string; tone: "ok" | "err" | "warn" | "muted" };
type Tab = "inicio" | "comprar" | "movimientos" | "pagos" | "extractos" | "ajustes";

const STORAGE_KEY = "acme-portal-session";
const TABS: { id: Tab; label: string }[] = [
  { id: "inicio", label: "Inicio" },
  { id: "comprar", label: "Comprar" },
  { id: "movimientos", label: "Movimientos" },
  { id: "pagos", label: "Pagar" },
  { id: "extractos", label: "Extractos" },
  { id: "ajustes", label: "Ajustes" },
];

const day = (iso?: string) => (iso ? new Date(iso).toLocaleDateString("es", { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" }) : "—");
const newSub = () => `cust-${Math.random().toString(36).slice(2, 8)}`;
let seq = 0;

function loadSession(): Session | null {
  try {
    return parseSession(localStorage.getItem(STORAGE_KEY));
  } catch {
    return null;
  }
}

function saveSession(s: Session | null) {
  try {
    if (s) localStorage.setItem(STORAGE_KEY, JSON.stringify(s));
    else localStorage.removeItem(STORAGE_KEY);
  } catch {}
}

export default function Portal() {
  const [session, setSession] = useState<Session | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [token, setToken] = useState("");
  const [account, setAccount] = useState<Account | null>(null);
  const [txs, setTxs] = useState<Tx[]>([]);
  const [payments, setPayments] = useState<Payment[]>([]);
  const [payouts, setPayouts] = useState<Payout[]>([]);
  const [summary, setSummary] = useState<Summary | null>(null);
  const [statements, setStatements] = useState<Statement[]>([]);
  const [charges, setCharges] = useState<Charge[]>([]);
  const [tab, setTab] = useState<Tab>("inicio");
  const [busy, setBusy] = useState(false);
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [activity, setActivity] = useState<Activity[]>([]);
  const [autoCapture, setAutoCapture] = useState(true);
  const supportToken = useRef("");

  // ---------- plumbing ----------

  const toast = useCallback((t: Toast["tone"], text: string) => {
    const id = ++seq;
    setToasts((l) => [...l, { id, tone: t, text }]);
    setTimeout(() => setToasts((l) => l.filter((x) => x.id !== id)), 5000);
  }, []);

  const log = useCallback((who: Activity["who"], text: string, t: Activity["tone"] = "muted") => {
    setActivity((l) => [{ id: ++seq, at: new Date().toLocaleTimeString("es"), who, text, tone: t }, ...l].slice(0, 100));
  }, []);

  function persist(s: Session | null) {
    setSession(s);
    saveSession(s);
  }

  async function mint(sub: string, accounts: string[], role?: string): Promise<string> {
    const res = await fetch("/api/token", { method: "POST", body: JSON.stringify({ sub, accounts, role }) });
    const j = await res.json();
    if (!j.token) throw new Error(j.error?.message ?? `token ${res.status}`);
    return j.token as string;
  }

  async function call(method: string, path: string, body?: object, tok = token): Promise<CallResult> {
    const headers: Record<string, string> = {};
    if (tok) headers.authorization = `Bearer ${tok}`;
    if (method !== "GET") headers["idempotency-key"] = crypto.randomUUID();
    const res = await fetch(`/api/gateway/${path.replace(/^\//, "")}`, { method, headers, body: body ? JSON.stringify(body) : undefined });
    if (!res.ok) return { status: res.status, replayed: false, ms: 0, body: await res.json().catch(() => "") };
    return ((await res.json()) as CallResult[])[0];
  }

  async function signed(route: string, payload: object): Promise<CallResult> {
    const res = await fetch(route, { method: "POST", body: JSON.stringify(payload) });
    if (!res.ok) return { status: res.status, replayed: false, ms: 0, body: await res.json().catch(() => "") };
    return ((await res.json()) as CallResult[])[0];
  }

  async function run(fn: () => Promise<void>) {
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      toast("err", String(e instanceof Error ? e.message : e));
    } finally {
      setBusy(false);
    }
  }

  // The gateway rate-limits each customer (RATE_LIMIT_PER_MINUTE, 60 in dev), so a reload fetches
  // only what an action can change: statements and charges move with the clock, payouts with a close.
  const reload = useCallback(async (acc: { id: string } | null = account, tok = token, full = false) => {
    if (!acc || !tok) return;
    const get = (p: string) => call("GET", `/v1/accounts/${acc.id}${p}`, undefined, tok);
    const skip = Promise.resolve(null);
    const [a, t, p, b, s, c] = await Promise.all([
      get(""), get("/transactions?page_size=50"), get("/payments?page_size=50"), get("/billing"),
      full ? get("/statements") : skip, full ? get("/charges") : skip,
    ]);
    const closing = a.status === 200 && (a.body.state === "closing" || a.body.state === "closed");
    const o = closing ? await get("/payouts") : null;
    const all = [a, t, p, b, s, c, o].filter((r): r is CallResult => r !== null);
    if (all.some((r) => r.status === 429)) toast("warn", "El gateway limita a 60 solicitudes por minuto por cliente; espera unos segundos y pulsa ↻ Actualizar.");
    if (a.status === 200) setAccount(a.body);
    else if (a.status === 401 || a.status === 403 || a.status === 404) toast("err", `No se pudo cargar la cuenta: ${errorText(a)}`);
    if (t.status === 200) setTxs(t.body.transactions ?? []);
    if (p.status === 200) setPayments(p.body.payments ?? []);
    if (b.status === 200) setSummary(b.body);
    if (s?.status === 200) setStatements(s.body.statements ?? []);
    if (c?.status === 200) setCharges(c.body.charges ?? []);
    if (o?.status === 200) setPayouts(o.body.payouts ?? []);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [account, token]);

  // Restore the saved customer on first load.
  useEffect(() => {
    const s = loadSession();
    setLoaded(true);
    if (!s) return;
    setSession(s);
    mint(s.sub, s.accountId ? [s.accountId] : [])
      .then(async (tok) => {
        setToken(tok);
        if (s.accountId) await reload({ id: s.accountId }, tok, true);
      })
      .catch((e) => toast("err", `No se pudo iniciar sesión: ${e.message}. ¿Está corriendo el stack con SIMULATOR_ENABLED=1?`));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // ---------- customer: onboarding ----------

  async function start(name: string, sub: string, accountId = "") {
    const tok = await mint(sub, accountId ? [accountId] : []);
    setToken(tok);
    const s: Session = { sub, name, accountId, deposits: [] };
    persist(s);
    setTab("inicio");
    setActivity([]);
    if (accountId) {
      await reload({ id: accountId }, tok, true);
      log("tú", `Entraste como ${sub}`);
    } else {
      setAccount(null);
      log("tú", `Cliente ${sub} creado`);
    }
  }

  async function openAccount(currency = "USD") {
    if (!session) return;
    const r = await call("POST", "/v1/accounts", { currency });
    if (r.status !== 201) return toast("err", errorText(r));
    const tok = await mint(session.sub, [r.body.id]); // the token must list the new account
    setToken(tok);
    setAccount(r.body);
    persist({ ...session, accountId: r.body.id });
    log("tú", "Abriste tu cuenta. Falta el depósito de garantía.", "ok");
  }

  async function deposit(amount: number) {
    if (!session || !account) return;
    const r = await call("POST", `/v1/accounts/${account.id}/collateral/deposits`, { amount_minor: amount });
    if (r.status !== 201) return toast("err", errorText(r));
    const d: SavedDeposit = { id: r.body.id, amount_minor: r.body.amount_minor, currency: r.body.currency, status: r.body.status };
    persist({ ...session, deposits: [d, ...session.deposits.filter((x) => x.id !== d.id)] });
    log("tú", `Enviaste un depósito de garantía de ${money(amount, account.currency)}`, "warn");
    toast("ok", "Depósito enviado. Tu banco debe confirmarlo (panel de simulación → Banco).");
  }

  async function quickCustomer() {
    const sub = newSub();
    const tok0 = await mint(sub, []);
    const a = await call("POST", "/v1/accounts", { currency: "USD" }, tok0);
    if (a.status !== 201) return toast("err", errorText(a));
    const tok = await mint(sub, [a.body.id]);
    const d = await call("POST", `/v1/accounts/${a.body.id}/collateral/deposits`, { amount_minor: 50000 }, tok);
    if (d.status !== 201) return toast("err", errorText(d));
    const b = await signed("/api/bank/webhook", { deposit_id: d.body.id, external_ref: `bank-${d.body.id.slice(-8)}`, amount_minor: 50000, currency: "USD", status: "settled" });
    if (b.status !== 200) return toast("err", `Banco: ${errorText(b)}`);
    setToken(tok);
    persist({ sub, name: "Cliente de prueba", accountId: a.body.id, deposits: [{ ...b.body.deposit }] });
    setActivity([]);
    setTab("inicio");
    log("banco", "Depósito de $500.00 confirmado. Tarjeta activa.", "ok");
    await reload({ id: a.body.id }, tok);
    toast("ok", "Cliente listo con $500.00 de cupo.");
  }

  async function cancelApplication() {
    if (!account) return;
    const r = await call("POST", `/v1/accounts/${account.id}/cancel`);
    if (r.status !== 200) return toast("err", errorText(r));
    setAccount(r.body);
    log("tú", "Cancelaste la solicitud de tarjeta", "err");
  }

  function logout() {
    persist(null);
    setToken("");
    setAccount(null);
    setTxs([]); setPayments([]); setPayouts([]); setSummary(null); setStatements([]); setCharges([]);
    setActivity([]);
  }

  // ---------- customer: card ----------

  async function buy(m: Pick<Merchant, "id" | "name" | "mcc">, amount: number, currency: string) {
    if (!account) return;
    const r = await signed("/api/network", {
      op: "authorize",
      payload: { network_ref: `n-${crypto.randomUUID()}`, card_id: account.id, amount_minor: amount, currency, merchant: { id: m.id, name: m.name, mcc: m.mcc } },
    });
    if (r.status !== 201) {
      toast("err", errorText(r));
    } else if (r.body.status === "declined") {
      toast("err", `Compra rechazada: ${declineText(r.body.decline_reason ?? "")}`);
      log("tú", `Compra en ${m.name} por ${money(amount, currency)} rechazada: ${declineText(r.body.decline_reason ?? "")}`, "err");
    } else {
      toast("ok", `Compra aprobada en ${m.name} por ${money(amount, currency)}`);
      log("tú", `Compra aprobada en ${m.name} por ${money(amount, currency)}`, "ok");
      if (autoCapture && r.body.status === "approved") await merchant("capture", r.body as Tx, amount);
    }
    await reload();
  }

  async function freeze(on: boolean) {
    if (!account) return;
    const r = await call("POST", `/v1/accounts/${account.id}/${on ? "freeze" : "unfreeze"}`);
    if (r.status !== 200) return toast("err", errorText(r));
    setAccount(r.body);
    log("tú", on ? "Congelaste tu tarjeta" : "Descongelaste tu tarjeta", on ? "warn" : "ok");
    toast("ok", on ? "Tarjeta congelada: se rechazarán compras nuevas." : "Tarjeta activa de nuevo.");
  }

  async function setLimit(amount: number) {
    if (!account) return;
    const r = await call("PUT", `/v1/accounts/${account.id}/limits`, { purchase_limit_minor: amount });
    if (r.status !== 200) return toast("err", errorText(r));
    setAccount(r.body);
    log("tú", amount ? `Límite por compra: ${money(amount, account.currency)}` : "Quitaste el límite por compra");
    toast("ok", "Límite actualizado.");
  }

  async function pay(amount: number) {
    if (!account) return;
    const r = await call("POST", `/v1/accounts/${account.id}/payments`, { amount_minor: amount });
    if (r.status !== 201) return toast("err", errorText(r));
    setAccount(r.body.account);
    log("tú", `Iniciaste un pago de ${money(amount, account.currency)}`, "warn");
    toast("ok", "Pago enviado. El cupo se libera cuando tu banco lo acredite.");
    await reload();
  }

  async function closeAccount(destination: { bank_code: string; account_number: string; holder_name: string }) {
    if (!account) return;
    const r = await call("POST", `/v1/accounts/${account.id}/close`, { destination });
    if (r.status !== 202) return toast("err", errorText(r));
    setAccount(r.body);
    log("tú", `Pediste el cierre. El saldo irá a la cuenta ****${destination.account_number.slice(-4)}`, "warn");
    toast("ok", "Cierre solicitado. Avanza el reloj para ver cada etapa.");
    await reload();
  }

  // ---------- simulation: merchant, bank, clock, support ----------

  async function merchant(op: "capture" | "reversal" | "refunds", t: Tx, amount?: number) {
    const payload =
      op === "capture" ? { network_ref: `c-${t.id}`, amount_minor: amount ?? t.amount_minor }
      : op === "reversal" ? { network_ref: `v-${t.id}` }
      : { network_ref: `r-${crypto.randomUUID()}`, amount_minor: amount };
    const r = await signed("/api/network", { op, authId: t.id, payload });
    const ok = r.status === 200 || r.status === 201;
    const what = op === "capture" ? `cobró ${money(amount ?? t.amount_minor, t.currency)}` : op === "reversal" ? "anuló la compra" : `devolvió ${money(amount ?? 0, t.currency)}`;
    if (!ok) {
      toast("err", `${t.merchant.name}: ${errorText(r)}`);
      return;
    }
    log("comercio", `${t.merchant.name} ${what}`, op === "capture" ? "muted" : "ok");
  }

  async function bankDeposit(d: SavedDeposit, status: "settled" | "failed") {
    if (!session) return;
    const r = await signed("/api/bank/webhook", { deposit_id: d.id, external_ref: `bank-${d.id.slice(-8)}`, amount_minor: d.amount_minor, currency: d.currency, status });
    if (r.status !== 200) return toast("err", `Banco: ${errorText(r)}`);
    persist({ ...session, deposits: session.deposits.map((x) => (x.id === d.id ? { ...x, status: r.body.deposit.status } : x)) });
    setAccount(r.body.account);
    log("banco", status === "settled" ? `Confirmó tu depósito de ${money(d.amount_minor, d.currency)}` : "Rechazó tu depósito", status === "settled" ? "ok" : "err");
    await reload();
  }

  async function bankPayment(op: "settle" | "fail" | "return", p: Payment) {
    const ref = op === "return" ? `ret-${p.id.slice(-8)}` : `bank-${p.id.slice(-8)}`;
    const r = await signed("/api/bank/payments", { op, payment: p, ref });
    if (r.status !== 200) return toast("err", `Banco: ${errorText(r)}`);
    const text = { settle: `Acreditó tu pago de ${money(p.amount_minor, p.currency)}`, fail: "Rechazó tu pago", return: `Devolvió tu pago de ${money(p.amount_minor, p.currency)} (la deuda vuelve)` }[op];
    log("banco", text, op === "settle" ? "ok" : "err");
    await reload();
  }

  async function bankPayout(op: "settle" | "fail", p: Payout) {
    const r = await signed("/api/bank/payouts", { op, attempt_id: p.attempt_id, amount_minor: p.amount_minor, currency: p.currency, last4: p.destination_last4 });
    if (r.status !== 200) return toast("err", `Banco: ${errorText(r)}`);
    log("banco", op === "settle" ? `Depositó ${money(p.amount_minor, p.currency)} en tu cuenta ****${p.destination_last4}` : "Rechazó la transferencia del saldo", op === "settle" ? "ok" : "err");
    await reload();
  }

  async function support(path: string, body: object | undefined, text: string) {
    if (!supportToken.current) supportToken.current = await mint("agent-portal", [], "support");
    const r = await call("POST", path, body, supportToken.current);
    if (r.status >= 300) return toast("err", `Soporte: ${errorText(r)}`);
    log("soporte", text, "ok");
    await reload(account, token, true);
  }

  async function advance(days: number | null, label: string) {
    if (!account || !days) return;
    const r = await call("POST", `/v1/dev/accounts/${account.id}/clock/advance`, { days });
    if (r.status !== 200) return toast("err", `Reloj: ${errorText(r)}`);
    log("reloj", `+${days} día${days === 1 ? "" : "s"} (${label})`);
    await reload(account, token, true);
  }

  // ---------- render ----------

  if (!loaded) return <div className="portal" />;

  return (
    <div className="portal">
      <header className="p-top">
        <div className="brand"><span className="logo">◆</span> acme<b>card</b> <span className="env">entorno de pruebas</span></div>
        <nav className="p-links">
          <a href="/">Simulador técnico ↗</a>
          {session && <button className="link" onClick={logout}>Salir</button>}
        </nav>
      </header>

      <div className="p-layout">
        <main className="p-main">
          {!session ? (
            <Welcome busy={busy} onStart={(n, s, a) => run(() => start(n, s, a))} onQuick={() => run(quickCustomer)} />
          ) : !account ? (
            <section className="card-panel center">
              <h1>Hola{session.name ? `, ${session.name}` : ""}</h1>
              <p className="muted">Aún no tienes tarjeta. Abre tu cuenta asegurada: tu cupo será igual al depósito de garantía que confirme tu banco.</p>
              <button className="btn primary" disabled={busy || !token} onClick={() => run(() => openAccount())}>Abrir mi cuenta (USD)</button>
            </section>
          ) : account.state === "pending_collateral" || account.state === "cancelled" ? (
            <Onboarding account={account} deposits={session.deposits} busy={busy}
              onDeposit={(a) => run(() => deposit(a))} onCancel={() => run(cancelApplication)} />
          ) : (
            <>
              <nav className="tabs" role="tablist">
                {TABS.map((t) => (
                  <button key={t.id} role="tab" aria-selected={tab === t.id} className={tab === t.id ? "on" : ""} onClick={() => setTab(t.id)}>{t.label}</button>
                ))}
              </nav>
              {tab === "inicio" && <Home account={account} name={session.name} summary={summary} txs={txs} busy={busy}
                onFreeze={(on) => run(() => freeze(on))} go={setTab} />}
              {tab === "comprar" && <Shop account={account} busy={busy} onBuy={(m, a, c) => run(() => buy(m, a, c))} />}
              {tab === "movimientos" && <Movements txs={txs} payments={payments} />}
              {tab === "pagos" && <Pay account={account} summary={summary} payments={payments} busy={busy} onPay={(a) => run(() => pay(a))} />}
              {tab === "extractos" && <Statements account={account} summary={summary} statements={statements} charges={charges} token={token} />}
              {tab === "ajustes" && <Settings account={account} payouts={payouts} busy={busy} session={session}
                onFreeze={(on) => run(() => freeze(on))} onLimit={(a) => run(() => setLimit(a))} onClose={(d) => run(() => closeAccount(d))} />}
            </>
          )}
        </main>

        {session && account && (
          <SimRail account={account} session={session} txs={txs} payments={payments} payouts={payouts} charges={charges} summary={summary}
            activity={activity} busy={busy} autoCapture={autoCapture} setAutoCapture={setAutoCapture}
            onMerchant={(op, t, a) => run(async () => { await merchant(op, t, a); await reload(); })}
            onDeposit={(d, s) => run(() => bankDeposit(d, s))}
            onPayment={(op, p) => run(() => bankPayment(op, p))}
            onPayout={(op, p) => run(() => bankPayout(op, p))}
            onRetryPayout={(p) => run(() => support(`/v1/support/accounts/${account.id}/payouts/${p.id}/retry`, undefined, "Reintentó la transferencia del saldo"))}
            onWaive={(c) => run(() => support(`/v1/support/accounts/${account.id}/charges/${c.id}/waivers`,
              { amount_minor: c.amount_minor - c.waived_minor, reason: "goodwill" }, `Condonó ${CHARGE_KINDS[c.kind] ?? c.kind} por ${money(c.amount_minor - c.waived_minor, account.currency)}`))}
            onAdvance={(d, l) => run(() => advance(d, l))}
            onRefresh={() => run(() => reload(account, token, true))} />
        )}
      </div>

      <div className="toasts" aria-live="polite">
        {toasts.map((t) => <div key={t.id} className={`toast t-${t.tone}`}>{t.text}</div>)}
      </div>
    </div>
  );
}

// ---------- screens ----------

function Welcome({ busy, onStart, onQuick }: { busy: boolean; onStart: (name: string, sub: string, accountId?: string) => void; onQuick: () => void }) {
  const [name, setName] = useState("");
  const [sub, setSub] = useState("");
  const [acc, setAcc] = useState("");
  return (
    <section className="card-panel">
      <h1>Portal del tarjetahabiente</h1>
      <p className="muted">Entorno de pruebas: actúas como cliente de la tarjeta asegurada. A la derecha simulas lo que hacen el banco, los comercios y el paso del tiempo.</p>
      <div className="welcome-grid">
        <div className="box">
          <h3>Atajo</h3>
          <p className="muted">Cliente nuevo con cuenta abierta, $500.00 de garantía confirmada y tarjeta activa.</p>
          <button className="btn primary" disabled={busy} onClick={onQuick}>Crear cliente listo para comprar</button>
        </div>
        <div className="box">
          <h3>Cliente nuevo</h3>
          <label>Tu nombre<input value={name} onChange={(e) => setName(e.target.value)} placeholder="Ana Pérez" /></label>
          <button className="btn" disabled={busy} onClick={() => onStart(name.trim(), newSub())}>Empezar desde cero</button>
        </div>
        <div className="box">
          <h3>Cliente existente</h3>
          <label>ID de cliente (sub)<input value={sub} onChange={(e) => setSub(e.target.value)} placeholder="cust-1a2b3c" /></label>
          <label>ID de cuenta<input value={acc} onChange={(e) => setAcc(e.target.value)} placeholder="uuid" /></label>
          <button className="btn" disabled={busy || !sub.trim() || !acc.trim()} onClick={() => onStart("", sub.trim(), acc.trim())}>Entrar</button>
        </div>
      </div>
    </section>
  );
}

function Onboarding({ account, deposits, busy, onDeposit, onCancel }: {
  account: Account; deposits: SavedDeposit[]; busy: boolean; onDeposit: (a: number) => void; onCancel: () => void;
}) {
  const [amount, setAmount] = useState("500");
  const minor = parseMoney(amount);
  if (account.state === "cancelled")
    return (
      <section className="card-panel center">
        <h1>Solicitud cancelada</h1>
        <p className="muted">Esta cuenta se canceló antes de activarse. Sal y crea otro cliente para empezar de nuevo.</p>
      </section>
    );
  const pending = deposits.some((d) => d.status === "pending");
  return (
    <section className="card-panel">
      <ol className="steps">
        <li className="done">Cuenta abierta</li>
        <li className={pending ? "done" : "now"}>Depósito de garantía</li>
        <li className={pending ? "now" : ""}>Confirmación del banco</li>
        <li>Tarjeta activa</li>
      </ol>
      <h2>Deposita tu garantía</h2>
      <p className="muted">Tu cupo será igual al monto que confirme el banco. Puedes hacer varios depósitos.</p>
      <div className="row">
        <label>Monto (USD)<input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} /></label>
        <button className="btn primary" disabled={busy || !minor} onClick={() => minor && onDeposit(minor)}>Depositar {minor ? money(minor, account.currency) : ""}</button>
      </div>
      {deposits.length > 0 && (
        <ul className="list">
          {deposits.map((d) => (
            <li key={d.id}><span>Depósito {money(d.amount_minor, d.currency)}</span><Badge status={d.status} labels={DEPOSIT_STATES} /></li>
          ))}
        </ul>
      )}
      {pending && <p className="hint">👉 En el panel de simulación, <b>Banco → Confirmar</b> activa la tarjeta.</p>}
      <hr />
      <button className="btn ghost danger" disabled={busy} onClick={onCancel}>Cancelar solicitud</button>
    </section>
  );
}

function CardFace({ account, name }: { account: Account; name: string }) {
  return (
    <div className={`cardface ${account.frozen ? "frozen" : ""} ${account.state !== "active" ? "inactive" : ""}`}>
      <div className="cf-top"><span>acme<b>card</b></span><span className="cf-chip" /></div>
      <div className="cf-num">•••• •••• •••• {account.id.replace(/-/g, "").slice(-4).toUpperCase()}</div>
      <div className="cf-bottom">
        <span>{name || account.customer_id}</span>
        <span>{account.frozen ? "❄ CONGELADA" : ACCOUNT_STATES[account.state]?.toUpperCase()}</span>
      </div>
    </div>
  );
}

function Alerts({ account, summary }: { account: Account; summary: Summary | null }) {
  const items: { tone: "err" | "warn"; text: string }[] = [];
  if (account.state === "closing") items.push({ tone: "warn", text: `Cierre en curso: ${CLOSE_STAGES[account.close_stage ?? ""] ?? account.close_stage}.` });
  if (account.state === "closed") items.push({ tone: "warn", text: "Tu cuenta está cerrada." });
  if (account.restricted_reason) items.push({ tone: "err", text: declineText(account.restricted_reason) });
  if (account.delinquency === "past_due") items.push({ tone: "err", text: `Pago vencido${summary ? `: ${money(summary.past_due_minor, account.currency)}, ${summary.dpd} días de mora` : ""}.` });
  if (account.delinquency === "charged_off") items.push({ tone: "err", text: "Tu deuda fue castigada." });
  if (account.frozen) items.push({ tone: "warn", text: "Tu tarjeta está congelada; las compras nuevas se rechazan." });
  if (!items.length) return null;
  return <div className="alerts">{items.map((a, i) => <div key={i} className={`alert a-${a.tone}`}>{a.text}</div>)}</div>;
}

function Home({ account, name, summary, txs, busy, onFreeze, go }: {
  account: Account; name: string; summary: Summary | null; txs: Tx[]; busy: boolean; onFreeze: (on: boolean) => void; go: (t: Tab) => void;
}) {
  const c = account.currency;
  const used = account.credit_limit_minor ? Math.min(100, Math.round(((account.credit_limit_minor - account.available_credit_minor) / account.credit_limit_minor) * 100)) : 0;
  return (
    <section className="stack">
      <Alerts account={account} summary={summary} />
      <div className="home-grid">
        <CardFace account={account} name={name} />
        <div className="card-panel">
          <div className="muted small">Cupo disponible</div>
          <div className="big">{money(account.available_credit_minor, c)}</div>
          <div className="meter" aria-label={`Usado ${used}%`}><span style={{ width: `${used}%` }} /></div>
          <div className="kv"><span>Cupo total</span><b>{money(account.credit_limit_minor, c)}</b></div>
          <div className="kv"><span>Deuda actual</span><b>{money(account.outstanding_minor, c)}</b></div>
          {account.credit_balance_minor > 0 && <div className="kv"><span>Saldo a favor</span><b>{money(account.credit_balance_minor, c)}</b></div>}
          <div className="kv"><span>Garantía</span><b>{money(account.collateral_minor, c)}</b></div>
        </div>
      </div>
      <div className="actions">
        <button className="btn primary" disabled={account.state !== "active"} onClick={() => go("comprar")}>🛍 Comprar</button>
        <button className="btn" disabled={account.state === "closed"} onClick={() => go("pagos")}>💳 Pagar</button>
        <button className={`btn ${account.frozen ? "primary" : ""}`} disabled={busy || account.state !== "active"} onClick={() => onFreeze(!account.frozen)}>
          {account.frozen ? "🔓 Descongelar" : "❄ Congelar tarjeta"}
        </button>
      </div>
      {summary?.due_date && account.state !== "closed" && (
        <div className="card-panel row-between">
          <div><div className="muted small">Pago mínimo</div><b>{money(summary.minimum_due_minor, c)}</b></div>
          <div><div className="muted small">Fecha límite</div><b>{day(summary.due_date)}</b></div>
          <div><div className="muted small">Próximo corte</div><b>{day(summary.cycle_end)}</b></div>
        </div>
      )}
      <div className="card-panel">
        <div className="row-between"><h3>Últimos movimientos</h3><button className="link" onClick={() => go("movimientos")}>Ver todos</button></div>
        <TxList txs={txs.slice(0, 5)} />
      </div>
    </section>
  );
}

function Shop({ account, busy, onBuy }: { account: Account; busy: boolean; onBuy: (m: Pick<Merchant, "id" | "name" | "mcc">, amount: number, currency: string) => void }) {
  const [selected, setSelected] = useState<Merchant | null>(null);
  const [amount, setAmount] = useState("");
  const [currency, setCurrency] = useState(account.currency);
  const [custom, setCustom] = useState({ name: "Mi Tienda", mcc: "5999" });
  const minor = parseMoney(amount);
  const pick = (m: Merchant) => { setSelected(m); setAmount((m.price / 100).toFixed(2)); };
  const merchant = selected ?? { id: `m-${custom.name.toLowerCase().replace(/[^a-z0-9]+/g, "-").slice(0, 40) || "tienda"}`, name: custom.name, mcc: custom.mcc, category: "Otro", price: 0, emoji: "🏬" };
  return (
    <section className="stack">
      <div className="card-panel">
        <h3>Elige un comercio</h3>
        <div className="shops">
          {MERCHANTS.map((m) => (
            <button key={m.id} className={`shop ${selected?.id === m.id ? "on" : ""}`} onClick={() => pick(m)}>
              <span className="emoji">{m.emoji}</span><b>{m.name}</b><span className="muted small">{m.category} · {money(m.price, "USD")}</span>
            </button>
          ))}
          <button className={`shop ${selected === null ? "on" : ""}`} onClick={() => { setSelected(null); setAmount(""); }}>
            <span className="emoji">🏬</span><b>Otro comercio</b><span className="muted small">nombre y MCC libres</span>
          </button>
        </div>
      </div>
      <div className="card-panel">
        <h3>Pagar en {merchant.name}</h3>
        {!selected && (
          <div className="row">
            <label>Comercio<input value={custom.name} maxLength={128} onChange={(e) => setCustom({ ...custom, name: e.target.value })} /></label>
            <label>MCC<input value={custom.mcc} maxLength={4} style={{ width: 80 }} onChange={(e) => setCustom({ ...custom, mcc: e.target.value })} /></label>
          </div>
        )}
        <div className="row">
          <label>Monto<input inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="0.00" /></label>
          <label>Moneda
            <select value={currency} onChange={(e) => setCurrency(e.target.value)}>
              {["USD", "EUR", "COP", "MXN"].map((c) => <option key={c}>{c}</option>)}
            </select>
          </label>
          <button className="btn primary" disabled={busy || !minor || !merchant.name.trim()} onClick={() => minor && onBuy(merchant, minor, currency)}>
            Pagar {minor ? money(minor, currency) : ""}
          </button>
        </div>
        <p className="muted small">Disponible: {money(account.available_credit_minor, account.currency)}
          {account.purchase_limit_minor > 0 && <> · límite por compra {money(account.purchase_limit_minor, account.currency)}</>}
          {currency !== account.currency && <> · ⚠ tu tarjeta es {account.currency}: esta compra se rechazará</>}
        </p>
      </div>
    </section>
  );
}

function TxList({ txs }: { txs: Tx[] }) {
  if (!txs.length) return <p className="muted">Sin compras todavía.</p>;
  return (
    <ul className="list">
      {txs.map((t) => (
        <li key={t.id}>
          <div>
            <b>{t.merchant.name}</b>
            <div className="muted small">{day(t.created_at)} · MCC {t.merchant.mcc}{t.decline_reason ? ` · ${declineText(t.decline_reason)}` : ""}</div>
            {t.refunded_amount_minor > 0 && <div className="small ok">Devuelto {money(t.refunded_amount_minor, t.currency)}</div>}
            {t.status === "captured" && t.captured_amount_minor !== t.amount_minor && <div className="small muted">Cobrado {money(t.captured_amount_minor, t.currency)} de {money(t.amount_minor, t.currency)}</div>}
          </div>
          <div className="right">
            <b className={t.status === "declined" || t.status === "reversed" || t.status === "expired" ? "strike" : ""}>-{money(t.status === "captured" ? t.captured_amount_minor : t.amount_minor, t.currency)}</b>
            <Badge status={t.status} labels={TX_STATES} />
          </div>
        </li>
      ))}
    </ul>
  );
}

function Movements({ txs, payments }: { txs: Tx[]; payments: Payment[] }) {
  return (
    <section className="stack">
      <div className="card-panel"><h3>Compras</h3><TxList txs={txs} /></div>
      <div className="card-panel"><h3>Pagos a la tarjeta</h3><PaymentList payments={payments} /></div>
    </section>
  );
}

function PaymentList({ payments }: { payments: Payment[] }) {
  if (!payments.length) return <p className="muted">Sin pagos todavía.</p>;
  return (
    <ul className="list">
      {payments.map((p) => (
        <li key={p.id}>
          <div><b>Pago a la tarjeta</b><div className="muted small">{day(p.created_at)}</div></div>
          <div className="right"><b className="ok">+{money(p.amount_minor, p.currency)}</b><Badge status={p.status} labels={PAYMENT_STATES} /></div>
        </li>
      ))}
    </ul>
  );
}

function Pay({ account, summary, payments, busy, onPay }: { account: Account; summary: Summary | null; payments: Payment[]; busy: boolean; onPay: (a: number) => void }) {
  const c = account.currency;
  const [choice, setChoice] = useState<"min" | "total" | "other">("total");
  const [other, setOther] = useState("");
  const min = Math.min(summary?.minimum_due_minor ?? 0, account.outstanding_minor);
  const amount = choice === "min" ? min : choice === "total" ? account.outstanding_minor : parseMoney(other) ?? 0;
  return (
    <section className="stack">
      <div className="card-panel">
        <h3>Pagar mi tarjeta</h3>
        <div className="kv"><span>Deuda actual</span><b>{money(account.outstanding_minor, c)}</b></div>
        {summary?.due_date && <div className="kv"><span>Fecha límite</span><b>{day(summary.due_date)}</b></div>}
        <div className="choices">
          <label className={choice === "total" ? "on" : ""}><input type="radio" checked={choice === "total"} onChange={() => setChoice("total")} /> Pago total <b>{money(account.outstanding_minor, c)}</b></label>
          <label className={choice === "min" ? "on" : ""}><input type="radio" checked={choice === "min"} disabled={!min} onChange={() => setChoice("min")} /> Pago mínimo <b>{money(min, c)}</b></label>
          <label className={choice === "other" ? "on" : ""}><input type="radio" checked={choice === "other"} onChange={() => setChoice("other")} /> Otro valor
            <input inputMode="decimal" value={other} onFocus={() => setChoice("other")} onChange={(e) => setOther(e.target.value)} placeholder="0.00" /></label>
        </div>
        <button className="btn primary" disabled={busy || amount <= 0} onClick={() => onPay(amount)}>Pagar {amount > 0 ? money(amount, c) : ""}</button>
        <p className="muted small">El pago sale de tu banco: el cupo se libera solo cuando el banco lo acredita (panel de simulación → Banco).</p>
      </div>
      <div className="card-panel"><h3>Mis pagos</h3><PaymentList payments={payments} /></div>
    </section>
  );
}

function Statements({ account, summary, statements, charges, token }: { account: Account; summary: Summary | null; statements: Statement[]; charges: Charge[]; token: string }) {
  const c = account.currency;
  const [open, setOpen] = useState<Statement | null>(null);
  const [doc, setDoc] = useState<unknown>(null);
  async function view(s: Statement) {
    setOpen(s);
    setDoc(null);
    const res = await fetch(`/api/gateway/v1/accounts/${account.id}/statements/${s.id}`, { headers: { authorization: `Bearer ${token}` } });
    const [r] = (await res.json()) as CallResult[];
    if (r?.status === 200) setDoc(r.body.document);
  }
  return (
    <section className="stack">
      {summary && (
        <div className="card-panel row-between">
          <div><div className="muted small">Ciclo actual</div><b>{day(summary.cycle_start)} – {day(summary.cycle_end)}</b></div>
          <div><div className="muted small">Pago mínimo</div><b>{money(summary.minimum_due_minor, c)}</b></div>
          <div><div className="muted small">Vencido</div><b>{money(summary.past_due_minor, c)}{summary.dpd ? ` · ${summary.dpd} días` : ""}</b></div>
        </div>
      )}
      <div className="card-panel">
        <h3>Estados de cuenta</h3>
        {!statements.length ? <p className="muted">Aún no hay cortes. Avanza el reloj hasta el próximo corte en el panel de simulación.</p> : (
          <ul className="list">
            {statements.map((s) => (
              <li key={s.id} className="click" onClick={() => view(s)}>
                <div><b>Corte {s.cycle}</b><div className="muted small">{day(s.period_start)} – {day(s.period_end)} · vence {day(s.due_date)}</div></div>
                <div className="right"><b>{money(s.new_balance_minor, c)}</b><span className="muted small">mín. {money(s.minimum_due_minor, c)}</span></div>
              </li>
            ))}
          </ul>
        )}
      </div>
      {open && (
        <div className="card-panel">
          <div className="row-between"><h3>Corte {open.cycle}</h3><button className="link" onClick={() => setOpen(null)}>Cerrar</button></div>
          {[
            ["Saldo anterior", open.previous_balance_minor], ["Compras", open.purchases_minor], ["Devoluciones", -open.refunds_minor],
            ["Pagos", -open.payments_minor], ["Pagos devueltos", open.returns_minor], ["Intereses", open.interest_minor],
            ["Cargos", open.fees_minor], ["Condonaciones", -open.waivers_minor], ["Garantía aplicada", -open.collateral_applied_minor],
          ].filter(([, v]) => v !== 0).map(([k, v]) => <div key={k as string} className="kv"><span>{k}</span><b>{money(v as number, c)}</b></div>)}
          <div className="kv total"><span>Nuevo saldo</span><b>{money(open.new_balance_minor, c)}</b></div>
          <div className="kv"><span>Pago mínimo</span><b>{money(open.minimum_due_minor, c)}</b></div>
          {doc != null && <details><summary className="muted small">Documento completo (JSON en S3)</summary><pre>{JSON.stringify(doc, null, 2)}</pre></details>}
        </div>
      )}
      <div className="card-panel">
        <h3>Intereses y cargos</h3>
        {!charges.length ? <p className="muted">Sin intereses ni cargos.</p> : (
          <ul className="list">
            {charges.map((ch) => (
              <li key={ch.id}>
                <div><b>{CHARGE_KINDS[ch.kind] ?? ch.kind}</b><div className="muted small">Corte {ch.cycle} · {day(ch.effective_at)}</div></div>
                <div className="right"><b>{money(ch.amount_minor, c)}</b>{ch.waived_minor > 0 && <span className="small ok">condonado {money(ch.waived_minor, c)}</span>}</div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

function Settings({ account, payouts, busy, session, onFreeze, onLimit, onClose }: {
  account: Account; payouts: Payout[]; busy: boolean; session: Session;
  onFreeze: (on: boolean) => void; onLimit: (a: number) => void;
  onClose: (d: { bank_code: string; account_number: string; holder_name: string }) => void;
}) {
  const c = account.currency;
  const [limit, setLimit] = useState(account.purchase_limit_minor ? (account.purchase_limit_minor / 100).toFixed(2) : "");
  const [dest, setDest] = useState({ bank_code: "BANK001", account_number: "000123456789", holder_name: session.name || "Cliente de prueba" });
  const [confirm, setConfirm] = useState(false);
  const limitMinor = limit.trim() === "" ? 0 : parseMoney(limit);
  return (
    <section className="stack">
      <div className="card-panel row-between">
        <div><h3>Congelar tarjeta</h3><p className="muted small">Bloquea compras nuevas. Los cobros de compras ya autorizadas siguen pasando.</p></div>
        <button className={`switch ${account.frozen ? "on" : ""}`} role="switch" aria-checked={account.frozen} disabled={busy || account.state !== "active"}
          onClick={() => onFreeze(!account.frozen)}><span /></button>
      </div>
      <div className="card-panel">
        <h3>Límite por compra</h3>
        <p className="muted small">Rechaza compras individuales mayores a este monto. Vacío = sin límite.</p>
        <div className="row">
          <input inputMode="decimal" value={limit} onChange={(e) => setLimit(e.target.value)} placeholder="Sin límite" />
          <button className="btn" disabled={busy || limitMinor === null || account.state !== "active"} onClick={() => limitMinor !== null && onLimit(limitMinor)}>Guardar</button>
          <span className="muted small">Actual: {account.purchase_limit_minor ? money(account.purchase_limit_minor, c) : "sin límite"}</span>
        </div>
      </div>
      <div className="card-panel">
        <h3>Datos de la cuenta</h3>
        <div className="kv"><span>Cliente</span><code>{session.sub}</code></div>
        <div className="kv"><span>Cuenta</span><code>{account.id}</code></div>
        <div className="kv"><span>Activa desde</span><b>{day(account.activated_at)}</b></div>
        <div className="kv"><span>Fecha de la cuenta (reloj simulado)</span><b>{day(account.account_time)}</b></div>
      </div>
      <div className="card-panel">
        <h3>Cerrar mi cuenta</h3>
        {account.state === "active" ? (
          <>
            <p className="muted small">Se emite un estado de cuenta final, la garantía cubre la deuda pendiente y el saldo restante se transfiere a tu cuenta bancaria.</p>
            <div className="row">
              <label>Banco<input value={dest.bank_code} onChange={(e) => setDest({ ...dest, bank_code: e.target.value })} /></label>
              <label>N.º de cuenta<input value={dest.account_number} onChange={(e) => setDest({ ...dest, account_number: e.target.value })} /></label>
              <label>Titular<input value={dest.holder_name} onChange={(e) => setDest({ ...dest, holder_name: e.target.value })} /></label>
            </div>
            {!confirm
              ? <button className="btn ghost danger" disabled={busy} onClick={() => setConfirm(true)}>Cerrar cuenta…</button>
              : <div className="row"><b>¿Seguro? No se puede deshacer.</b>
                  <button className="btn danger" disabled={busy} onClick={() => { setConfirm(false); onClose(dest); }}>Sí, cerrar</button>
                  <button className="btn" onClick={() => setConfirm(false)}>No</button></div>}
          </>
        ) : (
          <>
            <div className="kv"><span>Estado</span><Badge status={account.state} labels={ACCOUNT_STATES} /></div>
            {account.close_stage && <div className="kv"><span>Etapa</span><b>{CLOSE_STAGES[account.close_stage] ?? account.close_stage}</b></div>}
            {account.destination_last4 && <div className="kv"><span>Cuenta destino</span><b>****{account.destination_last4}</b></div>}
          </>
        )}
        {payouts.length > 0 && (
          <ul className="list">
            {payouts.map((p) => (
              <li key={p.id}><div><b>Transferencia del saldo</b><div className="muted small">intento {p.attempt} · ****{p.destination_last4}</div></div>
                <div className="right"><b>{money(p.amount_minor, p.currency)}</b><Badge status={p.status} labels={PAYMENT_STATES} /></div></li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

// ---------- simulation rail ----------

function SimRail(p: {
  account: Account; session: Session; txs: Tx[]; payments: Payment[]; payouts: Payout[]; charges: Charge[]; summary: Summary | null;
  activity: Activity[]; busy: boolean; autoCapture: boolean; setAutoCapture: (v: boolean) => void;
  onMerchant: (op: "capture" | "reversal" | "refunds", t: Tx, amount?: number) => void;
  onDeposit: (d: SavedDeposit, s: "settled" | "failed") => void;
  onPayment: (op: "settle" | "fail" | "return", pay: Payment) => void;
  onPayout: (op: "settle" | "fail", po: Payout) => void;
  onRetryPayout: (po: Payout) => void;
  onWaive: (c: Charge) => void;
  onAdvance: (days: number | null, label: string) => void;
  onRefresh: () => void;
}) {
  const { account, busy } = p;
  const [refund, setRefund] = useState<Record<string, string>>({});
  const [days, setDays] = useState("7");
  const pendingDeposits = p.session.deposits.filter((d) => d.status === "pending");
  const openPayments = p.payments.filter((x) => x.status === "pending" || x.status === "settled");
  const merchantTx = p.txs.filter((t) => { const a = txActions(t); return a.capture || a.refundable > 0; });
  const openPayouts = p.payouts.filter((x) => x.status !== "settled");
  const waivable = p.charges.filter((c) => c.status === "posted" && c.amount_minor > c.waived_minor);
  return (
    <aside className="p-rail">
      <div className="rail-head"><h2>Simulación</h2><button className="link" disabled={busy} onClick={p.onRefresh}>↻ Actualizar</button></div>

      <section className="rail-box">
        <h3>⏱ Reloj de la cuenta</h3>
        <p className="muted small">Hoy en la cuenta: <b>{day(account.account_time)}</b>. Avanzar corre los cortes, intereses, mora y el cierre.</p>
        <div className="chips">
          <button disabled={busy} onClick={() => p.onAdvance(1, "1 día")}>+1 día</button>
          <button disabled={busy || !p.summary?.cycle_end} onClick={() => p.summary && p.onAdvance(daysToNextClose(p.summary), "hasta el corte")}>Hasta el corte</button>
          <button disabled={busy || !p.summary?.due_date} onClick={() => p.summary && p.onAdvance(daysToDuePlus(p.summary, 1), "1 día después del vencimiento")}>Vencer +1</button>
          <button disabled={busy || !p.summary?.due_date} onClick={() => p.summary && p.onAdvance(daysToDuePlus(p.summary, 15), "15 días de mora: bloqueo")}>Mora 15d</button>
          <button disabled={busy || !p.summary?.due_date} onClick={() => p.summary && p.onAdvance(daysToDuePlus(p.summary, 60), "60 días de mora: se aplica garantía")}>Mora 60d</button>
        </div>
        <div className="row">
          <input inputMode="numeric" value={days} onChange={(e) => setDays(e.target.value)} style={{ width: 70 }} aria-label="Días" />
          <button className="btn small" disabled={busy || !(Number(days) >= 1 && Number(days) <= 400)} onClick={() => p.onAdvance(Math.floor(Number(days)), "manual")}>Avanzar días</button>
        </div>
      </section>

      <section className="rail-box">
        <h3>🏦 Banco</h3>
        {!pendingDeposits.length && !openPayments.length && !openPayouts.length && <p className="muted small">Nada pendiente.</p>}
        {pendingDeposits.map((d) => (
          <div key={d.id} className="sim-item">
            <span>Depósito {money(d.amount_minor, d.currency)}</span>
            <div className="chips">
              <button className="ok" disabled={busy} onClick={() => p.onDeposit(d, "settled")}>Confirmar</button>
              <button className="err" disabled={busy} onClick={() => p.onDeposit(d, "failed")}>Rechazar</button>
            </div>
          </div>
        ))}
        {openPayments.map((x) => (
          <div key={x.id} className="sim-item">
            <span>Pago {money(x.amount_minor, x.currency)} <Badge status={x.status} labels={PAYMENT_STATES} /></span>
            <div className="chips">
              {x.status === "pending" && <>
                <button className="ok" disabled={busy} onClick={() => p.onPayment("settle", x)}>Acreditar</button>
                <button className="err" disabled={busy} onClick={() => p.onPayment("fail", x)}>Rechazar</button>
              </>}
              {x.status === "settled" && <button className="err" disabled={busy} onClick={() => p.onPayment("return", x)} title="Dentro de 5 días desde la acreditación">Devolver (fondos insuficientes)</button>}
            </div>
          </div>
        ))}
        {openPayouts.map((x) => (
          <div key={x.id} className="sim-item">
            <span>Transferir saldo {money(x.amount_minor, x.currency)} <Badge status={x.status} labels={PAYMENT_STATES} /></span>
            <div className="chips">
              {(x.status === "pending" || x.status === "settling") && <>
                <button className="ok" disabled={busy} onClick={() => p.onPayout("settle", x)}>Depositar</button>
                <button className="err" disabled={busy} onClick={() => p.onPayout("fail", x)}>Fallar</button>
              </>}
              {x.status === "failed" && <button disabled={busy} onClick={() => p.onRetryPayout(x)}>Soporte: reintentar</button>}
            </div>
          </div>
        ))}
      </section>

      <section className="rail-box">
        <h3>🏪 Comercios</h3>
        <label className="inline small"><input type="checkbox" checked={p.autoCapture} onChange={(e) => p.setAutoCapture(e.target.checked)} />
          Cobrar automáticamente al aprobar</label>
        {!merchantTx.length && <p className="muted small">Sin compras abiertas ni devolvibles. {p.autoCapture ? "Desactiva el cobro automático para probar anulaciones." : ""}</p>}
        {merchantTx.map((t) => {
          const a = txActions(t);
          const r = parseMoney(refund[t.id] ?? (a.refundable / 100).toFixed(2));
          return (
            <div key={t.id} className="sim-item">
              <span>{t.merchant.name} {money(t.amount_minor, t.currency)} <Badge status={t.status} labels={TX_STATES} /></span>
              <div className="chips">
                {a.capture && <>
                  <button className="ok" disabled={busy} onClick={() => p.onMerchant("capture", t)}>Cobrar</button>
                  <button disabled={busy} onClick={() => p.onMerchant("reversal", t)} title="Libera el cupo; solo dentro de 3 días">Anular</button>
                </>}
                {a.refundable > 0 && <>
                  <input inputMode="decimal" aria-label={`Monto a devolver en ${t.merchant.name}`} style={{ width: 80 }}
                    value={refund[t.id] ?? (a.refundable / 100).toFixed(2)} onChange={(e) => setRefund({ ...refund, [t.id]: e.target.value })} />
                  <button className="ok" disabled={busy || !r || r > a.refundable} onClick={() => r && p.onMerchant("refunds", t, r)}>Devolver</button>
                </>}
              </div>
            </div>
          );
        })}
      </section>

      {waivable.length > 0 && (
        <section className="rail-box">
          <h3>🎧 Soporte</h3>
          {waivable.map((c) => (
            <div key={c.id} className="sim-item">
              <span>{CHARGE_KINDS[c.kind] ?? c.kind} corte {c.cycle}: {money(c.amount_minor - c.waived_minor, account.currency)}</span>
              <div className="chips"><button disabled={busy} onClick={() => p.onWaive(c)}>Condonar</button></div>
            </div>
          ))}
        </section>
      )}

      <section className="rail-box">
        <h3>📜 Actividad</h3>
        {!p.activity.length ? <p className="muted small">Lo que pase aparece aquí.</p> : (
          <ul className="feed">
            {p.activity.map((a) => (
              <li key={a.id} className={`f-${a.tone}`}><span className="who">{a.who}</span>{a.text}<span className="at">{a.at}</span></li>
            ))}
          </ul>
        )}
      </section>
    </aside>
  );
}

function Badge({ status, labels }: { status: string; labels: Record<string, string> }) {
  return <span className={`pill p-${tone(status)}`}>{labels[status] ?? status}</span>;
}
