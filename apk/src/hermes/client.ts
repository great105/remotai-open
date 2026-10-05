/** Hermes uses the same authenticated transport as every Remotai device feature. */
import { transport } from "@tgcontrol/shared";

export interface HermesStatus {
  installed: boolean;
  ownership: "managed" | "external" | "none";
  running: boolean;
  ready: boolean;
  state: string;
  operation?: { kind?: string; stage?: string; message?: string; progress?: number } | string | null;
  operation_detail?: string;
  version?: string;
  latest_version?: string;
  update_channel?: string;
  auto_update: boolean;
  update_pending?: boolean;
  update_available?: boolean;
  last_error?: string;
  data_dir?: string;
  backend_generation?: number;
}

export interface OAuthProvider {
  id: string;
  name: string;
  flow?: string;
  status?: string | { logged_in?: boolean; auth_verified?: boolean; configured?: boolean; source_label?: string };
  authenticated?: boolean;
  available?: boolean;
  reason?: string;
  cli_command?: string;
  docs_url?: string;
}

export interface DeviceLogin {
  session_id: string;
  flow: string;
  user_code: string;
  verification_url: string;
  expires_in: number;
  poll_interval: number;
}

export interface LoginPoll {
  status: "pending" | "approved" | "denied" | "expired" | "error";
  error_message?: string;
}

export interface RpcFrame {
  jsonrpc?: "2.0";
  id?: string | number;
  method?: string;
  params?: Record<string, unknown>;
  result?: unknown;
  error?: { code?: number; message: string };
}

export interface HermesEvent { seq: number; frame: RpcFrame }
export interface EventsPage { events: HermesEvent[]; latest_seq: number; reset?: boolean }
export interface AcceptedOperation { accepted: boolean; operation: string; status: HermesStatus }

export interface RequestTransport {
  request<T>(path: string, init?: RequestInit): Promise<T>;
}

/** Injected guard prevents an in-flight screen from acting on a newly selected device. */
export function createHermesClient(
  source: () => RequestTransport = transport,
  guard: () => void = () => {},
) {
  async function request<T>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    guard();
    const result = await source().request<T>(`/api/hermes${path}`, {
      ...(body === undefined ? {} : { method: "POST", body: JSON.stringify(body) }),
      ...(signal ? { signal } : {}),
    });
    guard();
    return result;
  }
  return {
    status: (signal?: AbortSignal) => request<HermesStatus>("/status", undefined, signal),
    install: async () => (await request<AcceptedOperation>("/install", {})).status,
    start: async () => (await request<AcceptedOperation>("/start", {})).status,
    update: async () => (await request<AcceptedOperation>("/update", {})).status,
    checkUpdate: async () => (await request<AcceptedOperation>("/check-update", {})).status,
    settings: (auto_update: boolean) => request<HermesStatus>("/settings", { auto_update }),
    rpc: <T = Record<string, unknown>>(method: string, params: Record<string, unknown> = {}) =>
      request<T>("/rpc", { method, params }),
    events: (after: number, signal?: AbortSignal) =>
      request<EventsPage>(`/events?after=${encodeURIComponent(after)}`, undefined, signal),
    reply: (id: string | number, result: unknown) => request<{ ok: boolean }>("/reply", { id: String(id), result }),
    providers: () => request<{ providers: OAuthProvider[] }>("/backend/providers/oauth"),
    login: (provider: string) => request<DeviceLogin>(`/backend/providers/oauth/${encodeURIComponent(provider)}/start`, {}),
    pollLogin: (provider: string, session: string) => request<LoginPoll>(
      `/backend/providers/oauth/${encodeURIComponent(provider)}/poll/${encodeURIComponent(session)}`,
    ),
    backend: <T = Record<string, unknown>>(path: string, body?: unknown) => request<T>(`/backend${path}`, body),
  };
}

export type HermesClient = ReturnType<typeof createHermesClient>;
export class HermesUserError extends Error {}

export function safeVerificationUrl(value: string): string | null {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password ? url.href : null;
  } catch { return null; }
}

/** Connected is an explicit upstream fact; mere provider availability is insufficient. */
export function providerConnected(provider: OAuthProvider): boolean {
  return provider.authenticated === true || (typeof provider.status === "object"
    ? provider.status.logged_in === true
    : ["connected", "authenticated", "logged_in", "active"].includes(provider.status || ""));
}

export function runtimeBusy(status: HermesStatus | null): boolean {
  return !!status && (["installing", "updating", "starting", "checking", "checking_update"].includes(status.state) || !!status.operation);
}

export function runtimeLabel(status: HermesStatus | null): string {
  if (!status) return "Проверяем Hermes на компьютере";
  if (status.last_error && !status.ready) return "Нужна помощь с запуском";
  if (status.state === "installing") return "Устанавливаем Hermes";
  if (status.state === "updating") return "Обновляем Hermes";
  if (status.state === "starting") return "Запускаем Hermes";
  if (["checking", "checking_update"].includes(status.state)) return "Проверяем обновление";
  if (!status.installed) return "Hermes еще не установлен";
  if (!status.running || !status.ready) return "Hermes готов к запуску";
  return "Hermes запущен";
}

export interface DraftStorage { getItem(key: string): string | null; setItem(key: string, value: string): void }
export interface HermesDraft { text: string; cwd: string; sessionId: string; provider?: string; model?: string }
const EMPTY_DRAFT: HermesDraft = { text: "", cwd: "", sessionId: "", provider: "", model: "" };

export function draftKey(device: string): string { return `remotai.hermes.draft.v1:${device}`; }

export function readDraft(storage: DraftStorage | null, device: string): HermesDraft {
  try {
    const value: unknown = JSON.parse(storage?.getItem(draftKey(device)) || "null");
    if (!value || typeof value !== "object") return { ...EMPTY_DRAFT };
    const obj = value as Record<string, unknown>;
    return {
      text: typeof obj.text === "string" ? obj.text : "",
      cwd: typeof obj.cwd === "string" ? obj.cwd : "",
      sessionId: typeof obj.sessionId === "string" ? obj.sessionId : "",
      provider: typeof obj.provider === "string" ? obj.provider : "",
      model: typeof obj.model === "string" ? obj.model : "",
    };
  } catch { return { ...EMPTY_DRAFT }; }
}

export function writeDraft(storage: DraftStorage | null, device: string, draft: HermesDraft): void {
  try { storage?.setItem(draftKey(device), JSON.stringify(draft)); } catch { /* Private mode must not block chatting. */ }
}

/** An accepted send may complete after navigating away. Never erase a newer task draft. */
export function acceptDraft(storage: DraftStorage | null, device: string, submitted: string, sessionId: string): void {
  const current = readDraft(storage, device);
  writeDraft(storage, device, { ...current, ...(current.text === submitted ? { text: "" } : {}), ...(sessionId ? { sessionId } : {}) });
}

export interface HermesMessage { id: string; role: string; content: string }
export interface HermesActivity { id: string; kind: string; text: string; details?: string; complete?: boolean }
export interface HermesPrompt {
  id: string | number;
  kind: "approval" | "question" | "request";
  method: string;
  title: string;
  description: string;
  choices: string[];
  params: Record<string, unknown>;
}
export interface HermesRunState {
  messages: HermesMessage[];
  activities: HermesActivity[];
  prompts: HermesPrompt[];
  busy: boolean;
  error: string;
  streamId: string | null;
}

export function emptyRunState(): HermesRunState {
  return { messages: [], activities: [], prompts: [], busy: false, error: "", streamId: null };
}

export function textContent(value: unknown): string {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) return value.map(part => {
    if (typeof part === "string") return part;
    if (part && typeof part === "object" && typeof (part as { text?: unknown }).text === "string") return (part as { text: string }).text;
    return "";
  }).filter(Boolean).join("\n");
  return "";
}

/** Duplicate poll pages are ignored; a restart resets only the event cursor. */
export function unseenEvents(page: EventsPage, cursor: number): HermesEvent[] {
  return (page.events || []).filter(event => Number.isFinite(event.seq) && (page.reset || event.seq > cursor))
    .sort((a, b) => a.seq - b.seq);
}
