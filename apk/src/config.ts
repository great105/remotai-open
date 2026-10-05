/** Server connection config — stored in localStorage. */
import { Capacitor } from "@capacitor/core";
import { RELAY_BASE } from "@tgcontrol/shared";

export type ConnectionMode = "self_hosted" | "cloud";

/**
 * true в нативном APK; false в веб-версии (remotai.ru/app). В вебе LAN-режим
 * недоступен: HTTPS-страница не может ходить на http://192.168.x.x
 * (mixed content), поэтому вход по умолчанию — облако.
 */
export const isNativeApp = Capacitor.isNativePlatform();

export interface ServerConfig {
  // self-hosted
  url: string;
  token: string;

  // cloud (P2)
  mode?: ConnectionMode;
  relayBase?: string;
  jwt?: string;
  jwtExpiresAt?: number;
  selectedDeviceId?: string;
  /** Человекочитаемая идентичность выбранной машины для рабочих экранов. */
  selectedDeviceName?: string;
  selectedDevicePlatform?: string;
  selectedDeviceType?: "computer" | "server";
  /** Версия агента выбранной машины — для плашки обновления (agentUpdateNotice). */
  selectedDeviceAgentVersion?: string;
  selectedWorkspaceId?: string;
  selectedWorkspaceName?: string;
  selectedWorkspaceRole?: "owner" | "admin" | "operator" | "viewer";
}

const STORAGE_KEY = "tgcontrol_server";

let _cache: ServerConfig | null = null;

try {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw) _cache = JSON.parse(raw);
} catch { /* ignore */ }

/** true внутри настоящего Telegram-клиента (подписанный initData). Читаем window
 *  напрямую, чтобы не зависеть от telegram.ts (избегаем циклического импорта). */
function inTelegram(): boolean {
  return typeof window !== "undefined" && !!window.Telegram?.WebApp?.initData;
}

export function getMode(): ConnectionMode {
  if (inTelegram()) return "cloud"; // Telegram-режим всегда облачный (через релей)
  return _cache?.mode ?? "self_hosted";
}

export function getServerUrl(): string {
  return _cache?.url ?? "";
}

/**
 * true только когда клиент открыт на самом ПК по loopback (окно exe или
 * браузер на 127.0.0.1) в self-hosted режиме — лишь там доступна панель
 * управления `/setup` (её эндпоинты loopback-only) и имеет смысл вкладка
 * «Панель ПК». На нативном телефоне, в облаке, Telegram или по LAN — false,
 * там обновить/спарить именно этот ПК нельзя.
 */
/**
 * Клиент открыт НА САМОМ компьютере (loopback) и у него есть локальный токен —
 * значит управлять этой машиной можно напрямую, что бы ни случилось с облаком.
 *
 * Отличается от `isOnPCPanel()` тем, что НЕ смотрит на режим: именно это и
 * нужно, когда режим уже стал облачным, а облако отказало.
 */
export function canControlLocally(): boolean {
  if (typeof window === "undefined") return false;
  if (isNativeApp || inTelegram()) return false;
  const host = window.location.hostname.toLowerCase().replace(/^\[(.*)\]$/, "$1");
  const loopback = host === "localhost" || host === "127.0.0.1" || host === "::1" || host === "0:0:0:0:0:0:0:1";
  if (!loopback) return false;
  try {
    return !!window.localStorage.getItem("remotai.local.token");
  } catch {
    return false;
  }
}

/**
 * Вернуть локальное управление, когда облачный доступ отвалился.
 *
 * Живая жалоба (2026-07-28): «открывается новое окно и я теряю ДАЖЕ локальное
 * управление… какая разница, я же локально всё равно управляю». Вход в аккаунт
 * из окна на ПК переводит клиента в облачный режим, и дальше любой отказ релея
 * (протух JWT, вошли другим аккаунтом, отвязали устройство) выкидывал на экран
 * входа — хотя человек физически сидит за этим компьютером, а локальный токен
 * лежит в localStorage и продолжает работать.
 *
 * Возвращает true, если управление восстановлено локально.
 */
export function restoreLocalControl(): boolean {
  if (!canControlLocally()) return false;
  try {
    const token = window.localStorage.getItem("remotai.local.token") || "";
    if (!token) return false;
    saveConfig({
      mode: "self_hosted",
      url: window.location.origin,
      token,
      // Облачные поля гасим: они уже недействительны, а их остатки заставили бы
      // клиент снова ходить на релей и снова получать отказ.
      jwt: "",
      jwtExpiresAt: 0,
      selectedDeviceId: "",
    });
    return true;
  } catch {
    return false;
  }
}

export function isOnPCPanel(): boolean {
  if (typeof window === "undefined") return false;
  if (isNativeApp) return false;
  if (inTelegram()) return false;
  // Режим подключения здесь НИ ПРИ ЧЁМ: панель доступна ровно потому, что
  // клиент открыт на самом компьютере (её эндпоинты loopback-only). Пока
  // условием был self_hosted, вход в аккаунт из окна на ПК ПРЯТАЛ вкладку
  // «Панель ПК» — вместе с обновлениями, версией и автозапуском. Живая жалоба:
  // «куда пропала страница, где обновления и тд». Та же ошибка, что с кнопкой
  // привязки сервера: условие обязано проверять ФАКТ (я на этом компьютере), а
  // не режим.
  const host = window.location.hostname.toLowerCase().replace(/^\[(.*)\]$/, "$1");
  return (
    host === "localhost" ||
    host === "127.0.0.1" ||
    host === "::1" ||
    host === "0:0:0:0:0:0:0:1"
  );
}

export function getServerConfig(): ServerConfig | null {
  return _cache;
}

export function getRelayBase(): string {
  return _cache?.relayBase ?? RELAY_BASE;
}

export function getCloudJWT(): string {
  return _cache?.jwt ?? "";
}

export function getSelectedDeviceId(): string {
  return _cache?.selectedDeviceId ?? "";
}

/** Stable scope for UI preferences about a terminal on a particular machine. */
export function getTerminalContextKey(): string {
  return getMode() === "cloud"
    ? `cloud:${getRelayBase()}:${getSelectedDeviceId()}`
    : `lan:${getServerUrl()}`;
}

export function getSelectedDeviceName(): string {
  if (_cache?.selectedDeviceName) return _cache.selectedDeviceName;
  if ((_cache?.mode ?? "self_hosted") === "self_hosted" && _cache?.url) {
    try { return new URL(_cache.url).hostname; } catch { /* invalid legacy URL */ }
  }
  return "";
}

export function getSelectedDevicePlatform(): string {
  return _cache?.selectedDevicePlatform ?? "";
}

/** Версия агента выбранной облачной машины; «» — не сохранена (старое
 *  переключение без объекта устройства, LAN-режим). */
export function getSelectedDeviceAgentVersion(): string {
  return _cache?.selectedDeviceAgentVersion ?? "";
}

/**
 * Освежить слепок версии агента выбранной машины.
 *
 * Слепок нужен не для показа, а для РЕШЕНИЯ: посылать ли агенту `resume`
 * (см. ptyWSUrl). До 04.08.2026 его писало ровно одно место — `selectDevice` —
 * и больше не трогал никто, поэтому у слепка было два способа врать, и оба
 * означали «resume не отправляем, история на экране стирается на каждом
 * возврате»:
 *   • пусто — так у всех, кто привязал ПК сканом QR, диплинком или кодом:
 *     эти пути объект устройства не передают;
 *   • устарело — так у всех остальных: агент обновляется САМ, а строку в
 *     localStorage переписать было нечем, и после выхода 2.49.11 клиент
 *     продолжал считать машину неумеющей.
 *
 * Поэтому версия теперь освежается из КАЖДОГО ответа со списком устройств —
 * поле там и так есть. Это же снимает вопрос «а если агент откатили назад»:
 * слепок следует за фактом в обе стороны.
 */
export function noteSelectedDeviceAgentVersion(deviceId: string, version?: string | null): void {
  if (!deviceId || !version) return;
  if (getSelectedDeviceId() !== deviceId) return;
  if (_cache?.selectedDeviceAgentVersion === version) return;
  saveConfig({ selectedDeviceAgentVersion: version });
}

export function getSelectedDeviceType(): "computer" | "server" {
  return _cache?.selectedDeviceType ?? "computer";
}

export function getSelectedWorkspaceId(): string {
  return _cache?.selectedWorkspaceId ?? "";
}

export function getSelectedWorkspaceName(): string {
  return _cache?.selectedWorkspaceName ?? "";
}

export function getSelectedWorkspaceRole(): "owner" | "admin" | "operator" | "viewer" | "" {
  return _cache?.selectedWorkspaceRole ?? "";
}

export function hasServerConfig(): boolean {
  if (inTelegram()) return true; // Telegram авторизует через подписанный initData (JWT не нужен)
  if (!_cache) return false;
  if ((_cache.mode ?? "self_hosted") === "cloud") {
    return !!_cache.jwt && !!_cache.relayBase;
  }
  return !!_cache.url && !!_cache.token;
}

export function normalizeServerUrl(input: string): string {
  let clean = input.trim().replace(/\/+$/, "");
  if (!clean) return "";
  if (!/^https?:\/\//i.test(clean)) {
    clean = `http://${clean}`;
  }
  const url = new URL(clean);
  url.hash = "";
  return url.toString().replace(/\/+$/, "");
}

function isPrivateIPv4(hostname: string): boolean {
  const parts = hostname.split(".");
  if (parts.length !== 4) return false;
  const nums = parts.map((part) => Number(part));
  if (nums.some((n) => !Number.isInteger(n) || n < 0 || n > 255)) return false;
  const [a, b] = nums;
  return (
    a === 10 ||
    a === 127 ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && b === 168) ||
    (a === 169 && b === 254)
  );
}

function isLocalHttpHost(hostname: string): boolean {
  const host = hostname.toLowerCase().replace(/^\[(.*)\]$/, "$1");
  return (
    host === "localhost" ||
    host === "::1" ||
    host === "0:0:0:0:0:0:0:1" ||
    host.endsWith(".local") ||
    host.endsWith(".lan") ||
    !host.includes(".") ||
    isPrivateIPv4(host)
  );
}

export function getServerUrlSecurityError(url: string): string | null {
  if (!url.trim()) return "Server URL is empty";
  try {
    const parsed = new URL(normalizeServerUrl(url));
    if (parsed.protocol === "https:") return null;
    if (parsed.protocol === "http:" && isLocalHttpHost(parsed.hostname)) return null;
    if (parsed.protocol === "http:") {
      return "HTTP is allowed only for localhost or private LAN addresses. Use HTTPS for public hosts.";
    }
    return "Only HTTP and HTTPS server URLs are supported.";
  } catch {
    return "Server URL is invalid";
  }
}

export function saveConfig(cfg: Partial<ServerConfig>): void {
  const next: ServerConfig = { ..._cache, ...cfg } as ServerConfig;
  if ((next.mode ?? "self_hosted") === "self_hosted") {
    const normalizedUrl = normalizeServerUrl(next.url ?? "");
    const error = getServerUrlSecurityError(normalizedUrl);
    if (error) throw new Error(error);
    next.url = normalizedUrl;
    next.token = cfg.token ?? next.token;
  }
  _cache = next;
  localStorage.setItem(STORAGE_KEY, JSON.stringify(_cache));
}

export function clearConfig(): void {
  _cache = null;
  localStorage.removeItem(STORAGE_KEY);
}
