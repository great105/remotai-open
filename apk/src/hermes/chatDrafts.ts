import type { DraftStorage, HermesDraft } from "./client";

function key(device: string, sessionId: string): string {
  return `remotai.hermes.chat-draft.v1:${encodeURIComponent(device)}:${sessionId ? `chat:${encodeURIComponent(sessionId)}` : "new"}`;
}

/** Drafts belong to a particular chat on a particular computer. */
export function saveChatDraft(storage: DraftStorage | null, device: string, draft: HermesDraft): void {
  try { storage?.setItem(key(device, draft.sessionId), JSON.stringify(draft)); }
  catch { /* Storage restrictions must not block a conversation. */ }
}

export function readChatDraft(storage: DraftStorage | null, device: string, sessionId: string): HermesDraft | null {
  try {
    const value: unknown = JSON.parse(storage?.getItem(key(device, sessionId)) || "null");
    if (!value || typeof value !== "object") return null;
    const draft = value as Record<string, unknown>;
    if (draft.sessionId !== sessionId || typeof draft.text !== "string" || typeof draft.cwd !== "string") return null;
    return {
      text: draft.text, cwd: draft.cwd, sessionId,
      provider: typeof draft.provider === "string" ? draft.provider : "",
      model: typeof draft.model === "string" ? draft.model : "",
    };
  } catch { return null; }
}
