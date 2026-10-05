import { describe, expect, it, vi } from "vitest";
import {
  createHermesClient, draftKey, emptyRunState, providerConnected, readDraft, runtimeBusy, acceptDraft,
  safeVerificationUrl, scheduledTaskTime, unseenEvents, writeDraft, type HermesEvent, type HermesStatus, type RequestTransport,
} from "./client";
import { applyHermesEvent, durableSessionId, gatewayRestarted, modelSelection, stateFromSession } from "./state";

const status: HermesStatus = { installed: false, ownership: "none", running: false, ready: false, state: "installing", auto_update: true };
function event(seq: number, type: string, payload: Record<string, unknown> = {}, session_id = "live-A"): HermesEvent {
  return { seq, frame: { method: "event", params: { type, payload, session_id } } };
}
function memory() {
  const values = new Map<string, string>();
  return { values, getItem: (key: string) => values.get(key) || null, setItem: (key: string, value: string) => { values.set(key, value); } };
}

describe("Hermes onboarding and transport", () => {
  it("rejects missing, invalid and elapsed one-off task times before scheduling", () => {
    const now = Date.parse("2026-10-01T07:00:00Z");
    for (const value of ["", "not a date", "2026-10-01T06:59:59Z", "2026-10-01T07:00:00Z"]) {
      expect(() => scheduledTaskTime("once", value, now)).toThrow("Выберите время в будущем.");
    }
  });
  it("keeps an offset time as an exact instant and repeating intervals independent of the date field", () => {
    const now = Date.parse("2026-10-01T07:00:00Z");
    expect(scheduledTaskTime("once", "2026-10-02T10:30:00+03:00", now)).toBe("2026-10-02T07:30:00.000Z");
    expect(scheduledTaskTime("every 24h", "", now)).toBe("every 24h");
  });
  it("keeps asynchronous installation acceptance separate from readiness", async () => {
    const request = vi.fn(async (_path: string, _init?: RequestInit) => ({ accepted: true, operation: "install", status }));
    const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
    const result = await client.install();
    expect(result.ready).toBe(false);
    expect(runtimeBusy(result)).toBe(true);
    expect(request.mock.calls[0]).toEqual(["/api/hermes/install", { method: "POST", body: "{}" }]);
  });
  it("does not infer a connected subscription from an available provider or discovered configuration", () => {
    expect(providerConnected({ id: "copilot", name: "Copilot", available: true, status: { configured: true, auth_verified: false } })).toBe(false);
    expect(providerConnected({ id: "openai-codex", name: "ChatGPT", status: { logged_in: true } })).toBe(true);
  });
  it("uses authenticated host transport for device-code polling and server request replies", async () => {
    const request = vi.fn(async (_path: string, _init?: RequestInit) => ({ status: "pending" }));
    const client = createHermesClient(() => ({ request } as unknown as RequestTransport));
    await client.pollLogin("openai-codex", "a/b");
    await client.reply("approval:3", { choice: "once" });
    expect(request.mock.calls[0][0]).toBe("/api/hermes/backend/providers/oauth/openai-codex/poll/a%2Fb");
    expect(request.mock.calls[1]).toEqual(["/api/hermes/reply", { method: "POST", body: '{"id":"approval:3","result":{"choice":"once"}}' }]);
  });
  it.each(["runtime", "backend"])("rejects a stale %s screen before it can mutate another selected computer", async kind => {
    const request = vi.fn();
    const client = createHermesClient(() => ({ request }), () => { throw new Error("device changed"); });
    await expect(kind === "runtime" ? client.start() : client.backendRequest("/profiles/default/soul", "PUT", { content: "Instruction" })).rejects.toThrow("device changed");
    expect(request).not.toHaveBeenCalled();
  });
  it.each(["runtime", "backend"])("rejects a %s response that arrives after the selected computer changes", async kind => {
    let changed = false;
    const request = vi.fn(async () => { changed = true; return status; });
    const client = createHermesClient(() => ({ request } as unknown as RequestTransport), () => { if (changed) throw new Error("device changed"); });
    await expect(kind === "runtime" ? client.status() : client.backendRequest("/memory?profile=default")).rejects.toThrow("device changed");
  });
  it("only opens an HTTPS verification URL without embedded credentials", () => {
    expect(safeVerificationUrl("https://auth.openai.com/codex/device")).toBe("https://auth.openai.com/codex/device");
    for (const url of ["javascript:alert(1)", "http://auth.openai.com", "https://user:password@example.com", "not a URL"]) expect(safeVerificationUrl(url)).toBeNull();
  });
});

describe("Hermes drafts and model selection", () => {
  it("recovers task text and chosen model per execution computer after reload", () => {
    const store = memory();
    const draft = { text: "Проверь проект", cwd: "C:/project", sessionId: "stored-A", provider: "openai-codex", model: "gpt-model" };
    writeDraft(store, "computer-A", draft);
    expect(readDraft(store, "computer-A")).toEqual(draft);
    expect(readDraft(store, "computer-B").text).toBe("");
  });
  it("handles unavailable storage and malformed saved drafts without losing the screen", () => {
    const store = memory();
    store.setItem(draftKey("A"), "{broken");
    expect(readDraft(store, "A").text).toBe("");
    expect(() => writeDraft({ getItem: () => null, setItem: () => { throw new Error("quota"); } }, "A", { text: "draft", cwd: "", sessionId: "" })).not.toThrow();
  });
  it("recovers an accepted send after leaving the page without erasing a newer draft", () => {
    const store = memory();
    writeDraft(store, "A", { text: "sent task", cwd: "/work", sessionId: "" });
    acceptDraft(store, "A", "sent task", "durable-A");
    expect(readDraft(store, "A")).toMatchObject({ text: "", cwd: "/work", sessionId: "durable-A" });
    writeDraft(store, "A", { text: "new task", cwd: "/work", sessionId: "durable-A" });
    acceptDraft(store, "A", "sent task", "durable-A");
    expect(readDraft(store, "A").text).toBe("new task");
  });
  it("preserves the active session's actual model after adding a different account", () => {
    const inventory = { provider: "nous", model: "welcome", providers: [{ slug: "openai-codex", models: ["subscription-model"] }] };
    expect(modelSelection(inventory, { provider: "nous", model: "live-model" }, true, "openai-codex"))
      .toEqual({ provider: "nous", model: "live-model" });
    expect(modelSelection(inventory, { provider: "nous", model: "welcome" }, false, "openai-codex"))
      .toEqual({ provider: "openai-codex", model: "subscription-model" });
  });
});

describe("Hermes streaming and recovery", () => {
  it("replaces streamed fragments with the gateway's authoritative final reply", () => {
    let state = emptyRunState();
    state = applyHermesEvent(state, event(1, "message.start"), "live-A");
    state = applyHermesEvent(state, event(2, "message.delta", { text: "Partial " }), "live-A");
    state = applyHermesEvent(state, event(3, "message.delta", { text: "reply" }), "live-A");
    expect(state.busy).toBe(true);
    state = applyHermesEvent(state, event(4, "message.complete", { text: "Final reply", status: "complete" }), "live-A");
    expect(state.messages.map(row => row.content)).toEqual(["Final reply"]);
    expect(state.busy).toBe(false);
  });
  it("never attaches another session's messages or approvals to the visible chat", () => {
    const state = emptyRunState();
    expect(applyHermesEvent(state, event(1, "message.delta", { text: "other" }, "live-B"), "live-A")).toBe(state);
    expect(applyHermesEvent(state, { seq: 2, frame: { id: "B", method: "approval", params: { session_id: "live-B", command: "other" } } }, "live-A")).toBe(state);
  });
  it("rebuilds an interrupted stream and unanswered approval when reconnecting", () => {
    const state = stateFromSession({ session_id: "new-live", stored_session_id: "stored-A", messages: [{ role: "user", text: "Task", content: [] }], info: { running: true }, inflight: { user: "Task", assistant: "Working", streaming: true }, open_requests: [{ id: "approve-A", method: "approval", params: { command: "command", choices: ["once", "deny"] } }] });
    expect(state.messages.map(row => row.content)).toEqual(["Task", "Working"]);
    expect(state.busy).toBe(true);
    expect(state.prompts[0].id).toBe("approve-A");
  });
  it("keeps the durable session identity when resume only returns session_key or info", () => {
    expect(durableSessionId({ session_id: "transient", stored_session_id: "", session_key: "durable" })).toBe("durable");
    expect(durableSessionId({ session_id: "transient", stored_session_id: "", info: { stored_session_id: "durable" } })).toBe("durable");
    expect(durableSessionId({ session_id: "transient", stored_session_id: "" }, "requested")).toBe("requested");
    expect(durableSessionId({ session_id: "transient", stored_session_id: "" })).toBe("");
  });
  it("detects a fast gateway restart even if status never showed an offline interval", () => {
    expect(gatewayRestarted([event(100, "gateway.ready")])).toBe(true);
    expect(gatewayRestarted([], true)).toBe(true);
    expect(gatewayRestarted([event(100, "message.complete")])).toBe(false);
  });
  it("restores only the live session reclaimed by the backend, not a different session", () => {
    const reclaimed = [event(9, "session.reclaimed", { session_id: "live-A", stored_session_id: "durable-A" }, "")];
    expect(gatewayRestarted(reclaimed, false, "live-A")).toBe(true);
    expect(gatewayRestarted(reclaimed, false, "live-B")).toBe(false);
  });
  it("displays delegated subagent goals even when no generic text field exists", () => {
    const state = applyHermesEvent(emptyRunState(), event(8, "subagent.start", { goal: "Check the project", task_count: 2 }), "live-A");
    expect(state.activities[0].text).toBe("Check the project");
  });
  it("ignores repeated polling pages and accepts an explicitly reset event buffer", () => {
    const page = { events: [event(3, "message.delta"), event(2, "message.delta")], latest_seq: 3 };
    expect(unseenEvents(page, 3)).toEqual([]);
    expect(unseenEvents({ ...page, reset: true }, 100).map(row => row.seq)).toEqual([2, 3]);
  });
  it("withdraws a cancelled request instead of leaving an obsolete approval button", () => {
    const approval = { seq: 1, frame: { id: "request-A", method: "approval", params: { session_id: "live-A", command: "command" } } };
    let state = applyHermesEvent(emptyRunState(), approval, "live-A");
    state = applyHermesEvent(state, event(2, "request.cancel", { id: "request-A" }), "live-A");
    expect(state.prompts).toEqual([]);
  });
});
