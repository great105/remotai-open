// Экран «Беседы»: все прошлые беседы Claude Code и Codex на компьютере по всем
// аккаунтам (GET /api/agent-sessions, internal/agentsessions на Go).
//
// Отдельным файлом, а не в api-endpoints.ts: тип ответа и обёртка живут рядом,
// и параллельные правки большого файла эндпоинтов не конфликтуют с этой.

import { transport } from "./api-transport";

/** Одна беседа. Текста беседы здесь нет — только заголовок и приметы. */
export interface AgentSessionItem {
  agent: "claude" | "codex" | string;
  session_id: string;
  /** Аккаунт CLI (каталог), в котором лежит беседа; "default" — основной. */
  account_id: string;
  account_label?: string;
  cwd: string;
  /** Первое настоящее сообщение человека (~120 символов) или ai-title. */
  title: string;
  /** Последняя запись в беседу, unix ms. */
  updated_at: number;
  /** Сообщения человека и ответы агента. Нет поля — ещё не посчитано. */
  messages?: number;
  /** Беседа идёт или спит в этом терминале Remotai — открывать его. */
  open_pty_id?: string;
  sleeping?: boolean;
  /** Claude держит беседу открытой вне Remotai (другое окно на ПК). */
  running?: boolean;
}

export interface AgentSessionsPage {
  sessions: AgentSessionItem[];
  /** Курсор следующей страницы. */
  next?: string;
  /** Сколько бесед подходит под поиск (по всем агентам, если agent не задан). */
  total: number;
  /** Сколько подходит у каждого агента — для переключателя. */
  agents?: Record<string, number>;
  /** Компьютер не успел разобрать все файлы — повторить чуть позже. */
  partial?: boolean;
  /** Фоновый подсчёт сообщений ещё идёт. */
  counting?: boolean;
}

export interface AgentSessionsQuery {
  q?: string;
  agent?: string;
  limit?: number;
  cursor?: string;
}

export function getAgentSessions(query: AgentSessionsQuery = {}, init?: RequestInit): Promise<AgentSessionsPage> {
  const params = new URLSearchParams();
  if (query.q) params.set("q", query.q);
  if (query.agent) params.set("agent", query.agent);
  if (query.limit) params.set("limit", String(query.limit));
  if (query.cursor) params.set("cursor", query.cursor);
  const qs = params.toString();
  return transport().request<AgentSessionsPage>(`/api/agent-sessions${qs ? `?${qs}` : ""}`, init);
}
