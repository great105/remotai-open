import { t } from "@tgcontrol/shared";
import { emptyRunState, textContent, type HermesEvent, type HermesRunState, type HermesPrompt, type RpcFrame } from "./client";
import { isTerminalHermesSubagentStatus, normalizeHermesSubagentStatus } from "./subagents";

function object(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

export function promptFromFrame(frame: RpcFrame): HermesPrompt | null {
  if (frame.id === undefined || !frame.method || frame.method === "event") return null;
  const params = frame.params || {};
  const choices = Array.isArray(params.choices) ? params.choices.map(choice => {
    const value = object(choice);
    return typeof choice === "string" ? choice : String(value.label || value.value || "");
  }).filter(Boolean) : [];
  return {
    id: frame.id,
    kind: ["approval", "approval.request"].includes(frame.method) ? "approval" : frame.method === "clarify" ? "question" : "request",
    method: frame.method,
    title: String(params.title || params.question || (["approval", "approval.request"].includes(frame.method) ? t("ui.state.m588755e02e") : t("ui.state.m2c461cc430"))),
    description: String(params.description || params.command || params.message || params.reason || ""),
    choices,
    params,
  };
}

/** The gateway's live session id scopes streams; stored ids are used only for resume. */
export function applyHermesEvent(state: HermesRunState, event: HermesEvent, sessionId: string): HermesRunState {
  const frame = event.frame;
  const params = frame.params || {};
  if (params.session_id && params.session_id !== sessionId) return state;
  const prompt = promptFromFrame(frame);
  if (prompt) return {
    ...state,
    prompts: [...state.prompts.filter(row => String(row.id) !== String(prompt.id)), prompt],
  };
  if (frame.method !== "event") return state;
  const type = String(params.type || "");
  const payload = object(params.payload);
  if (type === "request.cancel") {
    return { ...state, prompts: state.prompts.filter(row => String(row.id) !== String(payload.id || payload.request_id)) };
  }
  if (type === "approval.cancelled") {
    const ids = Array.isArray(payload.request_ids) ? payload.request_ids.map(String) : [];
    return { ...state, prompts: state.prompts.filter(row => !ids.includes(String(row.id))) };
  }
  if (type === "message.start") return { ...state, busy: true, error: "", streamId: null, progressText: "" };
  if (type === "session.info" && typeof payload.running === "boolean") return { ...state, busy: payload.running };
  if (type === "thinking.delta") {
    // Hermes also uses this callback for provider waits and spinner rewrites.
    // Only the dedicated reasoning events carry published model reasoning.
    return typeof payload.text === "string" ? { ...state, progressText: payload.text } : state;
  }
  if (type === "reasoning.delta" || type === "reasoning.available") {
    if (typeof payload.text !== "string" || !payload.text) return state;
    const id = state.streamId || `stream-${event.seq}`;
    const existing = state.messages.find(row => row.id === id);
    // `available` is a complete block, like the official Desktop's replacement
    // path; deltas remain exact fragments and never enter the answer body.
    const reasoning = type === "reasoning.available" ? payload.text : (existing?.reasoning || "") + payload.text;
    return {
      ...state, busy: true, streamId: id, progressText: "",
      messages: existing ? state.messages.map(row => row.id === id ? { ...row, reasoning } : row)
        : [...state.messages, { id, role: "assistant", content: "", reasoning }],
    };
  }
  if (type === "message.delta") {
    const id = state.streamId || `stream-${event.seq}`;
    const existing = state.messages.find(row => row.id === id);
    const content = (existing?.content || "") + textContent(payload.text);
    return {
      ...state, busy: true, streamId: id, progressText: "",
      messages: existing ? state.messages.map(row => row.id === id ? { ...row, content } : row)
        : [...state.messages, { id, role: "assistant", content }],
    };
  }
  if (type === "message.complete" || type === "message.interim") {
    const id = state.streamId || `assistant-${event.seq}`;
    const content = textContent(payload.text);
    const reasoning = type === "message.complete" && typeof payload.reasoning === "string" ? payload.reasoning : undefined;
    const existing = state.messages.some(row => row.id === id);
    const messages = existing ? state.messages.map(row => row.id === id ? { ...row, content,
      ...(reasoning === undefined ? {} : { reasoning }) } : row)
      : content || reasoning ? [...state.messages, { id, role: "assistant", content,
        ...(reasoning ? { reasoning } : {}) }] : state.messages;
    return {
      ...state, messages, streamId: null,
      busy: type === "message.interim",
      progressText: type === "message.complete" ? "" : state.progressText,
      error: type === "message.complete" && payload.status === "error" ? String(payload.error || payload.failure_reason || t("ui.state.mf6718dc78d")) : state.error,
    };
  }
  if (type === "error") return { ...state, busy: false, progressText: "", error: String(payload.message || t("ui.state.m31ec01afb2")) };
  if (type === "tool.generating") {
    return typeof payload.name === "string" && payload.name
      ? { ...state, progressText: t("ui.state.m49f2072067", { p0: (payload.name) }) } : state;
  }
  if (type === "tool.start" || type === "tool.complete") {
    const id = String(payload.tool_id || event.seq);
    const entry = {
      id, kind: "tool", text: String(payload.summary || payload.preview || payload.name || t("ui.state.m590d118f9a")),
      details: type === "tool.start" ? String(payload.args_text || JSON.stringify(payload.args || {}, null, 2))
        : String(payload.result_text || payload.inline_diff || (payload.result ? JSON.stringify(payload.result, null, 2) : "")),
      complete: type === "tool.complete",
    };
    return { ...state, activities: state.activities.some(row => row.id === id)
      ? state.activities.map(row => row.id === id ? entry : row) : [...state.activities, entry] };
  }
  if (type === "subagent.thinking") return state;
  if (type.startsWith("subagent.")) {
    const subagentId = typeof payload.subagent_id === "string" && payload.subagent_id ? payload.subagent_id : undefined;
    const id = subagentId ? `subagent:${subagentId}` : `subagent-event:${event.seq}`;
    const previous = state.activities.find(row => row.id === id);
    // Progress callbacks may arrive after the terminal callback; never revive an ended child.
    if (previous?.complete && type !== "subagent.complete") return state;
    const reported = normalizeHermesSubagentStatus(payload.status);
    const status = reported !== "unknown" ? reported : type === "subagent.complete" ? "unknown"
      : previous?.status || (["subagent.start", "subagent.spawn_requested"].includes(type) ? "running" : "unknown");
    const text = typeof payload.goal === "string" ? payload.goal : previous?.text || t("ui.state.mffcd1996d1");
    const entry = {
      id, kind: type, text, status,
      ...(subagentId ? { subagentId } : {}),
      complete: type === "subagent.complete" || isTerminalHermesSubagentStatus(status),
    };
    // A child lifecycle is independent of the parent's main turn and answer body.
    return { ...state, activities: previous ? state.activities.map(row => row.id === id ? entry : row)
      : [...state.activities, entry].slice(-100) };
  }
  if (type === "status.update" || type === "notice") {
    const text = String(payload.text || payload.message || payload.goal || payload.task || payload.status || "");
    return text ? { ...state,
      ...(type === "status.update" ? { progressText: text } : {}),
      activities: [...state.activities, { id: String(event.seq), kind: type, text }].slice(-100) } : state;
  }
  return state;
}

export interface SessionSnapshot {
  session_id: string;
  stored_session_id?: string;
  session_key?: string;
  resumed?: string;
  messages?: Array<{
    role?: string; content?: unknown; text?: string; row_id?: number; reasoning?: string;
    tool_call_id?: string; name?: string; args?: Record<string, unknown>;
  }>;
  info?: { running?: boolean; cwd?: string; model?: string; provider?: string; title?: string; stored_session_id?: string };
  inflight?: { assistant?: string; streaming?: boolean; user?: string; error?: string } | null;
  open_requests?: RpcFrame[];
  running?: boolean;
}

export function durableSessionId(session: SessionSnapshot, requestedId = ""): string {
  return session.stored_session_id || session.session_key || session.resumed || session.info?.stored_session_id || requestedId;
}

export function gatewayRestarted(events: HermesEvent[], reset = false, liveId = ""): boolean {
  return reset || events.some(event => event.frame.method === "event" && (event.frame.params?.type === "gateway.ready"
    || (event.frame.params?.type === "session.reclaimed" && object(event.frame.params.payload).session_id === liveId)));
}

export function modelSelection(
  inventory: { provider: string; model: string; providers: Array<{ slug: string; models: string[] }> },
  current: { provider: string; model: string },
  hasLiveSession: boolean,
  preferredProvider = "",
): { provider: string; model: string } {
  if (hasLiveSession) return current;
  const selected = inventory.providers.find(row => row.slug === (preferredProvider || current.provider));
  if (!selected) return { provider: inventory.provider || "", model: inventory.model || "" };
  const savedModel = (!preferredProvider || preferredProvider === current.provider) && selected.models.includes(current.model) ? current.model : "";
  return { provider: selected.slug, model: savedModel || selected.models[0] || "" };
}

export function stateFromSession(session: SessionSnapshot): HermesRunState {
  const state = emptyRunState();
  const tools = new Map<string, HermesRunState["activities"][number]>();
  (session.messages || []).forEach((row, index) => {
    if (row.role !== "tool") return;
    const id = row.tool_call_id || `stored-tool-${row.row_id ?? index}`;
    const result = row.text ?? row.content;
    const details = textContent(result) || (result && typeof result === "object" && !Array.isArray(result)
      ? JSON.stringify(result, null, 2) : "");
    // A persisted role:tool row is a completed result, not evidence of an active tool.
    tools.set(id, { id, kind: "tool", text: row.name || t("ui.state.m590d118f9a"), complete: true,
      ...(details ? { details } : {}) });
  });
  state.activities = [...tools.values()];
  state.messages = (session.messages || []).map((row, index) => ({
    id: `stored-${row.row_id ?? index}`, role: row.role || "assistant", content: textContent(row.text ?? row.content),
    ...(row.role === "assistant" && typeof row.reasoning === "string" && row.reasoning ? { reasoning: row.reasoning } : {}),
  })).filter(row => (row.content || row.reasoning) && ["user", "assistant", "agent", "system"].includes(row.role));
  state.busy = session.running === true || session.info?.running === true || session.inflight?.streaming === true;
  state.error = session.inflight?.error || "";
  if (session.inflight?.user && !state.messages.some(row => row.role === "user" && row.content === session.inflight?.user)) {
    state.messages.push({ id: "inflight-user", role: "user", content: session.inflight.user });
  }
  if (session.inflight?.assistant) {
    state.streamId = "inflight-assistant";
    state.messages.push({ id: state.streamId, role: "assistant", content: session.inflight.assistant });
  }
  state.prompts = (session.open_requests || []).map(promptFromFrame).filter((row): row is HermesPrompt => !!row);
  return state;
}
