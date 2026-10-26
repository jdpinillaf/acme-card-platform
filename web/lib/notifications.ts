// Notifications helpers for the simulator page (Sprint 7): the popup feed. Client-safe.

/** Items of `incoming` not already in `prev`, oldest first (the inbox API answers newest first). */
export function newItems<T extends { id: number }>(prev: T[], incoming: T[]): T[] {
  const seen = new Set(prev.map((i) => i.id));
  return incoming.filter((i) => !seen.has(i.id)).sort((a, b) => a.id - b.id);
}

export function channelIcon(channel: string): string {
  return ({ push: "📱", sms: "💬", email: "✉️" } as Record<string, string>)[channel] ?? "🔔";
}
