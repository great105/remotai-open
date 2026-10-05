import { t } from "@tgcontrol/shared";
/** Published lifecycle from the session-owned gateway roster, never inferred from reply text. */
export type HermesSubagentStatus = "running" | "completed" | "failed" | "cancelled" | "unknown";

export interface HermesSubagent {
  id: string;
  goal: string;
  status: HermesSubagentStatus;
  toolCount: number;
  lastTool?: string;
  /** Native Unix seconds: child start, never last activity. */
  startedAt?: number;
  /** Retains queued/pending so the UI can distinguish waiting from executing. */
  rawStatus?: string;
}

export function safeSubagentToolName(value: unknown): string | undefined {
  return typeof value === "string" && /^[A-Za-z][A-Za-z0-9_.:-]{0,95}$/.test(value) ? value : undefined;
}

export function normalizeHermesSubagentStatus(value: unknown): HermesSubagentStatus {
  switch (value) {
    case "queued":
    case "pending":
    case "running": return "running";
    case "completed": return "completed";
    case "failed":
    case "error":
    case "timeout": return "failed";
    case "interrupted":
    case "cancelled":
    case "canceled": return "cancelled";
    default: return "unknown";
  }
}

export function isTerminalHermesSubagentStatus(status: HermesSubagentStatus): boolean {
  return status === "completed" || status === "failed" || status === "cancelled";
}

/**
 * Only a well-formed successful subagent.list response can establish an empty roster.
 * Callers keep null/unknown on unsupported RPCs, transport failures or malformed results.
 */
export function normalizeHermesSubagents(value: unknown): HermesSubagent[] {
  const result = value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : null;
  if (!result || !Array.isArray(result.subagents)) throw new Error(t("ui.subagents.mc2672d6108"));
  const seen = new Set<string>();
  return result.subagents.map(item => {
    const row = item && typeof item === "object" && !Array.isArray(item)
      ? item as Record<string, unknown> : null;
    const id = typeof row?.subagent_id === "string" ? row.subagent_id : "";
    if (!row || !id || id.trim() !== id || id.length > 256 || seen.has(id)) throw new Error(t("ui.subagents.mc2672d6108"));
    seen.add(id);
    const toolCount = typeof row.tool_count === "number" && Number.isSafeInteger(row.tool_count) && row.tool_count >= 0
      ? row.tool_count : 0;
    return {
      id,
      goal: typeof row.goal === "string" ? row.goal : "",
      status: normalizeHermesSubagentStatus(row.status),
      toolCount,
      ...(safeSubagentToolName(row.last_tool) ? { lastTool: safeSubagentToolName(row.last_tool) } : {}),
      ...(typeof row.started_at === "number" && Number.isFinite(row.started_at) && row.started_at >= 0
        && row.started_at <= 8640000000000 ? { startedAt: row.started_at } : {}),
      ...(typeof row.status === "string" ? { rawStatus: row.status } : {}),
    };
  });
}
