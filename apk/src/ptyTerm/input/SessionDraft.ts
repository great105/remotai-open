/** Draft ownership outlives a component and is independent of WS reconnects. */
const fallback = new Map<string, string>();
const listeners = new Map<string, Set<() => void>>();
export const draftOwner = (context: string, session: string) => JSON.stringify([context, session]);
const key = (owner: string) => `pty.draft.v2.${encodeURIComponent(owner)}`;

export function readSessionDraft(owner: string): string {
  if (fallback.has(owner)) return fallback.get(owner)!;
  try { return localStorage.getItem(key(owner)) ?? ""; } catch { return ""; }
}

export function writeSessionDraft(owner: string, text: string): void {
  try {
    localStorage.setItem(key(owner), text);
    fallback.delete(owner);
  } catch { fallback.set(owner, text); }
  listeners.get(owner)?.forEach(notify => notify());
}

export function appendSessionDraft(owner: string, text: string): void {
  const current = readSessionDraft(owner);
  writeSessionDraft(owner, current ? current + "\n" + text : text);
}

export function migrateSessionDraft(owner: string, session: string): void {
  if (!session) return;
  try {
    const legacy = localStorage.getItem(`pty.draft.${session}`);
    if (legacy === null || localStorage.getItem(key(owner)) !== null || fallback.has(owner)) return;
    // Claim an existing legacy draft once, so another device cannot inherit it.
    localStorage.setItem(key(owner), legacy);
    localStorage.removeItem(`pty.draft.${session}`);
    listeners.get(owner)?.forEach(notify => notify());
  } catch { /* The existing draft is retained if persistence is unavailable. */ }
}

export function observeSessionDraft(owner: string, notify: () => void): () => void {
  let subscribers = listeners.get(owner);
  if (!subscribers) listeners.set(owner, subscribers = new Set());
  subscribers.add(notify);
  const changed = (event: StorageEvent) => { if (event.key === key(owner) || event.key === null) notify(); };
  window.addEventListener("storage", changed);
  return () => {
    subscribers.delete(notify);
    if (!subscribers.size) listeners.delete(owner);
    window.removeEventListener("storage", changed);
  };
}
