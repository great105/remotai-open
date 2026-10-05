import { afterEach, expect, it, vi } from "vitest";
import { appendSessionDraft, draftOwner, migrateSessionDraft, readSessionDraft, writeSessionDraft } from "./SessionDraft";

afterEach(() => vi.unstubAllGlobals());
it("isolates device/session drafts and retains delayed text at its original owner", () => {
  const data = new Map<string, string>();
  vi.stubGlobal("localStorage", { getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => data.set(k, v), removeItem: (k: string) => data.delete(k) });
  const a = draftOwner("pc-a", "session"), b = draftOwner("pc-b", "session");
  writeSessionDraft(a, "A draft"); writeSessionDraft(b, "B draft");
  appendSessionDraft(a, "delayed clipboard");
  expect(readSessionDraft(a)).toBe("A draft\ndelayed clipboard");
  expect(readSessionDraft(b)).toBe("B draft");
  data.set("pty.draft.legacy", "old draft");
  migrateSessionDraft(draftOwner("pc-a", "legacy"), "legacy");
  migrateSessionDraft(draftOwner("pc-b", "legacy"), "legacy");
  expect(readSessionDraft(draftOwner("pc-a", "legacy"))).toBe("old draft");
  expect(readSessionDraft(draftOwner("pc-b", "legacy"))).toBe("");
});
it("retains a recovery draft in memory if browser storage refuses writes", () => {
  vi.stubGlobal("localStorage", { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("quota"); } });
  const owner = draftOwner("private-fixture", "unmounted");
  writeSessionDraft(owner, "draft"); appendSessionDraft(owner, "pending");
  expect(readSessionDraft(owner)).toBe("draft\npending");
});
