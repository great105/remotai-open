import { t } from "@tgcontrol/shared";
/** Hermes uses the same authenticated transport as every Remotai device feature. */
import { transport } from "@tgcontrol/shared";
import type { ControlSnapshot, ControlCapabilities, SubmitTask, TaskReceipt } from "./control";

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
  auto_start?: boolean;
  delivery_enabled?: boolean;
  delivery_ready?: boolean;
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
  async function backendRequest<T>(path: string, method = "GET", body?: unknown, signal?: AbortSignal): Promise<T> {
    guard();
    const result = await source().request<T>(`/api/hermes/backend${path}`, {
      method,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      ...(signal ? { signal } : {}),
    });
    guard();
    return result;
  }
  return {
    status: (signal?: AbortSignal) => request<HermesStatus>("/status", undefined, signal),
    readAttention: (id:string) => request<{ok:boolean}>("/control/read", {id}),
    artifact: (id:string) => request<{name:string;base64:string}>(`/artifact?id=${encodeURIComponent(id)}&format=json`),
    controlReadiness: (session_id:string) => request<{generation:number;tools:{status:string;total?:number;remediation?:string;sections?:Array<{name:string;tools:Array<{name:string;description:string}>}>};model_quota:string}>("/control/readiness",{session_id}),
    controlSnapshot: () => request<ControlSnapshot>("/control/snapshot"),
    controlCapabilities: () => request<ControlCapabilities>("/control/capabilities"),
    submitTask: (task: SubmitTask) => request<TaskReceipt>("/control/submit", task),
    controlIntent: (intent: "steer" | "interrupt", session_id: string, text?: string) => request<{status?: string}>("/control/intent", {intent,session_id,...(text ? {text} : {})}),
    controlReply: (id: string, result: unknown) => request<{ok:boolean}>("/control/reply", {id,result}),
    controlSettings: (settings: { auto_start?:boolean; delivery_enabled?:boolean }) => request<HermesStatus>("/settings", settings),
    install: async () => (await request<AcceptedOperation>("/install", {})).status,
    start: async () => (await request<AcceptedOperation>("/start", {})).status,
    update: async () => (await request<AcceptedOperation>("/update", {})).status,
    checkUpdate: async () => (await request<AcceptedOperation>("/check-update", {})).status,
    settings: (auto_update: boolean) => request<HermesStatus>("/settings", { auto_update }),
    rpc: <T = Record<string, unknown>>(method: string, params: Record<string, unknown> = {}) =>
      request<T>("/rpc", { method, params }),
    subagentActivity: (sessionId: string, subagentId: string, signal?: AbortSignal) =>
      request<unknown>(`/subagents/activity?session_id=${encodeURIComponent(sessionId)}&subagent_id=${encodeURIComponent(subagentId)}`, undefined, signal),
    eventCursor: (signal?: AbortSignal) => request<EventsPage>("/events?cursor=1", undefined, signal),
    events: (after: number, signal?: AbortSignal) =>
      request<EventsPage>(`/events?after=${encodeURIComponent(after)}`, undefined, signal),
    waitEvents: async (after: number, signal?: AbortSignal, waitMs = 20000) => {
      if (!Number.isSafeInteger(waitMs) || waitMs < 0) throw new RangeError("Invalid Hermes event wait");
      return request<EventsPage>(`/events?after=${encodeURIComponent(after)}&wait_ms=${Math.min(waitMs, 20000)}`, undefined, signal);
    },
    reply: (id: string | number, result: unknown) => request<{ ok: boolean }>("/reply", { id: String(id), result }),
    providers: () => request<{ providers: OAuthProvider[] }>("/backend/providers/oauth"),
    login: (provider: string) => request<DeviceLogin>(`/backend/providers/oauth/${encodeURIComponent(provider)}/start`, {}),
    pollLogin: (provider: string, session: string) => request<LoginPoll>(
      `/backend/providers/oauth/${encodeURIComponent(provider)}/poll/${encodeURIComponent(session)}`,
    ),
    backend: <T = Record<string, unknown>>(path: string, body?: unknown) => request<T>(`/backend${path}`, body),
    backendRequest,
  };
}

export type HermesClient = ReturnType<typeof createHermesClient>;
export class HermesUserError extends Error {}

/** Explicit UTC instant for a one-off job; repeating jobs use upstream intervals. */
export function scheduledTaskTime(repeat: string, when: string, now = Date.now()): string {
  if (repeat !== "once") return repeat;
  const date = new Date(when);
  if (!when || Number.isNaN(date.valueOf()) || date.valueOf() <= now) throw new HermesUserError(t("ui.client.mc9f0a455a4"));
  return date.toISOString();
}

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
  if (!status) return t("ui.client.mc4ec82ece3");
  if (status.last_error && !status.ready) return t("ui.client.m3561553bb4");
  if (status.state === "installing") return t("ui.client.ma957a75478");
  if (status.state === "updating") return t("ui.client.m9ae25dfad4");
  if (status.state === "starting") return t("ui.client.m697979637a");
  if (["checking", "checking_update"].includes(status.state)) return t("ui.client.mc599bae5fe");
  if (!status.installed) return t("ui.client.mf07f9a6d25");
  if (!status.running || !status.ready) return t("ui.client.m25ca8dc196");
  return t("ui.client.m100f25f044");
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

export interface StoredSession {
  id: string;
  title: string;
  preview: string;
  message_count: number;
  started_at: number;
  cwd?: string | null;
  git_repo_root?: string | null;
}

export interface HermesMessage { id: string; role: string; content: string; reasoning?: string }
export interface HermesActivity {
  id: string;
  kind: string;
  text: string;
  details?: string;
  complete?: boolean;
  subagentId?: string;
  status?: import("./subagents").HermesSubagentStatus;
}
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
  progressText: string;
}

export function emptyRunState(): HermesRunState {
  return { messages: [], activities: [], prompts: [], busy: false, error: "", streamId: null, progressText: "" };
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
