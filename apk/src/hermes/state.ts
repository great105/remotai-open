import { emptyRunState, textContent, type HermesEvent, type HermesRunState, type HermesPrompt, type RpcFrame } from "./client";

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
    kind: frame.method === "approval" ? "approval" : frame.method === "clarify" ? "question" : "request",
    method: frame.method,
    title: String(params.title || params.question || (frame.method === "approval" ? "Разрешить действие?" : "Hermes ждет ответа")),
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
  if (type === "message.start") return { ...state, busy: true, error: "", streamId: null };
  if (type === "session.info" && typeof payload.running === "boolean") return { ...state, busy: payload.running };
  if (type === "message.delta") {
    const id = state.streamId || `stream-${event.seq}`;
    const existing = state.messages.find(row => row.id === id);
    const content = (existing?.content || "") + textContent(payload.text);
    return {
      ...state, busy: true, streamId: id,
      messages: existing ? state.messages.map(row => row.id === id ? { ...row, content } : row)
        : [...state.messages, { id, role: "assistant", content }],
    };
  }
  if (type === "message.complete" || type === "message.interim") {
    const id = state.streamId || `assistant-${event.seq}`;
    const content = textContent(payload.text);
    const existing = state.messages.some(row => row.id === id);
    const messages = existing ? state.messages.map(row => row.id === id ? { ...row, content } : row)
      : content ? [...state.messages, { id, role: "assistant", content }] : state.messages;
    return {
      ...state, messages, streamId: null,
      busy: type === "message.interim",
      error: type === "message.complete" && payload.status === "error" ? String(payload.error || payload.failure_reason || "Hermes не смог закончить задачу. Проверьте подключение модели и попробуйте снова.") : state.error,
    };
  }
  if (type === "error") return { ...state, busy: false, error: String(payload.message || "Hermes сообщил об ошибке") };
  if (type === "tool.start" || type === "tool.complete") {
    const id = String(payload.tool_id || event.seq);
    const entry = {
      id, kind: "tool", text: String(payload.summary || payload.preview || payload.name || "Действие Hermes"),
      details: type === "tool.start" ? String(payload.args_text || JSON.stringify(payload.args || {}, null, 2))
        : String(payload.result_text || payload.inline_diff || (payload.result ? JSON.stringify(payload.result, null, 2) : "")),
      complete: type === "tool.complete",
    };
    return { ...state, activities: state.activities.some(row => row.id === id)
      ? state.activities.map(row => row.id === id ? entry : row) : [...state.activities, entry] };
  }
  if (type === "status.update" || type === "notice" || type.startsWith("subagent.")) {
    const text = String(payload.text || payload.message || payload.goal || payload.task || payload.status || "");
    return text ? { ...state, activities: [...state.activities, { id: String(event.seq), kind: type, text }].slice(-100) } : state;
  }
  return state;
}

export interface SessionSnapshot {
  session_id: string;
  stored_session_id?: string;
  session_key?: string;
  resumed?: string;
  messages?: Array<{ role?: string; content?: unknown; text?: string; row_id?: number }>;
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
  state.messages = (session.messages || []).map((row, index) => ({
    id: `stored-${row.row_id ?? index}`, role: row.role || "assistant", content: textContent(row.text ?? row.content),
  })).filter(row => row.content && ["user", "assistant", "agent", "system"].includes(row.role));
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
