import type { PtySessionInfo } from "./types";

const key = (context: string) => `pty.lastViewed.v1:${context}`;

/** Remember a user's visit, never output/activity. Scope it to the connected machine. */
export function rememberTerminal(storage: Pick<Storage, "setItem">, context: string, id: string): void {
  if (!context || !id) return;
  try { storage.setItem(key(context), id); } catch { /* private mode */ }
}

export function lastViewedTerminal(storage: Pick<Storage, "getItem">, context: string): string {
  try { return storage.getItem(key(context)) || ""; } catch { return ""; }
}

export function terminalToContinue(sessions: PtySessionInfo[], id: string): PtySessionInfo | undefined {
  return sessions.find((session) => session.alive && session.id === id);
}
