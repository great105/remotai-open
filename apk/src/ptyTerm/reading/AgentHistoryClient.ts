export interface AgentHistoryPage {
  source: string; agent: string; version: string; schema: string;
  text: string; next: string; partial: boolean;
}
export interface HistoryTransport { readyState: number; send(data: string): void }

export function parseAgentHistoryPage(value: unknown): AgentHistoryPage | null {
  if (!value || typeof value !== "object") return null;
  const p = value as Record<string, unknown>;
  if (typeof p.source !== "string" || !p.source || p.source.length > 128
    || typeof p.agent !== "string" || !["claude", "codex"].includes(p.agent)
    || typeof p.version !== "string" || p.version.length > 64
    || typeof p.schema !== "string" || !["claude-jsonl-chain-v1", "codex-app-server-turns-v2"].includes(p.schema)
    || typeof p.text !== "string" || p.text.length > 256 * 1024
    || p.next != null && (typeof p.next !== "string" || p.next.length > 8192)
    || typeof p.partial !== "boolean") return null;
  return { source: p.source, agent: p.agent, version: p.version, schema: p.schema,
    text: p.text, next: typeof p.next === "string" ? p.next : "", partial: p.partial };
}

/**
 * Код отказа наверх (T-23): поверхность различает «источник сменился»,
 * «версия агента не поддержана» (с версией) и «недоступно». message = код,
 * чтобы прежние проверки по тексту ошибки оставались верными.
 */
export class AgentHistoryError extends Error {
  constructor(readonly code: string, readonly version?: string) {
    super(code);
    this.name = "AgentHistoryError";
  }
}

/** One explicit request, correlated response, no retry after disconnection. */
export class AgentHistoryClient {
  private sequence = 0;
  private pending: { id: string; timer: ReturnType<typeof setTimeout>; resolve(page: AgentHistoryPage): void; reject(error: Error): void } | null = null;
  reset() { this.fail("history_source_changed"); }
  private fail(error: string, version?: string) {
    const pending = this.pending; this.pending = null;
    if (pending) { clearTimeout(pending.timer); pending.reject(new AgentHistoryError(error, version)); }
  }
  read(transport: HistoryTransport | null, cursor: string): Promise<AgentHistoryPage> {
    if (this.pending || transport?.readyState !== 1) return Promise.reject(new Error("history_unavailable"));
    const id = `history-${++this.sequence}`;
    return new Promise((resolve, reject) => {
      this.pending = { id, resolve, reject, timer: setTimeout(() => this.fail("history_unavailable"), 18000) };
      try { transport.send(JSON.stringify({ t: "agent-history-read", request: id, cursor })); }
      catch { this.fail("history_unavailable"); }
    });
  }
  accept(message: Record<string, unknown>) {
    if (message.t !== "agent-history" || message.v !== 1 || message.request !== this.pending?.id) return;
    const page = parseAgentHistoryPage(message.page);
    if (!page) {
      // Код и версия приходят с сервера: ограничиваем длину, содержимого истории в них нет (I-15).
      const code = typeof message.error === "string" && message.error ? message.error.slice(0, 64) : "history_unsupported_format";
      const version = typeof message.version === "string" || typeof message.version === "number"
        ? String(message.version).slice(0, 64) : undefined;
      this.fail(code, version);
      return;
    }
    const pending = this.pending!; this.pending = null;
    clearTimeout(pending.timer); pending.resolve(page);
  }
}
