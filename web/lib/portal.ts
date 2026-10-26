// Pure helpers for the cardholder portal (/portal): money parsing and formatting, Spanish copy for
// gateway states and errors, the demo merchant catalog and the saved browser session.

/** Parses a typed amount ("120", "120.50", "1.234,56") into minor units; null if invalid or ≤ 0. */
export function parseMoney(input: string): number | null {
  const s = input.trim();
  if (!/^\d[\d.,]*$/.test(s)) return null;
  const lastSep = Math.max(s.lastIndexOf("."), s.lastIndexOf(","));
  let units = s;
  let cents = "";
  if (lastSep >= 0) {
    const tail = s.slice(lastSep + 1);
    const mixed = s.includes(".") && s.includes(",");
    if (tail.length <= 2 && (mixed || s.split(s[lastSep]).length === 2)) {
      units = s.slice(0, lastSep);
      cents = tail;
    } else if (!mixed && tail.length === 3) {
      return null; // "1.234" is ambiguous: thousands or three decimals
    }
  }
  units = units.replace(/[.,]/g, "");
  if (!/^\d+$/.test(units) || !/^\d{0,2}$/.test(cents)) return null;
  const minor = Number(units) * 100 + Number(cents.padEnd(2, "0") || 0);
  return Number.isSafeInteger(minor) && minor > 0 ? minor : null;
}

/** Exact formatting of minor units: USD as "$1,234.50", other currencies as "1,234.50 EUR". */
export function money(minor: number, currency: string): string {
  const sign = minor < 0 ? "-" : "";
  const abs = Math.abs(minor);
  const units = Math.floor(abs / 100).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const n = `${units}.${String(abs % 100).padStart(2, "0")}`;
  return currency === "USD" ? `${sign}$${n}` : `${sign}${n} ${currency}`;
}

const DECLINES: Record<string, string> = {
  account_closed: "La cuenta está cerrada.",
  account_closing: "La cuenta está en proceso de cierre.",
  charged_off: "La deuda fue castigada; la tarjeta no admite compras.",
  collateral_not_confirmed: "El banco aún no confirma tu depósito de garantía.",
  currency_not_supported: "La tarjeta no admite compras en esa moneda.",
  payment_returned: "Un pago fue devuelto por tu banco; regulariza la deuda para volver a comprar.",
  over_limit: "Tu deuda supera el cupo; paga para volver a comprar.",
  past_due: "Tienes un pago vencido; ponte al día para volver a comprar.",
  card_frozen: "Tu tarjeta está congelada.",
  velocity_limit_exceeded: "Demasiados intentos en poco tiempo; espera un minuto.",
  purchase_limit_exceeded: "La compra supera el límite por compra que configuraste.",
  insufficient_credit: "No tienes cupo disponible suficiente.",
};

/** Spanish explanation for a decline reason; unknown reasons are shown as-is. */
export function declineText(reason: string): string {
  return DECLINES[reason] ?? reason;
}

const ERRORS: Record<string, string> = {
  ...DECLINES,
  account_exists: "Ya tienes una cuenta en esa moneda.",
  previous_account_has_debt: "Una cuenta cerrada en esa moneda aún tiene deuda.",
  payment_exceeds_balance: "El pago supera tu deuda actual.",
  nothing_to_pay: "No tienes deuda por pagar.",
  invalid_amount: "Monto inválido (el mínimo de pago es $1.00, o la deuda completa si es menor).",
  purchase_limit_above_credit_limit: "El límite por compra no puede superar tu cupo.",
  not_cancellable: "Solo se puede cancelar una cuenta que aún espera la garantía.",
  not_closable: "Una cuenta sin garantía se cancela, no se cierra.",
  close_already_requested: "Ya pediste el cierre con otra cuenta de destino.",
  already_captured: "El comercio ya cobró esta compra.",
  already_reversed: "Esta compra ya fue anulada.",
  authorization_expired: "La autorización expiró.",
  not_captured: "Solo se puede devolver una compra ya cobrada.",
  refund_exceeds_captured: "La devolución supera lo cobrado.",
  reversal_window_expired: "Solo se puede anular dentro de los 3 días siguientes.",
  return_window_expired: "El banco solo puede devolver un pago dentro de 5 días.",
  not_settled: "Solo se puede devolver un pago ya acreditado.",
  operation_in_progress: "Hay otra operación en curso; intenta de nuevo.",
  deposit_settling: "El depósito se está procesando; intenta de nuevo.",
  payment_settling: "El pago se está procesando; intenta de nuevo.",
  rate_limited: "Demasiadas solicitudes; espera un momento.",
};

/** Human message for a failed gateway call: known code, then the gateway's message, then the status. */
export function errorText(r: { status: number; body: unknown }): string {
  const err = (r.body as { error?: { code?: string; message?: string } } | null)?.error;
  if (err?.code && ERRORS[err.code]) return ERRORS[err.code];
  if (err?.message) return err.message;
  return r.status === 0 ? "No se pudo contactar el servidor (0)" : `Error ${r.status}`;
}

export const ACCOUNT_STATES: Record<string, string> = {
  pending_collateral: "Esperando garantía",
  active: "Activa",
  closing: "En cierre",
  closed: "Cerrada",
  cancelled: "Cancelada",
};

export const TX_STATES: Record<string, string> = {
  pending: "Procesando",
  approved: "Autorizada",
  captured: "Cobrada",
  declined: "Rechazada",
  reversed: "Anulada",
  expired: "Expirada",
  failed: "Fallida",
};

export const PAYMENT_STATES: Record<string, string> = {
  pending: "En proceso",
  settling: "Acreditando",
  settled: "Acreditado",
  failed: "Rechazado",
  expired: "Expirado",
  returning: "Devolviendo",
  returned: "Devuelto",
};

export const DEPOSIT_STATES: Record<string, string> = {
  pending: "Esperando banco",
  settled: "Confirmado",
  failed: "Rechazado",
  voided: "Anulado",
};

export const CLOSE_STAGES: Record<string, string> = {
  in_flight: "Esperando que terminen operaciones en curso",
  final_statement: "Generando estado de cuenta final",
  awaiting_due: "Esperando la fecha límite del estado final",
  applying: "Aplicando la garantía a la deuda",
  awaiting_returns: "Esperando la ventana de devoluciones de pagos",
  payout: "Enviando el saldo a tu cuenta bancaria",
  closed: "Cerrada",
};

export const CHARGE_KINDS: Record<string, string> = {
  interest: "Intereses",
  late_fee: "Cargo por mora",
};

/** Status → visual tone for badges. */
export function tone(status: string): "ok" | "warn" | "err" | "muted" {
  if (["active", "captured", "settled", "approved", "posted"].includes(status)) return status === "approved" ? "warn" : "ok";
  if (["pending", "settling", "pending_collateral", "closing", "returning"].includes(status)) return "warn";
  if (["declined", "failed", "returned", "closed", "cancelled"].includes(status)) return "err";
  return "muted";
}

export type Merchant = { id: string; name: string; mcc: string; category: string; price: number; emoji: string };

/** Demo shops (price in minor units, editable before buying). */
export const MERCHANTS: Merchant[] = [
  { id: "m-cafe", name: "Café Central", mcc: "5814", category: "Restaurantes", price: 450, emoji: "☕" },
  { id: "m-super", name: "Supermercado La Huerta", mcc: "5411", category: "Supermercado", price: 8730, emoji: "🛒" },
  { id: "m-gas", name: "Estación Rápida", mcc: "5541", category: "Combustible", price: 4200, emoji: "⛽" },
  { id: "m-books", name: "Corner Bookstore", mcc: "5942", category: "Libros", price: 2399, emoji: "📚" },
  { id: "m-tech", name: "TecnoMundo", mcc: "5732", category: "Electrónica", price: 64900, emoji: "💻" },
  { id: "m-air", name: "Aerolínea Sur", mcc: "4511", category: "Viajes", price: 31250, emoji: "✈️" },
  { id: "m-stream", name: "StreamPlus", mcc: "4899", category: "Suscripciones", price: 1599, emoji: "🎬" },
  { id: "m-pharma", name: "Farmacia Salud", mcc: "5912", category: "Farmacia", price: 1875, emoji: "💊" },
];

export type TxLike = { status: string; amount_minor: number; captured_amount_minor: number; refunded_amount_minor: number };

/** Which merchant-side operations the simulator offers for a purchase. */
export function txActions(t: TxLike): { capture: boolean; reverse: boolean; refundable: number } {
  const open = t.status === "approved";
  return { capture: open, reverse: open, refundable: t.status === "captured" ? t.captured_amount_minor - t.refunded_amount_minor : 0 };
}

export type SavedDeposit = { id: string; amount_minor: number; currency: string; status: string };
export type Session = { sub: string; name: string; accountId: string; deposits: SavedDeposit[] };

/** Reads a session saved in localStorage; anything malformed yields null. */
export function parseSession(raw: string | null): Session | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw) as Partial<Session>;
    if (typeof v?.sub !== "string" || !v.sub) return null;
    return {
      sub: v.sub,
      name: typeof v.name === "string" ? v.name : "",
      accountId: typeof v.accountId === "string" ? v.accountId : "",
      deposits: Array.isArray(v.deposits) ? v.deposits : [],
    };
  } catch {
    return null;
  }
}
