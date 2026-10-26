// Day math and formatting for the simulator's billing panel (Sprint 4). The gateway advances an
// account's simulated clock by whole days (1-400 per call).
const DAY_MS = 24 * 3600 * 1000;
const MAX_ADVANCE_DAYS = 400;

export type SummaryTimes = { account_time: string; cycle_end?: string; due_date?: string };

function daysUntil(fromIso: string, to: Date): number {
  const ms = to.getTime() - new Date(fromIso).getTime();
  return Math.min(Math.max(Math.ceil(ms / DAY_MS), 1), MAX_ADVANCE_DAYS);
}

/** Days to advance so the account reaches its next statement close. */
export function daysToNextClose(s: SummaryTimes): number {
  return daysUntil(s.account_time, new Date(s.cycle_end ?? s.account_time));
}

/** Days to advance so the account is N days past its last due date (DPD N); null without one. */
export function daysToDuePlus(s: SummaryTimes, n: number): number | null {
  if (!s.due_date) return null;
  return daysUntil(s.account_time, new Date(new Date(s.due_date).getTime() + n * DAY_MS));
}

/** Exact minor units with two decimals, e.g. 2500 USD → "25.00 USD". */
export function formatMinor(minor: number, currency: string): string {
  const sign = minor < 0 ? "-" : "";
  const abs = Math.abs(minor);
  return `${sign}${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, "0")} ${currency}`;
}
