import { t } from "@tgcontrol/shared";
/** Cloud relay API for APK. Authorization via Bearer JWT (no Telegram WebApp wrapper here).
 *  Note: релей принимает и Bearer JWT, и Telegram `tma initData` на account-эндпоинтах
 *  (см. tgcontrol-relay requireUserAuth), поэтому те же функции переиспользуемы и в
 *  Telegram-режиме — нужно лишь подставить tma-заголовок (Этап E). */
import { ERR_NETWORK_CLOUD, fetchOrNetworkError, isNetworkFailure } from "@tgcontrol/shared";
import {
  getCloudJWT, getRelayBase, getSelectedDeviceId, isNativeApp,
  noteSelectedDeviceAgentVersion, saveConfig,
} from "../config";
import { tlog } from "../debuglog";

export class CloudError extends Error {
  status: number;
  code?: string;
  details?: Record<string, unknown>;
  constructor(status: number, msg: string, code?: string, details?: Record<string, unknown>) {
    super(msg);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export type WorkspaceKind = "personal" | "company";
export type WorkspaceRole = "owner" | "admin" | "operator" | "viewer";
export type DeviceType = "computer" | "server";

export interface CloudZone {
  id: string;
  workspace_id: string;
  name: string;
  created_at?: string;
}

/** @deprecated Historical API name. New UI and endpoints use CloudZone. */
export type CloudDeviceGroup = CloudZone;

export interface CloudTag {
  id: string;
  workspace_id: string;
  name: string;
  color: string;
  created_at?: string;
}

/** Управляемое устройство аккаунта: компьютер или сервер. */
export interface CloudDevice {
  id: string;
  name: string;
  hostname: string;
  platform: "windows" | "linux" | "darwin" | string;
  agent_version: string;
  online: boolean;
  last_seen_at: string | null;
  paired_at: string;
  workspace_id: string;
  workspace_name: string;
  workspace_role: WorkspaceRole;
  device_type: DeviceType;
  zone?: CloudZone | null;
  group: CloudZone | null;
  tags: CloudTag[];
  favorite: boolean;
}

/** Accept both the v2.33.2 zone field and the legacy group field during rollout. */
export function deviceZone(device: CloudDevice): CloudZone | null {
  return device.zone ?? device.group ?? null;
}

export interface CloudWorkspace {
  id: string;
  kind: WorkspaceKind;
  name: string;
  role: WorkspaceRole;
  owner_user_id: number;
  member_count: number;
  device_count: number;
  online_count: number;
  max_devices?: number;
  created_at: string;
}

export interface CloudWorkspaceMember {
  user_id: number;
  role: WorkspaceRole;
  username?: string;
  first_name?: string;
  display?: string;
  joined_at: string;
}

export interface CloudAuditEvent {
  id: string;
  actor?: string;
  actor_user_id?: number;
  action: string;
  target_type: string;
  target_id: string;
  metadata?: Record<string, unknown> | null;
  created_at: string;
}

export interface CloudLoginSession {
  id: string;
  client_kind: "android" | "telegram" | "web" | string;
  client_name: string;
  ip_address?: string;
  created_at: string;
  last_seen_at: string;
  expires_at?: string;
  current: boolean;
}

/** Профиль пользователя облака + квота устройств. telegram_id отрицателен у
 *  анонимного (не-Telegram) аккаунта, положителен у вошедшего через Telegram. */
export interface CloudMe {
  id: number;
  telegram_id?: number;
  username?: string;
  first_name?: string;
  tier: "free" | "pro" | "team" | string;
  effective_tier?: "free" | "pro" | "team" | string;
  /** Вечный Pro нынешним (грандфазеринг). Перебивает всё. */
  founder?: boolean;
  /** Сколько целых дней осталось у пробного Pro (0/undefined — триала нет). */
  trial_days_left?: number;
  /** Момент окончания пробного Pro (ISO). */
  trial_end?: string;
  /**
   * Работает ли удалённый доступ прямо сейчас. Решает релей (PROD-008): он видит
   * каждое облачное соединение и он же его пускает. Клиенту остаётся показать.
   */
  cloud_allowed?: boolean;
  beta?: boolean;
  billing_enabled?: boolean;
  self_hosted?: boolean;
  devices_count: number;
  max_devices: number;
  permanent?: boolean;
  identities_count?: number;
  login_provider?: string;
  login_display?: string;
  /** Данные пришли с релея в этот момент (свежие). */
  fetched_at?: number;
  /** Данные прочитаны из кэша, потому что облако не ответило (см. getMe). */
  cached_at?: number;
}

/** Заголовок авторизации account-эндпоинтов: в Telegram — подписанный initData
 *  (`tma …`), иначе — Bearer user-JWT. Релей принимает оба (requireUserAuth),
 *  поэтому список ПК/профиль работают и в Telegram Mini App, и в APK/web/exe. */
function accountAuthHeader(jwt: string): string | null {
  const tg = (window as { Telegram?: { WebApp?: { initData?: string } } }).Telegram?.WebApp;
  // В Telegram полученный JWT предпочтительнее initData: тот живёт 24 часа с
  // запуска мини-аппа, а WebView Telegram держит сутками (см. miniAppAuth.ts).
  if (tg?.initData && !jwt) return `tma ${tg.initData}`;
  return jwt ? `Bearer ${jwt}` : null;
}

/**
 * `fetch` к релею, у которого сетевой сбой уже человеческий. Без обёртки любой
 * пропавший Wi-Fi, VPN, рвущий TLS, или недоступный релей доезжали до аккаунтных
 * экранов сырым «Failed to fetch» (находка V3). Типизированную ошибку shared
 * переносим в CloudError: вызывающие ветвятся по `instanceof CloudError` и его
 * status (probeCloudAuthLost, экраны входа и инфраструктуры), поэтому класс
 * ошибки на этом слое должен остаться прежним.
 */
async function cloudFetch(url: string, init?: RequestInit): Promise<Response> {
  try {
    return await fetchOrNetworkError(url, init, "cloud");
  } catch (e) {
    if (isNetworkFailure(e)) throw new CloudError(0, t("ui.api.m0133aa7d59"), ERR_NETWORK_CLOUD);
    throw e;
  }
}

function utf8ToBase64(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/**
 * Каким клиентом мы представляемся релею в списке «Входы в аккаунт».
 *
 * Окно Remotai на компьютере — это WebView2, и по User-Agent оно неотличимо от
 * Edge: в списке входов оно называлось «Edge на Windows», как посторонний
 * браузер (живой вопрос владельца 2026-07-30 — «что это за веб-сессии?»).
 * Признак окна — клиент открыт по петле: панель ПК и встроенный клиент живут
 * только там.
 */
function clientKindHeader(): string {
  const inTelegram = !!(window as { Telegram?: { WebApp?: { initData?: string } } }).Telegram?.WebApp?.initData;
  if (inTelegram) return "telegram";
  if (isNativeApp) return "android";
  if (typeof window !== "undefined") {
    const host = window.location.hostname.toLowerCase().replace(/^\[(.*)\]$/, "$1");
    if (host === "localhost" || host === "127.0.0.1" || host === "::1" || host === "0:0:0:0:0:0:0:1") {
      return "desktop";
    }
  }
  return "web";
}

/** Account-запрос к релею (профиль/список/переименование/отзыв ПК). */
async function cloudUserFetch<T>(path: string, init: RequestInit = {}): Promise<T> {
  const base = getRelayBase().replace(/\/+$/, "");
  const headers = new Headers(init.headers ?? {});
  const jwt = getCloudJWT();
  const auth = accountAuthHeader(jwt);
  if (auth) headers.set("Authorization", auth);
  headers.set("X-Remotai-Client", clientKindHeader());
  if (!headers.has("Content-Type") && init.body) headers.set("Content-Type", "application/json");
  const res = await cloudFetch(base + path, { ...init, headers });
  const text = await res.text();
  let body: unknown = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { raw: text }; }
  if (!res.ok) {
    const errorBody = body as { error?: string; code?: string } | null;
    const msg = errorBody?.error ?? res.statusText;
    throw new CloudError(res.status, msg, errorBody?.code, (body && typeof body === "object") ? body as Record<string, unknown> : undefined);
  }
  // Скользящее продление: релей кладёт свежий user-JWT в ответ account-эндпоинтов
  // (только для Bearer-аккаунтов). Сохраняем — иначе анонимный токен жёстко
  // протух бы на JWT_TTL без обновления, телефон получил бы новый аккаунт, а ПК
  // остался бы за старым → 409 при ре-пейринге. См. relay slidingUserJWT.
  const refreshed = (body as { refreshed_jwt?: string; refreshed_expires_at?: string })?.refreshed_jwt;
  if (refreshed && refreshed !== jwt) {
    const expRaw = (body as { refreshed_expires_at?: string })?.refreshed_expires_at;
    saveConfig({ jwt: refreshed, jwtExpiresAt: (expRaw && Date.parse(expRaw)) || undefined });
  }
  return body as T;
}

/**
 * Подключить ПК из Telegram Mini App.
 *
 * Использует `POST /v1/pair/confirm`, который авторизуется подписанным initData
 * (`Authorization: tma …`) — то есть ПК привязывается именно к этому
 * Telegram-аккаунту. Раньше в Telegram кнопка «Добавить компьютер» просто
 * закрывала мини-апп: единственный клиентский путь (`pairNative` →
 * confirm-native) ходит с Bearer-JWT, которого в Telegram нет, поэтому релей
 * создал бы АНОНИМНЫЙ аккаунт и ПК уехал бы не туда.
 *
 * ВАЖНО: поле `jwt` в ответе — это DEVICE-токен для самого ПК (он забирает его
 * через /v1/pair/status), а НЕ user-JWT. Сохранять его в конфиг нельзя, иначе
 * весь аккаунтный слой ляжет — поэтому наружу отдаём только device_id.
 */
export async function pairWithTelegram(
  code: string,
  options: { workspaceId?: string; deviceType?: DeviceType; zoneId?: string } = {},
): Promise<{ device_id: string }> {
  const r = await cloudUserFetch<{ ok?: boolean; device_id: string }>("/v1/pair/confirm", {
    method: "POST",
    body: JSON.stringify({
      code: code.trim().toUpperCase(),
      workspace_id: options.workspaceId || undefined,
      device_type: options.deviceType || undefined,
      zone_id: options.zoneId || undefined,
    }),
  });
  return { device_id: r.device_id };
}

/** Профиль текущего аккаунта (тариф, квота устройств). */
const ME_CACHE_KEY = "remotai.cloud.me.v1";

export function getCachedMe(): CloudMe | null {
  try {
    const parsed = JSON.parse(localStorage.getItem(ME_CACHE_KEY) || "null") as CloudMe | null;
    return parsed && typeof parsed.id === "number" ? parsed : null;
  } catch {
    return null;
  }
}

/**
 * Профиль аккаунта. При успехе штампуем `fetched_at` («данные свежие»), а
 * `cached_at` выставляем ТОЛЬКО в ветке фолбэка на localStorage.
 *
 * Раньше штамп ставился на каждый удачный /v1/me, и в подписке всегда висело
 * «Данные аккаунта · на 15:32» — метка кэша читалась как «устарело», а на
 * настоящем офлайне выглядела ровно так же (N171).
 */
export async function getMe(): Promise<CloudMe> {
  try {
    const me = await cloudUserFetch<CloudMe>("/v1/me");
    const fresh: CloudMe = { ...me, fetched_at: Date.now(), cached_at: undefined };
    try { localStorage.setItem(ME_CACHE_KEY, JSON.stringify(fresh)); } catch { /* quota/private mode */ }
    return fresh;
  } catch (error) {
    const cached = getCachedMe();
    // Время в метке — момент, когда данные ПОЛУЧИЛИ, а не когда достали из кэша.
    if (cached) return { ...cached, cached_at: cached.fetched_at ?? cached.cached_at ?? Date.now() };
    throw error;
  }
}

/** Список всех ПК аккаунта (для мульти-устройство выбора).
 *
 *  Заодно освежает слепок версии агента ТЕКУЩЕЙ машины: от него зависит, шлём
 *  ли мы `resume` (см. noteSelectedDeviceAgentVersion и ptyWSUrl). Место
 *  выбрано как единственная воронка — списком устройств пользуются и главная,
 *  и «Мои компьютеры», и сводка, поэтому слепок освежается сам, без нового
 *  запроса и без правок в каждом экране. */
export async function listDevices(): Promise<{ devices: CloudDevice[] }> {
  const res = await cloudUserFetch<{ devices: CloudDevice[] }>("/v1/devices");
  const selected = getSelectedDeviceId();
  if (selected) {
    const mine = (res.devices || []).find((d) => d.id === selected);
    if (mine) noteSelectedDeviceAgentVersion(selected, mine.agent_version);
  }
  return res;
}

export function listWorkspaces(): Promise<{ workspaces: CloudWorkspace[] }> {
  return cloudUserFetch<{ workspaces: CloudWorkspace[] }>("/v1/workspaces");
}

export function createWorkspace(name: string): Promise<{ workspace: CloudWorkspace }> {
  return cloudUserFetch<{ workspace: CloudWorkspace }>("/v1/workspaces", {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

export function renameWorkspace(id: string, name: string): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify({ name }),
  });
}

export function archiveWorkspace(id: string): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export function acceptWorkspaceInvite(code: string): Promise<{ workspace: CloudWorkspace }> {
  return cloudUserFetch<{ workspace: CloudWorkspace }>("/v1/workspace-invites/accept", {
    method: "POST",
    body: JSON.stringify({ code: code.trim().toUpperCase() }),
  });
}

export function listWorkspaceMembers(id: string): Promise<{ members: CloudWorkspaceMember[] }> {
  return cloudUserFetch<{ members: CloudWorkspaceMember[] }>(
    `/v1/workspaces/${encodeURIComponent(id)}/members`,
  );
}

export function createWorkspaceInvite(
  id: string,
  role: Exclude<WorkspaceRole, "owner">,
): Promise<{ code: string; role: WorkspaceRole; expires_at: string }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/invites`, {
    method: "POST",
    body: JSON.stringify({ role }),
  });
}

export function updateWorkspaceMember(
  workspaceId: string,
  userId: number,
  role: Exclude<WorkspaceRole, "owner">,
): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/members/${encodeURIComponent(String(userId))}`,
    { method: "PATCH", body: JSON.stringify({ role }) },
  );
}

export function removeWorkspaceMember(workspaceId: string, userId: number): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/members/${encodeURIComponent(String(userId))}`,
    { method: "DELETE" },
  );
}

export function listWorkspaceGroups(id: string): Promise<{ groups: CloudDeviceGroup[] }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/groups`);
}

export function createWorkspaceGroup(id: string, name: string): Promise<{ group: CloudDeviceGroup }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/groups`, {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

export function deleteWorkspaceGroup(workspaceId: string, groupId: string): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/groups/${encodeURIComponent(groupId)}`,
    { method: "DELETE" },
  );
}

export function listWorkspaceZones(id: string): Promise<{ zones: CloudZone[] }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/zones`);
}

export function createWorkspaceZone(id: string, name: string): Promise<{ zone: CloudZone }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/zones`, {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

export function renameWorkspaceZone(workspaceId: string, zoneId: string, name: string): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/zones/${encodeURIComponent(zoneId)}`,
    { method: "PATCH", body: JSON.stringify({ name }) },
  );
}

export function deleteWorkspaceZone(workspaceId: string, zoneId: string): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/zones/${encodeURIComponent(zoneId)}`,
    { method: "DELETE" },
  );
}

export function listWorkspaceTags(id: string): Promise<{ tags: CloudTag[] }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/tags`);
}

export function createWorkspaceTag(
  id: string,
  name: string,
  color: string,
): Promise<{ tag: CloudTag }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/tags`, {
    method: "POST",
    body: JSON.stringify({ name, color }),
  });
}

export function deleteWorkspaceTag(workspaceId: string, tagId: string): Promise<{ ok: true }> {
  return cloudUserFetch(
    `/v1/workspaces/${encodeURIComponent(workspaceId)}/tags/${encodeURIComponent(tagId)}`,
    { method: "DELETE" },
  );
}

export function listWorkspaceAudit(id: string): Promise<{ events: CloudAuditEvent[] }> {
  return cloudUserFetch(`/v1/workspaces/${encodeURIComponent(id)}/audit?limit=100`);
}

/**
 * Выполнить обычный agent-API запрос на конкретном компьютере, не меняя
 * выбранный контекст приложения. Нужен сводке нескольких машин: временно
 * переключать глобальный selectedDeviceId ради fan-out опасно — WS и рабочие
 * экраны в этот момент ушли бы на чужой компьютер.
 */
export async function requestDevice<T>(
  deviceId: string,
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const base = getRelayBase().replace(/\/+$/, "");
  const auth = accountAuthHeader(getCloudJWT());
  if (!auth) throw new CloudError(401, t("ui.api.m926eefca4d"));
  const [rawPath, rawQuery = ""] = path.split("?");
  const query: Record<string, string> = {};
  new URLSearchParams(rawQuery).forEach((value, key) => { query[key] = value; });
  const body = typeof init.body === "string" ? init.body : "";
  const controller = new AbortController();
  const external = init.signal;
  const abort = () => controller.abort();
  if (external?.aborted) controller.abort();
  else external?.addEventListener("abort", abort, { once: true });
  const timeout = window.setTimeout(() => controller.abort(), 15_000);
  try {
    const res = await cloudFetch(`${base}/v1/client/${encodeURIComponent(deviceId)}/request`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: auth },
      body: JSON.stringify({
        method: (init.method || "GET").toUpperCase(),
        path: rawPath,
        query: Object.keys(query).length ? query : undefined,
        body: body ? utf8ToBase64(body) : undefined,
        headers: { "Content-Type": "application/json" },
      }),
      signal: controller.signal,
    });
    const text = await res.text();
    let payload: any = null;
    try { payload = text ? JSON.parse(text) : null; } catch { payload = { error: text || res.statusText }; }
    if (!res.ok) {
      throw new CloudError(
        res.status,
        payload?.error || res.statusText,
        payload?.code,
        payload && typeof payload === "object" ? payload : undefined,
      );
    }
    return payload as T;
  } catch (error: any) {
    if (error?.name === "AbortError") {
      throw new CloudError(0, external?.aborted ? t("ui.api.m542bd26f07") : t("ui.api.m7ff4e933d5"), "timeout");
    }
    throw error;
  } finally {
    window.clearTimeout(timeout);
    external?.removeEventListener("abort", abort);
  }
}

/** Переименовать ПК. */
export function renameDevice(id: string, name: string): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/devices/${encodeURIComponent(id)}/rename`, {
    method: "POST",
    body: JSON.stringify({ name }),
  });
}

/** Отвязать ПК от аккаунта. */
export function revokeDevice(id: string): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/devices/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export interface DeviceInfrastructurePatch {
  name?: string;
  device_type?: DeviceType;
  workspace_id?: string;
  zone_id?: string;
  /** @deprecated Use zone_id. */
  group_id?: string;
}

export function updateDeviceInfrastructure(
  id: string,
  patch: DeviceInfrastructurePatch,
): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/devices/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

export function setDeviceFavorite(id: string, favorite: boolean): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/devices/${encodeURIComponent(id)}/favorite`, {
    method: "PUT",
    body: JSON.stringify({ favorite }),
  });
}

export function replaceDeviceTags(id: string, tagIds: string[]): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/devices/${encodeURIComponent(id)}/tags`, {
    method: "PUT",
    body: JSON.stringify({ tag_ids: tagIds }),
  });
}

export function bulkUpdateDevices(
  deviceIds: string[],
  action: "favorite" | "device_type" | "zone" | "group" | "move" | "tags",
  value: boolean | string | string[],
): Promise<{ ok: boolean; succeeded: string[]; failures: Record<string, string> }> {
  return cloudUserFetch("/v1/devices/bulk", {
    method: "POST",
    body: JSON.stringify({ device_ids: deviceIds, action, value }),
  });
}

export interface PairResult {
  ok: boolean;
  device_id: string;
  workspace_id: string;
  device_type: DeviceType;
  zone_id?: string;
  user_jwt: string;
  expires_at: string;
  tier: string;
}

/**
 * No-Telegram pairing: the desktop shows an 8-char code; the phone enters it
 * here. The relay creates (or reuses) an account and returns a user JWT.
 * If we already have a JWT, it's sent so the new PC joins the same account.
 */
export async function pairNative(
  relayBase: string,
  code: string,
  options: { workspaceId?: string; deviceType?: DeviceType; zoneId?: string } = {},
): Promise<PairResult> {
  const base = relayBase.replace(/\/+$/, "");
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const jwt = getCloudJWT();
  if (jwt) headers["Authorization"] = `Bearer ${jwt}`;
  headers["X-Remotai-Client"] = clientKindHeader();
  const res = await cloudFetch(base + "/v1/pair/confirm-native", {
    method: "POST",
    headers,
    body: JSON.stringify({
      code: code.trim().toUpperCase(),
      workspace_id: options.workspaceId || undefined,
      device_type: options.deviceType || undefined,
      zone_id: options.zoneId || undefined,
    }),
  });
  tlog("http:confirm-native", { status: res.status });
  const text = await res.text();
  let body: unknown = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = { raw: text }; }
  if (!res.ok) {
    const errorBody = body as { error?: string; code?: string } | null;
    const msg = errorBody?.error ?? res.statusText;
    throw new CloudError(res.status, msg, errorBody?.code, (body && typeof body === "object") ? body as Record<string, unknown> : undefined);
  }
  return body as PairResult;
}

/** Способ входа аккаунта (telegram/email/vk/...). */
export interface CloudIdentity {
  provider: string;
  uid: string;
  display: string;
}

/** Список способов входа текущего аккаунта. */
export function getIdentities(): Promise<{ identities: CloudIdentity[] }> {
  return cloudUserFetch<{ identities: CloudIdentity[] }>("/v1/me/identities");
}

/** Привязать email к текущему аккаунту (код уже запрошен через email/start). */
export function linkEmailIdentity(loginToken: string, code: string): Promise<{ ok: true }> {
  return cloudUserFetch<{ ok: true }>("/v1/me/identities/link", {
    method: "POST",
    body: JSON.stringify({ provider: "email", login_token: loginToken, code }),
  });
}

export function listLoginSessions(): Promise<{ sessions: CloudLoginSession[]; current_session_id: string }> {
  return cloudUserFetch("/v1/me/sessions");
}

export function revokeLoginSession(id: string): Promise<{ ok: true }> {
  return cloudUserFetch(`/v1/me/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export function revokeOtherLoginSessions(): Promise<{ ok: true; revoked: number }> {
  return cloudUserFetch("/v1/me/sessions/revoke-others", {
    method: "POST",
    body: JSON.stringify({}),
  });
}

// ── Подписка и карта (ЮKassa) ─────────────────────────────────────────────
//
// Экран отвязки — не украшение: ЮKassa включает автоплатежи только после того,
// как увидит скриншоты, на которых человек САМ отвязывает карту (требование
// менеджера, 01.09.2026). Плюс наше обязательство: при отвязке удалить токен
// повторов у себя.

export interface CloudCard {
  last4: string;
  type: string;
}

export interface CloudSubscription {
  tier: string;
  founder: boolean;
  billing_enabled: boolean;
  self_hosted?: boolean;
  /** Карта для автосписаний; null — не привязана или отвязана человеком. */
  card: CloudCard | null;
  /** До какого числа оплачено (ISO). */
  paid_until: string | null;
  auto_renew: boolean;
  unbound_at?: string;
  /**
   * Почта, на которую придёт чек. Сервер запоминает её с первой оплаты —
   * показываем, чтобы человек видел: второй раз вводить не нужно.
   */
  billing_email?: string;
  /**
   * Показывать ли проверочный платёж на 10 ₽. Решает СЕРВЕР по списку
   * администраторов: полка скрыта с витрины, но «нет на витрине» — не защита,
   * поэтому и кнопку рисуем только по флагу, и сам платёж сервер пускает
   * только администратору.
   */
  can_test_pay?: boolean;
}

/** Состояние подписки: что показывать на экране тарифа. */
/** Сменить почту для чека, не начиная оплату. */
export function setBillingEmail(email: string): Promise<{ ok: boolean; billing_email: string }> {
  return cloudUserFetch("/v1/billing/email", {
    method: "POST",
    body: JSON.stringify({ email }),
  });
}

export function getSubscription(): Promise<CloudSubscription> {
  return cloudUserFetch<CloudSubscription>("/v1/billing/subscription");
}

/**
 * Отвязать карту.
 *
 * Оплаченные дни при этом НЕ сгорают — за них уже заплачено; отвязка означает
 * «больше не списывать», а не «выключить сейчас».
 */
export function unbindCard(): Promise<{ ok: true; auto_renew: false; paid_until?: string }> {
  return cloudUserFetch<{ ok: true; auto_renew: false; paid_until?: string }>(
    "/v1/billing/unbind",
    { method: "POST" },
  );
}

/**
 * Создать платёж и получить ссылку на оплату (СБП первым способом).
 *
 * Почта — для чека (54-ФЗ). Если сервер её ещё не знает, он отвечает
 * `email_required`, и приложение спрашивает её один раз; дальше он помнит сам.
 */
export function createCheckout(tier: string, method?: "sbp" | "bank_card", email?: string): Promise<{
  payment_id: string;
  status: string;
  confirmation_url: string;
  amount_minor: number;
  tier: string;
}> {
  return cloudUserFetch("/v1/billing/checkout", {
    method: "POST",
    body: JSON.stringify({ tier, method: method ?? "", email: email ?? "" }),
  });
}
