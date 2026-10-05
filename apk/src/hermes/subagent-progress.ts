import type { HermesEvent } from "./client";
import { isTerminalHermesSubagentStatus, normalizeHermesSubagentStatus, safeSubagentToolName, type HermesSubagent } from "./subagents";
export interface SubagentJournalEntry { id: string; kind: "tool_start" | "tool_result"; toolName?: string; receivedAt?: number; sourceTimeText?: string; status?: "ok" | "error"; durationSeconds?: number }
export interface SubagentObservation extends HermesSubagent { observedStarts?: number; present: boolean; terminalConfirmed?: boolean; checkedAt?: number; receivedAt?: number; partial: boolean; journal: SubagentJournalEntry[]; nativeJournal: SubagentJournalEntry[]; nativeState: "unchecked" | "supported" | "unsupported" | "unavailable" | "error"; capturedAt?: number }
export interface SubagentProgressState { namespace: string; liveId: string; sequence: number; children: SubagentObservation[] }
export function normalizeNativeSubagentActivity(value: unknown): { state: SubagentObservation["nativeState"]; entries: SubagentJournalEntry[]; partial: boolean; capturedAt?: number } {
  const v = object(value);
  if (v.supported === false) return { state: "unsupported", entries: [], partial: true };
  if (v.supported !== true || v.source !== "native_live_log" || v.source_time_zone !== "unknown" || !Array.isArray(v.entries)) return { state: "error", entries: [], partial: true };
  if (v.available !== true) return { state: "unavailable", entries: [], partial: true };
  const entries: SubagentJournalEntry[] = [];
  const seen = new Set<string>();
  for (const raw of v.entries.slice(-200)) {
    const row = object(raw), toolName = safeSubagentToolName(row.tool_name);
    if (!toolName || typeof row.id !== "string" || !row.id || row.id.length > 256 || seen.has(row.id)
      || !["tool_start", "tool_result"].includes(String(row.kind)) || typeof row.source_time_text !== "string"
      || !/^(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d$/.test(row.source_time_text)) continue;
    const kind = row.kind as SubagentJournalEntry["kind"];
    if (kind === "tool_result" && row.status !== "ok" && row.status !== "error") continue;
    seen.add(row.id);
    entries.push({ id: row.id, kind, toolName, sourceTimeText: row.source_time_text,
      ...(kind === "tool_result" ? { status: row.status as "ok" | "error" } : {}),
      ...(kind === "tool_result" && typeof row.duration_seconds === "number" && Number.isFinite(row.duration_seconds) && row.duration_seconds >= 0 && row.duration_seconds <= 604800 ? { durationSeconds: row.duration_seconds } : {}) });
  }
  return { state: "supported", entries, partial: v.truncated === true || v.history_incomplete !== false || v.entries.length > 200 || entries.length !== v.entries.length,
    ...(typeof v.captured_at_ms === "number" && Number.isFinite(v.captured_at_ms) && v.captured_at_ms >= 0 && v.captured_at_ms <= 8640000000000000 ? { capturedAt: v.captured_at_ms } : {}) };
}
function object(value: unknown): Record<string, unknown> { return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {}; }
export function emptySubagentProgress(namespace: string, liveId: string): SubagentProgressState { return { namespace, liveId, sequence: 0, children: [] }; }
/** Compact selection is separate from retained journal history. */
export function compactSubagentObservation(children: SubagentObservation[], roster: HermesSubagent[] | null): SubagentObservation | undefined {
  return children.filter(child => !child.terminalConfirmed && !isTerminalHermesSubagentStatus(child.status)
    && (!child.present || roster?.some(row => row.id === child.id && row.status === "running")))
    .sort((a,b) => (b.receivedAt ?? 0) - (a.receivedAt ?? 0))[0];
}
export function observeSubagentRoster(state: SubagentProgressState, rows: HermesSubagent[], receivedAt: number): SubagentProgressState {
  const children: SubagentObservation[] = rows.slice(0, 32).map(row => {
    const old = state.children.find(child => child.id === row.id);
    return { ...newObservation(row), ...old, ...row, startedAt: row.startedAt ?? old?.startedAt,
      status: old?.terminalConfirmed ? old.status : row.status, present: true, checkedAt: receivedAt };
  });
  for (const old of state.children) if (!children.some(child => child.id === old.id)) children.push({ ...old, present: false });
  return { ...state, children: children.slice(0, 32) };
}
function newObservation(row: HermesSubagent): SubagentObservation {
  return { ...row, present: true, partial: true, journal: [], nativeJournal: [], nativeState: "unchecked" };
}
export function observeSubagentEvent(state: SubagentProgressState, event: HermesEvent, receivedAt: number): SubagentProgressState {
  const params = event.frame.params;
  if (event.frame.method !== "event" || params?.session_id !== state.liveId || event.seq <= state.sequence) return state;
  const gap = state.sequence > 0 && event.seq > state.sequence + 1;
  const children = gap ? state.children.map(child => ({ ...child, partial: true })) : state.children;
  const next = { ...state, sequence: event.seq, children };
  const type = params.type;
  if (!["subagent.start", "subagent.tool", "subagent.complete"].includes(String(type))) return next;
  const payload = params.payload as Record<string, unknown> | undefined;
  if (!payload || typeof payload.subagent_id !== "string" || !payload.subagent_id || payload.subagent_id.length > 256) return next;
  const old = children.find(child => child.id === payload.subagent_id);
  if (old?.terminalConfirmed && type !== "subagent.complete") return next;
  const child = old ?? newObservation({ id: payload.subagent_id, goal: typeof payload.goal === "string" ? payload.goal : "", status: "unknown", toolCount: 0 });
  const updated = { ...child, receivedAt, partial: gap || (type === "subagent.start" ? false : child.partial) };
  if (type === "subagent.complete") {
    updated.status = normalizeHermesSubagentStatus(payload.status);
    updated.terminalConfirmed = true;
  } else if (type === "subagent.start") updated.status = "running";
  else {
    const toolName = safeSubagentToolName(payload.tool_name);
    updated.lastTool = toolName;
    updated.observedStarts = (child.observedStarts ?? 0) + 1;
    updated.toolCount = Math.max(child.toolCount, typeof payload.tool_count === "number" && Number.isSafeInteger(payload.tool_count) && payload.tool_count >= 0 ? payload.tool_count : updated.observedStarts);
    const remaining = Math.max(0,200-child.nativeJournal.length);
    updated.journal = remaining ? [...child.journal, { id: `event:${event.seq}`, kind: "tool_start" as const, toolName, receivedAt }].slice(-remaining) : [];
    if (child.journal.length >= remaining) updated.partial = true;
  }
  return { ...next, children: old ? children.map(row => row.id === updated.id ? updated : row) : [...children, updated].slice(-32) };
}
