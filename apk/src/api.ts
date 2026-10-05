/** HTTP + WebSocket client for the TGControl API (standalone APK version).
 *
 * Общие endpoint-обёртки переехали в @tgcontrol/shared (api-endpoints.ts) и
 * ходят в сеть через зарегистрированный здесь ApiTransport. В этом файле
 * остаётся только app-специфика: auth (X-API-Token), cloud-роутинг через
 * релей, бинарный I/O облака, разошедшиеся функции и WS-слой.
 */

import { getInitData, getTelegram } from "./telegram";
import { agentSupportsResumeInPath, localStreamPath } from "./ptyTerm/streamPath";
import {
  getServerUrl, getServerUrlSecurityError, isNativeApp,
  getMode, getRelayBase, getCloudJWT, getSelectedDeviceId,
  getSelectedDeviceAgentVersion,
} from "./config";
import { getMe, CloudError } from "./cloud/api";
import { tlog } from "./debuglog";

/** Авторизация облачных вызовов к релею: в Telegram — подписанный `tma initData`,
 *  иначе — `Bearer` user-JWT (нативный/веб пейринг). Релей принимает оба
 *  (tgcontrol-relay requireUserAuth: middleware.go). */
function cloudAuthHeader(): string {
  const tg = getTelegram();
  // ⚠ В Telegram JWT ПРЕДПОЧТИТЕЛЬНЕЕ initData, если он уже получен
  // (cloud/miniAppAuth.ts): initData живёт 24 часа с момента запуска
  // мини-аппа, а Telegram держит его WebView сутками. Боевой лог релея
  // 01.09.2026: 216 отказов 401 на служебный сокет за сутки пачками по три с
  // паузой в четыре минуты — мини-апп владельца стучался протухшим initData.
  if (tg?.initData && !getCloudJWT()) return `tma ${tg.initData}`;
  return `Bearer ${getCloudJWT()}`;
}

/** Auth для WS-стрима (хендшейк не несёт заголовков) — query-параметр. */
function cloudStreamAuthQuery(): string {
  const tg = getTelegram();
  if (tg?.initData && !getCloudJWT()) return `initData=${encodeURIComponent(tg.initData)}`;
  return `jwt=${encodeURIComponent(getCloudJWT())}`;
}
import {
  createHttpClient, setApiTransport, downloadUrl, ptyExportUrl, ApiError,
  createWSClient, sendToTelegram as sendToTelegramLocal,
  fetchOrNetworkError, setNetworkContext, setPcLiveProbe,
} from "@tgcontrol/shared";
import type { WSParsed, UploadOptions } from "@tgcontrol/shared";
import type { WSEvent, PtySessionInfo, HealthReport } from "./types";

function getBase(): string {
  const base = getServerUrl();
  if (!base) return "";
  const error = getServerUrlSecurityError(base);
  if (error) throw new Error(error);
  return base;
}

function getToken(): string {
  const raw = getInitData(); // "token:xxx"
  return raw.startsWith("token:") ? raw.slice(6) : raw;
}

function headers(): HeadersInit {
  return {
    "Content-Type": "application/json",
    "X-API-Token": getToken(),
  };
}

/** UTF-8 safe base64 (btoa is latin1-only). */
function utf8ToB64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

/**
 * Каким путём клиент ходит за данными компьютера: LAN / окно exe / self-hosted
 * = `direct`, релей = `cloud`. shared не видит getMode(), поэтому маршрут
 * сообщаем ему сами — от него зависит и текст сетевого сбоя («Компьютер не
 * отвечает» против «Нет связи с Remotai»), и подпись баннера соединения. Режим
 * меняется при пейринге, входе в аккаунт и выборе ПК, а config.ts событий об
 * этом не рассылает, поэтому синхронизируем при загрузке модуля и перед каждым
 * обращением к сети — иначе аккаунтные экраны снова показывали бы «Нет связи.
 * Проверьте интернет и что компьютер включён» вместо честной причины (V3).
 */
function syncRoute(): "direct" | "cloud" {
  const route = getMode() === "cloud" ? "cloud" : "direct";
  setNetworkContext({ route });
  return route;
}
syncRoute();

/**
 * Текст потери доступа к ПК (совпадает с `cloud.authLost` в i18n). Ошибку с ним
 * бросаем БЕЗ status: mapApiError на 401/403 подставил бы общее «сессия истекла
 * — переподключитесь», а здесь нужно назвать и причину, и нужный аккаунт.
 */
const CLOUD_AUTH_LOST = "Доступ к компьютеру сброшен. Войдите через тот же Telegram-аккаунт, которым подключали ПК";

/**
 * Отказ бинарного запроса (скачивание файла, экспорт терминала, SFTP, превью
 * экрана) в человеческом виде. Тело у таких эндпоинтов — тот же JSON
 * `{error, code}`, что и у обычных, поэтому читаем его и отдаём ApiError со
 * status/code: раньше здесь бросали `new Error("HTTP " + status)`, mapApiError
 * не видел ни статуса, ни кода, и человек читал «HTTP 502» вместо «Компьютер не
 * в сети», а «HTTP 401» не уводил на вход (находка N111).
 *
 * `viaRelay` — ответ дал прокси релея: его СОБСТВЕННЫЙ 401/403 (без машинного
 * code) значит, что наш user-JWT больше не владеет устройством, и приложению
 * нужно сказать об этом один раз — иначе следующее скачивание снова упрётся в
 * тот же отказ. Отказы самого агента релей проксирует вербатим и они приходят с
 * кодом, поэтому «процесс защищён» из аккаунта не выкидывает (см. cloudApi).
 */
async function blobError(res: Response, viaRelay: boolean): Promise<Error> {
  const body = (await res.json().catch(() => ({}))) as {
    error?: string; code?: string; fingerprint?: string;
  };
  if (viaRelay && (res.status === 401 || res.status === 403) && !body.code) {
    notifyCloudAuthLost();
    return new Error(CLOUD_AUTH_LOST);
  }
  return new ApiError(body.error || res.statusText, res.status, body.code, body.fingerprint);
}

// Shared HTTP core for the LAN path (same client as the Mini App). Auth is the
// X-API-Token header; baseUrl is resolved per request from mutable config and
// may throw a security error. The cloud path uses a separate relay transport.
const httpClient = createHttpClient({
  baseUrl: getBase,
  getHeaders: headers,
  timeoutMessage: "Запрос не успел выполниться",
});

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  if (syncRoute() === "cloud") return cloudApi<T>(path, init);
  return httpClient.request<T>(path, init);
}

/** Адаптер multipart-загрузок для shared-эндпоинтов: cloud — через релей-прокси
 *  (cloudUpload), LAN — прямой POST с X-API-Token (auth остаётся здесь).
 *  LAN-ветка — на XMLHttpRequest ради upload.onprogress: fetch прогресс
 *  отправки не даёт. onProgress получает проценты 0–100 (в cloud-режиме
 *  промежуточных событий нет — только финальные 100). */
async function uploadForm<T>(
  path: string,
  form: FormData,
  onProgress?: (pct: number) => void,
  options?: UploadOptions,
): Promise<T> {
  const files = form.getAll("file").filter((f): f is File => f instanceof File);
  if (syncRoute() === "cloud") {
    const [rawPath, rawQuery] = path.split("?");
    const query: Record<string, string> = {};
    if (rawQuery) new URLSearchParams(rawQuery).forEach((v, k) => { query[k] = v; });
    const res = await cloudUpload(
      rawPath,
      Object.keys(query).length ? query : undefined,
      files,
      onProgress,
      options,
    );
    onProgress?.(100);
    return res as T;
  }
  const totalSize = files.reduce((sum, file) => sum + file.size, 0);
  if (options?.resumable && totalSize > CLOUD_CHUNK_SIZE) {
    return lanChunkUpload<T>(path, files, onProgress, options);
  }
  return xhrUpload<T>(path, form, onProgress, options?.signal);
}

function uploadAbortError(): ApiError {
  return new ApiError("Загрузка отменена", 0, "aborted");
}

function xhrUpload<T>(
  path: string,
  form: FormData,
  onProgress?: (pct: number) => void,
  signal?: AbortSignal,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const onAbort = () => xhr.abort();
    xhr.open("POST", `${getBase()}${path}`);
    xhr.setRequestHeader("X-API-Token", getToken());
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) onProgress(Math.round((e.loaded / e.total) * 100));
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try { resolve(xhr.responseText ? JSON.parse(xhr.responseText) : null); }
        catch { reject(new Error("Некорректный ответ сервера")); }
      } else {
        let parsed: { error?: string; code?: string } = {};
        try { parsed = JSON.parse(xhr.responseText || "{}"); } catch { /* plain body */ }
        const body = (parsed.error || xhr.responseText || "").trim().slice(0, 200);
        // Номер статуса в тексте не нужен: он есть в самой ошибке, а человеку
        // «(HTTP 413)» ничего не объясняет — по status/code фразу выберет
        // mapApiError (то же правило, что для бинарных ответов в blobError).
        reject(new ApiError(
          body ? `Не удалось загрузить файл: ${body}` : "Не удалось загрузить файл — компьютер отклонил запрос.",
          xhr.status,
          parsed.code,
        ));
      }
    };
    xhr.onerror = () => reject(new Error("Не удалось загрузить файл: нет соединения"));
    xhr.onabort = () => reject(uploadAbortError());
    if (signal?.aborted) {
      reject(uploadAbortError());
      return;
    }
    signal?.addEventListener("abort", onAbort, { once: true });
    xhr.onloadend = () => signal?.removeEventListener("abort", onAbort);
    xhr.send(form);
  });
}

// Регистрация транспорта для общих endpoint-обёрток из @tgcontrol/shared.
// Function declarations хоистятся, так что вызов в начале модуля валиден.
setApiTransport({
  request: api,
  uploadForm,
  base: getBase,
  authQuery: getInitData,
});

// Реэкспорт перенесённых endpoint-обёрток — все существующие импорты
// `from "../api"` продолжают работать без правок.
export {
  // Sessions
  getSessions, createSession, getSession, deleteSession, switchSession,
  sendPrompt, stopSession, updateSessionConfig,
  // Files
  listFiles, downloadUrl, deleteFile, deleteDir, mkDir, renameFile,
  getQuickPaths, uploadFiles, previewFile, searchFiles, getDiskInfo,
  getDirStat, copyFile,
  // PTY
  listPtySessions, createPtySession, closePtySession, uploadPtyFile, restorePtySession, reattachPtySession,
  closeDeadPtySessions, getPtyState, renamePty, setPtyPlacements, setPtyFolders,
  handoffPty, ptyInput, ptySleep, ptyWake, ptyExportUrl,
  connectSSH,
  // SSH: хосты / история / форвардинг / SFTP
  getSshHosts, createSshHost, updateSshHost, deleteSshHost, getSshHistory,
  unlockSshHost, forgetSshHostSecret, forgetSshKnownHost,
  // SSH-ключи внутри приложения
  getSshKeys, importSshKey, generateSshKey, renameSshKey, deleteSshKey, installSshKey,
  getSshForwards, createSshForward, deleteSshForward, deleteSshForwardSpec,
  sftpList, sftpPreview, sftpUpload, sftpMkdir, sftpDelete, sftpRename,
  sftpPush, sftpPull, sftpTransfers, sftpCancelTransfer,
  // Projects / System
  getProjects, getSystemStats, getProcesses, killProcess, takeScreenshot,
  saveScreenshotToPC,
  powerAction, getAutostartStatus, setAutostart, getServiceStatus,
  // Здоровье компьютера: перерыв, связь, VPN
  getBootReport, getNetworkStatus, controlVPN,
  getVBrowserStatus, startVBrowser, stopVBrowser, installVBrowserInput, navigateVBrowser,
  keepaliveVBrowser, getVBrowserDownloads,
  getBrowserPage, getBrowserTabs, newBrowserTab, activateBrowserTab, closeBrowserTab,
  emulateBrowserDevice, setBrowserScale, chooseVBrowserFiles,
  cancelVBrowserFileChooser, answerVBrowserDialog,
  // Виртуальный браузер как обычный: вид сайта, жесты, стартовая, пароли
  getBrowserDevices, hitBrowserPoint, getBrowserSelection, findOnBrowserPage,
  setBrowserViewport, getBrowserPlaces, addBrowserBookmark, removeBrowserBookmark,
  forgetBrowserSite, getBrowserLogins, saveBrowserLogin, deleteBrowserLogin,
  saveBrowserProfile, getBrowserForm, fillBrowserForm,
  getBrowserHistory, forgetBrowserHistory,
  // Bookmarks / Recent folders
  getBookmarks, addBookmark, removeBookmark, getRecentFolders,
  // Quick actions / pinned prompts
  cloneSession, clearSession, getPinnedPrompts, addPinnedPrompt, removePinnedPrompt,
  // Templates / multi-send / history / discover
  getTemplates, createTemplate, deleteTemplate, applyTemplate, multiSend,
  getClaudeHistory, discoverSessions, importDiscoveredSession,
  // Research / stats / agents / config / presets
  getResearch, startResearch, stopResearch, saveResearchConfig,
  getCostStats, getAgents, rescanAgents, getConfig, updateConfig,
  getPresets, launchPreset,
  getUserCommands, saveUserCommand, deleteUserCommand, importUserCommands,
  getSettingsCatalog, applySetting,
  getAgentAccounts, saveAgentAccount, activateAgentAccount, deleteAgentAccount,
  pinAgentAccount, setPtyAccount,
  getOpenRouter, setOpenRouterKey, forgetOpenRouterKey, getOpenRouterModels, setOpenRouterModel,
  getAgentConnection, runAgentCheck,
  getAgentRequests, answerAgentRequest,
} from "@tgcontrol/shared";

/**
 * В LAN/own_bot оставляем локальный эндпоинт агента. В cloud файл отправляет
 * релейный бот: на типичной облачной установке bot token на ПК отсутствует.
 */
export async function sendToTelegram(path: string, name?: string): Promise<any> {
  if (syncRoute() !== "cloud") return sendToTelegramLocal(path);
  const relayBase = getRelayBase().replace(/\/+$/, "");
  const deviceId = getSelectedDeviceId();
  if (!deviceId) throw new ApiError("Компьютер не выбран", 400, "device_not_selected");
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 120_000);
  try {
    const res = await fetchOrNetworkError(`${relayBase}/v1/files/send`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: cloudAuthHeader() },
      body: JSON.stringify({ device_id: deviceId, path, name }),
      signal: controller.signal,
    }, "cloud");
    const body = await res.json().catch(() => ({ error: res.statusText }));
    if (!res.ok) throw new ApiError(body.error || res.statusText, res.status, body.code);
    return body;
  } catch (e: any) {
    if (e?.name === "AbortError") {
      throw new ApiError("Telegram не успел принять файл. Попробуйте ещё раз.", 0, "timeout");
    }
    throw e;
  } finally {
    clearTimeout(timer);
  }
}

export interface AgentUpdateInfo {
  version: string;
  current: string;
  latest?: string;
  available?: boolean;
  auto_update?: boolean;
  pending_restart?: boolean;
  deferred_since?: number;
  update?: { available: boolean; version: string; changelog?: string };
}

/** Update state of the selected desktop agent (LAN or relay-proxied). */
export function getAgentUpdateInfo(): Promise<AgentUpdateInfo> {
  return api<AgentUpdateInfo>("/api/system/version");
}

/**
 * Применить обновление агента прямо сейчас (кнопка «Обновить сейчас»).
 *
 * Скачанное обновление агент применяет без сети — просто перезапускается; если
 * не скачано, скачает сам. Ответ уходит ДО рестарта, поэтому обрыв связи после
 * успешного ответа — норма, а не ошибка (клиент ждёт возврата по версии).
 */
export function applyAgentUpdate(): Promise<{ ok?: boolean; version?: string }> {
  return api<{ ok?: boolean; version?: string }>("/api/system/update", {
    method: "POST",
    body: JSON.stringify({}),
  });
}

// Чисто-апишные типы тоже переехали в @tgcontrol/shared (см. ./types);
// реэкспорт сохраняет существующие импорты `from "../api"`.
export type {
  PtyState, Project, AutostartStatus, ServiceStatus, RecentFolder,
  SessionTemplate, Preset, HealthCheck, HealthReport,
} from "./types";
export type { PtySessionInfo };
export type { UserCommand } from "@tgcontrol/shared";
// SSH-типы живут в shared рядом с endpoint-обёртками (как SSHConnectOpts).
// Там же тело быстрого ответа агенту (ptyInput) — чтобы вызывающему не
// приходилось импортировать функцию из "../api", а её тип из shared.
export type {
  SSHConnectOpts, SshHost, SshHostInput, SshHistoryEntry,
  SshKey, SshKeyImportInput, SshKeyInstallInput,
  SshForward, SshForwardInput, SshForwardSpec, SshForwardType, SftpConn, SftpEntry, SftpPreview, SshTransfer,
  PtyInputKey, PtyInputBody,
  // Виртуальный браузер: вид сайта, что под пальцем, стартовая, пароли.
  BrowserDevice, BrowserHit, BrowserBookmark, BrowserTopSite,
  BrowserLogin, BrowserProfileData, BrowserFormField, BrowserHistoryEntry,
  // Терминал, прерванный перезагрузкой компьютера (GET /api/pty → lost).
  PtyLostSession,
} from "@tgcontrol/shared";

/**
 * Relay-level 401/403 means our user JWT no longer owns the device (expired,
 * or the device was re-paired from another client). Tell the app once so it
 * can route the user back to pairing instead of showing cryptic errors on
 * every poller. Debounced: parallel запросы не должны спамить редиректами.
 */
let _lastAuthLostAt = 0;
function notifyCloudAuthLost() {
  const now = Date.now();
  if (now - _lastAuthLostAt < 5000) return;
  _lastAuthLostAt = now;
  window.dispatchEvent(new CustomEvent("tgc:cloud-auth-lost"));
}

/**
 * Cloud transport: tunnel the same REST request through the relay's
 * POST /v1/client/{deviceID}/request proxy. The relay forwards it to the PC
 * agent, which executes it against its local web server and returns the
 * response verbatim — so the result shape matches the LAN path.
 */
async function cloudApi<T>(path: string, init?: RequestInit): Promise<T> {
  const relayBase = getRelayBase().replace(/\/+$/, "");
  const deviceId = getSelectedDeviceId();
  if (!deviceId) throw new Error("Компьютер не выбран");

  const [rawPath, rawQuery] = path.split("?");
  const query: Record<string, string> = {};
  if (rawQuery) new URLSearchParams(rawQuery).forEach((v, k) => { query[k] = v; });
  const method = (init?.method || "GET").toUpperCase();
  const bodyStr = typeof init?.body === "string" ? init.body : "";

  const proxyReq = {
    method,
    path: rawPath,
    query: Object.keys(query).length ? query : undefined,
    body: bodyStr ? utf8ToB64(bodyStr) : undefined,
    headers: { "Content-Type": "application/json" },
  };

  const controller = new AbortController();
  const externalSignal = init?.signal;
  const onExternalAbort = () => controller.abort();
  if (externalSignal?.aborted) controller.abort();
  else externalSignal?.addEventListener("abort", onExternalAbort, { once: true });
  const timeout = setTimeout(() => controller.abort(), 30000);
  try {
    const res = await fetchOrNetworkError(`${relayBase}/v1/client/${encodeURIComponent(deviceId)}/request`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: cloudAuthHeader() },
      body: JSON.stringify(proxyReq),
      signal: controller.signal,
    }, "cloud");
    // The relay relays the agent's status+body verbatim.
    if (!res.ok) {
      // Тело читаем РОВНО ОДИН раз (второй res.json() бросит «body already
      // read») — и уже по нему решаем, чей это отказ.
      const body = await res.json().catch(() => ({ error: res.statusText }));
      if ((res.status === 401 || res.status === 403) && !body?.code) {
        // Отказ САМОГО релея: наш user-JWT больше не владеет устройством
        // (протух или ПК перепривязали с другого клиента). Такие ответы релей
        // пишет через writeErr — без машинного code (см. proxy.go:
        // requireUserAuth → 401, deviceForUser → 403); собственных 401/403 с
        // кодом у релея нет, так что протухший JWT по-прежнему уводит на вход.
        //
        // А ответ АГЕНТА релей проксирует вербатим, и раньше любой его 403
        // («процесс защищён» и т.п.) читался как потеря авторизации: человек
        // вылетал из аккаунта посреди работы. Наличие code = отказ прикладной,
        // а не по авторизации. Агентские отказы без кода (jsonError 403) сюда
        // ещё попадают — их нужно переводить на jsonErrorCode.
        notifyCloudAuthLost();
        throw new Error(CLOUD_AUTH_LOST);
      }
      // Машинные поля (code/fingerprint от /api/ssh/connect и т.п.) сохраняем
      // на ошибке — вызывающий ветвится по ним, а не по тексту (как ApiError
      // на LAN-пути в api-core).
      const err = new Error(body.error || res.statusText) as Error & {
        status?: number; code?: string; fingerprint?: string;
      };
      err.status = res.status;
      err.code = body.code;
      err.fingerprint = body.fingerprint;
      throw err;
    }
    const text = await res.text();
    return (text ? JSON.parse(text) : null) as T;
  } catch (e: any) {
    if (e.name === "AbortError") {
      const err = new Error(externalSignal?.aborted ? "Запрос отменён" : "Запрос не успел выполниться") as Error & {
        status?: number; code?: string;
      };
      err.status = 0;
      err.code = externalSignal?.aborted ? "aborted" : "timeout";
      throw err;
    }
    throw e;
  } finally {
    clearTimeout(timeout);
    externalSignal?.removeEventListener("abort", onExternalAbort);
  }
}

// ── Streaming endpoints (PTY / Remote Desktop) ──────────────────
//
// In LAN mode these connect straight to the PC's local /ws endpoint. In cloud
// mode they go through the relay's reverse-tunnel (/v1/client/{id}/stream),
// which the desktop agent bridges to that same local endpoint — so the binary
// protocol (xterm I/O, JPEG frames) is identical either way.

/** Build a WebSocket URL for a streaming endpoint, routed for the active mode.
 *
 *  localPath — путь, который откроет У СЕБЯ агент, вместе со своими параметрами
 *  (см. ptyTerm/streamPath). Дописывать параметры к результату НЕЛЬЗЯ: в
 *  облачном режиме это адрес релея, и параметр достанется ему, а не агенту —
 *  ровно так и потерялся resume. */
export function streamWSUrl(localPath: string): string {
  if (syncRoute() === "cloud") {
    const relayBase = getRelayBase().replace(/\/+$/, "").replace(/^http/, "ws");
    const deviceId = getSelectedDeviceId();
    if (!deviceId) throw new Error("Компьютер не выбран");
    // localPath кодируется ЦЕЛИКОМ (вместе со своим запросом) в один параметр —
    // релей передаёт его агенту как есть.
    return `${relayBase}/v1/client/${encodeURIComponent(deviceId)}/stream` +
      `?${cloudStreamAuthQuery()}&path=${encodeURIComponent(localPath)}`;
  }
  const base = getBase(); // throws on an unsafe URL
  if (!base) throw new Error("Сервер не настроен");
  const wsBase = base.replace(/^http/, "ws");
  // appendQuery сохраняет уже имеющийся запрос (resume), а не затирает его.
  return appendQuery(`${wsBase}${localPath}`, { initData: getInitData() });
}

/** resume=<epoch>:<offset> — «поток этой эпохи у меня есть до этого байта»;
 *  агент дошлёт только хвост вместо всего буфера.
 *
 *  В облаке параметр едет ВНУТРИ пути, который агент откроет у себя, а это
 *  умеют только агенты с 2.49.11: у старых такой путь ломал авторизацию и
 *  терминал не открывался совсем. Поэтому машине со старым агентом resume не
 *  шлём — она просто продолжит присылать полный буфер, как и раньше. */
export function ptyWSUrl(id: string, resume?: string): string {
  const canSendResume = syncRoute() !== "cloud" ||
    agentSupportsResumeInPath(getSelectedDeviceAgentVersion());
  // ⚠ НИКАКИХ НОВЫХ ПАРАМЕТРОВ В ЭТОМ АДРЕСЕ, кроме разрешённых релеем
  // (STREAM_QUERY_ALLOWED в ptyTerm/streamPath.ts, зеркало белого списка
  // relay/internal/server/ws_stream.go). Живой отказ 12.08.2026: кадр экрана
  // просили флагом `?screen=1`, релей ответил 400 «invalid stream path» за
  // 2 мс, и телефон перестал открывать терминалы ВООБЩЕ — 35 отказов подряд,
  // до компьютера соединение не доходило. Проверка на localhost этого не
  // видит: там релея нет. Умение теперь объявляется ВНУТРИ соединения
  // (ctrl-сообщение `screen`, см. PtyTermView) — релею знать о нём не нужно.
  return streamWSUrl(localStreamPath(`/ws/pty/${id}`, canSendResume ? { resume } : undefined));
}

export function screenWSUrl(): string {
  return streamWSUrl(`/ws/screen`);
}

// ── WebRTC Remote Desktop signaling ─────────────────────────────
//
// Non-trickle: the client gathers ICE, POSTs a complete offer to the agent's
// /api/webrtc/offer (routed LAN-direct or via the relay request-tunnel by the
// active mode), and gets a complete answer SDP back. ICE servers (STUN + TURN)
// come from the relay in cloud mode; LAN uses host candidates + a public STUN.

/** Fetch ICE servers for a WebRTC session. Cloud: ephemeral TURN creds from the
 *  relay. LAN: a public STUN (host candidates reach the PC on the same network). */
export async function fetchIceServers(): Promise<RTCIceServer[]> {
  if (syncRoute() === "cloud") {
    try {
      const relayBase = getRelayBase().replace(/\/+$/, "");
      const deviceId = getSelectedDeviceId();
      const query = deviceId ? `?device_id=${encodeURIComponent(deviceId)}` : "";
      const res = await fetch(`${relayBase}/v1/turn/credentials${query}`, {
        headers: { Authorization: cloudAuthHeader() },
      });
      if (res.ok) {
        const data = await res.json();
        if (Array.isArray(data?.iceServers)) return data.iceServers as RTCIceServer[];
      }
    } catch { /* fall through to the public-STUN default */ }
  }
  return [{ urls: ["stun:stun.l.google.com:19302"] }];
}

/** Send a WebRTC offer to the PC agent and return its answer. Works over both
 *  LAN (direct) and cloud (relay request-tunnel) via the shared api() router. */
export async function rtcSignalOffer(
  offer: { type: string; sdp: string; iceServers?: RTCIceServer[] },
): Promise<{ type: string; sdp: string }> {
  return api<{ type: string; sdp: string }>("/api/webrtc/offer", {
    method: "POST",
    body: JSON.stringify(offer),
  });
}

/** One lightweight JPEG shown while the streaming transport negotiates. */
export async function remotePreviewBlob(width = 640, signal?: AbortSignal): Promise<Blob> {
  const safeWidth = Math.max(320, Math.min(1280, Math.round(width)));
  if (syncRoute() === "cloud") {
    return cloudBlob("/api/remote/preview", { w: String(safeWidth) }, signal);
  }
  const res = await fetchOrNetworkError(`${getBase()}/api/remote/preview?w=${safeWidth}`, {
    headers: headers(),
    signal,
    cache: "no-store",
  }, "direct");
  if (!res.ok) throw await blobError(res, false);
  return res.blob();
}

// ── Cloud binary I/O (download / upload over the relay proxy) ────

function bytesToB64(bytes: Uint8Array): string {
  let bin = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(bin);
}

/** Cloud: fetch a binary GET endpoint through the relay proxy, return a Blob. */
async function cloudBlob(path: string, query?: Record<string, string>, signal?: AbortSignal): Promise<Blob> {
  const relayBase = getRelayBase().replace(/\/+$/, "");
  const deviceId = getSelectedDeviceId();
  if (!deviceId) throw new Error("Компьютер не выбран");
  const res = await fetchOrNetworkError(`${relayBase}/v1/client/${encodeURIComponent(deviceId)}/request`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: cloudAuthHeader() },
    body: JSON.stringify({ method: "GET", path, query }),
    signal,
  }, "cloud");
  if (!res.ok) throw await blobError(res, true);
  return res.blob();
}

/** Cloud: build a multipart body and POST it through the relay proxy. */
// Chunked cloud upload: relay proxy держит в памяти весь конверт запроса,
// поэтому большие файлы режем на куски по 12 МБ — релей остаётся чистым
// передатчиком и масштабируется на много клиентов. Агент собирает куски
// обратно (см. internal/web/upload_chunks.go).
const CLOUD_CHUNK_SIZE = 12 * 1024 * 1024;

function concatChunks(chunks: Uint8Array[]): Uint8Array {
  let len = 0;
  for (const c of chunks) len += c.length;
  const body = new Uint8Array(len);
  let off = 0;
  for (const c of chunks) { body.set(c, off); off += c.length; }
  return body;
}

async function cloudPost(
  path: string,
  query: Record<string, string> | undefined,
  body: Uint8Array,
  boundary: string,
  signal?: AbortSignal,
): Promise<any> {
  const relayBase = getRelayBase().replace(/\/+$/, "");
  const deviceId = getSelectedDeviceId();
  if (!deviceId) throw new Error("Компьютер не выбран");
  const res = await fetchOrNetworkError(`${relayBase}/v1/client/${encodeURIComponent(deviceId)}/request`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: cloudAuthHeader() },
    body: JSON.stringify({
      method: "POST", path, query,
      body: bytesToB64(body),
      headers: { "Content-Type": `multipart/form-data; boundary=${boundary}` },
    }),
    signal,
  }, "cloud");
  if (!res.ok) throw await blobError(res, true);
  return res.json();
}

function newBoundary(): string {
  return "----tgc" + Math.random().toString(16).slice(2) + Date.now().toString(16);
}

function uploadHash(value: string, seed: number): string {
  let h = seed >>> 0;
  for (let i = 0; i < value.length; i++) {
    h ^= value.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(36);
}

/** Один и тот же файл, повторно выбранный после обрыва, получает тот же id. */
function stableUploadID(path: string, file: File): string {
  const key = `${path}\n${file.name}\n${file.size}\n${file.lastModified}`;
  return `upl-${uploadHash(key, 0x811c9dc5)}-${uploadHash(key, 0x9e3779b9)}-${file.size.toString(36)}`;
}

async function uploadStatus(uploadId: string, signal?: AbortSignal): Promise<{ exists: boolean; offset: number }> {
  try {
    return await api(`/api/files/upload/status?upload_id=${encodeURIComponent(uploadId)}`, { signal });
  } catch (e: any) {
    // Старый агент не знает status: начинаем с нуля, а chunk_ack ниже даст
    // понятную ошибку только если он не знает и сам чанкованный протокол.
    if (e?.status === 404) return { exists: false, offset: 0 };
    throw e;
  }
}

async function abortUpload(uploadId: string): Promise<void> {
  await api("/api/files/upload/abort", {
    method: "POST",
    body: JSON.stringify({ upload_id: uploadId }),
  });
}

function appendQuery(path: string, query: Record<string, string>): string {
  const [base, raw = ""] = path.split("?");
  const q = new URLSearchParams(raw);
  for (const [key, value] of Object.entries(query)) q.set(key, value);
  return `${base}?${q}`;
}

async function lanChunkUpload<T>(
  path: string,
  files: File[],
  onProgress?: (pct: number) => void,
  options?: UploadOptions,
): Promise<T> {
  const totalSize = files.reduce((sum, file) => sum + file.size, 0);
  const saved: string[] = [];
  const failed: unknown[] = [];
  let completed = 0;
  let lastResult: any = null;

  for (const file of files) {
    const uploadId = stableUploadID(path, file);
    let status = options?.resumable
      ? await uploadStatus(uploadId, options.signal)
      : { exists: false, offset: 0 };
    if (status.offset > file.size) {
      await abortUpload(uploadId).catch(() => {});
      status = { exists: false, offset: 0 };
    }
    let offset = status.offset;
    const chunks = Math.max(1, Math.ceil(file.size / CLOUD_CHUNK_SIZE));
    try {
      do {
        if (options?.signal?.aborted) throw uploadAbortError();
        const end = Math.min(file.size, offset + CLOUD_CHUNK_SIZE);
        const chunk = file.slice(offset, end);
        const form = new FormData();
        form.append("file", new File([chunk], file.name, {
          type: file.type,
          lastModified: file.lastModified,
        }), file.name);
        const index = Math.min(chunks - 1, Math.floor(offset / CLOUD_CHUNK_SIZE));
        const chunkPath = appendQuery(path, {
          upload_id: uploadId,
          chunk: String(index),
          chunks: String(chunks),
          offset: String(offset),
          total_size: String(file.size),
        });
        const result: any = await xhrUpload(
          chunkPath,
          form,
          (pct) => {
            const inFlight = Math.round((chunk.size * pct) / 100);
            onProgress?.(totalSize ? Math.round(((completed + offset + inFlight) / totalSize) * 100) : 100);
          },
          options?.signal,
        );
        if (!result?.chunk_ack) {
          throw new Error("Для больших файлов обновите Remotai на компьютере.");
        }
        offset = end;
        lastResult = result;
      } while (offset < file.size || (file.size === 0 && !lastResult));
      saved.push(...(Array.isArray(lastResult?.files) ? lastResult.files : [file.name]));
      if (Array.isArray(lastResult?.failed)) failed.push(...lastResult.failed);
      completed += file.size;
      onProgress?.(totalSize ? Math.round((completed / totalSize) * 100) : 100);
    } catch (e) {
      if (options?.signal?.aborted) await abortUpload(uploadId).catch(() => {});
      throw e;
    }
  }
  return { ...(lastResult || {}), ok: failed.length === 0, files: saved, failed } as T;
}

async function cloudUpload(
  path: string,
  query: Record<string, string> | undefined,
  files: File[],
  onProgress?: (pct: number) => void,
  options?: UploadOptions,
): Promise<any> {
  const totalSize = files.reduce((s, f) => s + f.size, 0);

  // Мелкое — как раньше, одним конвертом со всеми файлами.
  if (totalSize <= CLOUD_CHUNK_SIZE) {
    const boundary = newBoundary();
    const parts: Uint8Array[] = [];
    const enc = new TextEncoder();
    for (const f of files) {
      if (options?.signal?.aborted) throw uploadAbortError();
      parts.push(enc.encode(
        `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${f.name}"\r\n` +
        `Content-Type: ${f.type || "application/octet-stream"}\r\n\r\n`));
      parts.push(new Uint8Array(await f.arrayBuffer()));
      parts.push(enc.encode("\r\n"));
    }
    parts.push(enc.encode(`--${boundary}--\r\n`));
    const res = await cloudPost(path, query, concatChunks(parts), boundary, options?.signal);
    onProgress?.(100);
    return res;
  }

  // Крупное — по одному файлу и по кускам; релей буферизует только один кусок.
  let result: any = null;
  let completed = 0;
  const saved: string[] = [];
  const failed: unknown[] = [];
  for (const f of files) {
    const uploadId = stableUploadID(`${path}?${new URLSearchParams(query || {})}`, f);
    const total = Math.max(1, Math.ceil(f.size / CLOUD_CHUNK_SIZE));
    let status = options?.resumable
      ? await uploadStatus(uploadId, options.signal)
      : { exists: false, offset: 0 };
    if (status.offset > f.size) {
      await abortUpload(uploadId).catch(() => {});
      status = { exists: false, offset: 0 };
    }
    let offset = status.offset;
    let fileResult: any = null;
    try {
      do {
        if (options?.signal?.aborted) throw uploadAbortError();
        const end = Math.min(f.size, offset + CLOUD_CHUNK_SIZE);
        const blob = f.slice(offset, end);
        const boundary = newBoundary();
        const enc = new TextEncoder();
        const body = concatChunks([
          enc.encode(
            `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${f.name}"\r\n` +
            `Content-Type: ${f.type || "application/octet-stream"}\r\n\r\n`),
          new Uint8Array(await blob.arrayBuffer()),
          enc.encode(`\r\n--${boundary}--\r\n`),
        ]);
        const index = Math.min(total - 1, Math.floor(offset / CLOUD_CHUNK_SIZE));
        const res = await cloudPost(path, {
          ...query,
          upload_id: uploadId,
          chunk: String(index),
          chunks: String(total),
          offset: String(offset),
          total_size: String(f.size),
        }, body, boundary, options?.signal);
        if (!res.chunk_ack) {
          throw new Error("Для больших файлов обновите Remotai на компьютере.");
        }
        offset = end;
        onProgress?.(totalSize ? Math.round(((completed + offset) / totalSize) * 100) : 100);
        result = res;
        fileResult = res;
      } while (offset < f.size || (f.size === 0 && !fileResult));
    } catch (e) {
      if (options?.signal?.aborted) await abortUpload(uploadId).catch(() => {});
      throw e;
    }
    saved.push(...(Array.isArray(fileResult?.files) ? fileResult.files : [f.name]));
    if (Array.isArray(fileResult?.failed)) failed.push(...fileResult.failed);
    completed += f.size;
  }
  return { ...(result || {}), ok: failed.length === 0, files: saved, failed };
}

// ── Binary downloads (LAN + cloud) ──────────────────────────────

/**
 * Скачивание кусками. Раньше файл ехал одним запросом, и на 200 МБ в облаке это
 * была не «медленно», а авария: агент base64-ит файл целиком (~267 МБ) и пишет
 * ОДНИМ кадром под write-deadline 10 с → i/o timeout → рвётся управляющий
 * relay-сокет, и ПК на минуту теряет облако целиком.
 *
 * Куски по 8 МБ, размер берём из списка файлов (сервер отдаёт его и в
 * X-File-Size, но релей заголовки агента не пробрасывает). Файл могли дописать
 * или усечь между кусками — тогда сервер по expect_size/expect_mtime ответит
 * file_changed, и лучше честная ошибка, чем молча битый дамп.
 */
const DOWNLOAD_CHUNK = 8 * 1024 * 1024;

export interface DownloadOpts {
  /** Размер из списка файлов. Без него качаем одним запросом, как раньше. */
  size?: number;
  /** Unix-секунды mtime из списка — для сверки, что файл не подменили. */
  mtime?: number;
  onProgress?: (done: number, total: number) => void;
  signal?: AbortSignal;
}

async function downloadChunk(path: string, offset: number, len: number, o: DownloadOpts): Promise<Blob> {
  const q: Record<string, string> = { path, offset: String(offset), len: String(len) };
  if (o.size) q.expect_size = String(o.size);
  if (o.mtime) q.expect_mtime = String(o.mtime);
  if (syncRoute() === "cloud") return cloudBlob("/api/files/download", q, o.signal);
  const qs = new URLSearchParams({ ...q, initData: getInitData() });
  const res = await fetchOrNetworkError(`${getBase()}/api/files/download?${qs}`, { signal: o.signal }, "direct");
  if (!res.ok) throw await blobError(res, false);
  return res.blob();
}

/** Fetch a file's bytes as a Blob — works in both LAN and cloud modes. */
export async function downloadBlob(path: string, o: DownloadOpts = {}): Promise<Blob> {
  const total = o.size ?? 0;
  if (!total || total <= DOWNLOAD_CHUNK) {
    if (syncRoute() === "cloud") return cloudBlob("/api/files/download", { path }, o.signal);
    const res = await fetchOrNetworkError(downloadUrl(path), { signal: o.signal }, "direct");
    if (!res.ok) throw await blobError(res, false);
    const b = await res.blob();
    o.onProgress?.(b.size, b.size);
    return b;
  }

  const parts: Blob[] = [];
  let done = 0;
  while (done < total) {
    const len = Math.min(DOWNLOAD_CHUNK, total - done);
    let part: Blob;
    try {
      part = await downloadChunk(path, done, len, o);
    } catch (e: any) {
      if (o.signal?.aborted) throw e;
      // Один повтор: обрыв одного куска из двух десятков в метро иначе убьёт
      // всё скачивание. Для чтения повтор безопасен (в отличие от загрузки).
      part = await downloadChunk(path, done, len, o);
    }
    if (part.size > len) {
      // Агент старой версии не понял offset/len и отдал файл ЦЕЛИКОМ. Если это
      // ровно тот файл, что мы ждали, — просто берём его; иначе честно просим
      // обновиться, а не склеиваем мусор.
      if (done === 0 && part.size === total) {
        o.onProgress?.(total, total);
        return part;
      }
      throw new Error("Компьютеру нужно обновление: он не умеет отдавать файл частями");
    }
    if (part.size === 0) break; // защита от бесконечного цикла на пустом ответе
    parts.push(part);
    done += part.size;
    o.onProgress?.(done, total);
  }
  return new Blob(parts);
}

/** Fetch a PTY scrollback export as a Blob — works in LAN and cloud modes. */
export async function ptyExportBlob(id: string, format: "txt" | "md"): Promise<Blob> {
  if (syncRoute() === "cloud") return cloudBlob(`/api/pty/${encodeURIComponent(id)}/export`, { format });
  const res = await fetchOrNetworkError(ptyExportUrl(id, format), undefined, "direct");
  if (!res.ok) throw await blobError(res, false);
  return res.blob();
}

export interface SshSftpConnArg {
  host_id?: string; host: string; port?: number; user: string; password?: string;
  identity_file?: string; key_passphrase?: string; proxy_jump?: string;
  proxy_password?: string; trust_host?: boolean; path: string;
}

/** Cloud: бинарный POST через релей-прокси. Секрет уезжает телом, а не query:
 *  при 25 кусках пароль иначе 25 раз попал бы в access-логи релея. */
async function cloudBlobPost(path: string, body: unknown, signal?: AbortSignal): Promise<Blob> {
  const relayBase = getRelayBase().replace(/\/+$/, "");
  const deviceId = getSelectedDeviceId();
  if (!deviceId) throw new ApiError("Компьютер не выбран", 400, "device_not_selected");
  const res = await fetchOrNetworkError(`${relayBase}/v1/client/${encodeURIComponent(deviceId)}/request`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: cloudAuthHeader() },
    body: JSON.stringify({
      method: "POST",
      path,
      body: utf8ToB64(JSON.stringify(body)),
      headers: { "Content-Type": "application/json" },
    }),
    signal,
  }, "cloud");
  if (!res.ok) throw await blobError(res, true);
  return res.blob();
}

async function sshSftpChunk(c: SshSftpConnArg, offset: number, len: number, o: DownloadOpts): Promise<Blob> {
  const body: Record<string, unknown> = {
    host_id: c.host_id, host: c.host, port: c.port || 22, user: c.user,
    password: c.password, identity_file: c.identity_file,
    key_passphrase: c.key_passphrase, proxy_jump: c.proxy_jump,
    proxy_password: c.proxy_password, trust_host: c.trust_host,
    path: c.path,
  };
  if (len > 0) {
    body.offset = offset;
    body.len = len;
    if (o.size) body.expect_size = o.size;
    if (o.mtime) body.expect_mtime = o.mtime;
  }
  if (syncRoute() === "cloud") {
    try {
      return await cloudBlobPost("/api/ssh/sftp/download", body, o.signal);
    } catch (e: any) {
      // Роль «наблюдатель» пропускает через прокси только GET/HEAD (proxy.go,
      // viewer_read_only). Для чтения файла это ограничение транспорта, а не
      // запрет: тот же кусок берём GET-ом (пароль тогда уезжает в query —
      // ровно как было до чанкования).
      if (e?.code !== "viewer_read_only") throw e;
      const query: Record<string, string> = {
        host: c.host, port: String(c.port || 22), user: c.user, path: c.path,
      };
      for (const [k, v] of Object.entries(body)) {
        if (k === "host" || k === "port" || k === "user" || k === "path") continue;
        if (v === undefined || v === null || v === "" || v === false) continue;
        // Имена полей тела и query у эндпоинта совпадают (api_ssh_sftp.go).
        query[k] = v === true ? "1" : String(v);
      }
      return cloudBlob("/api/ssh/sftp/download", query, o.signal);
    }
  }
  const res = await fetchOrNetworkError(`${getBase()}/api/ssh/sftp/download`, {
    method: "POST",
    headers: headers(), // X-API-Token + Content-Type: application/json
    body: JSON.stringify(body),
    signal: o.signal,
  }, "direct");
  if (!res.ok) throw await blobError(res, false);
  return res.blob();
}

/**
 * Скачивание файла с SSH-сервера (SFTP через агент) — LAN и cloud, кусками.
 *
 * Раньше файл ехал одним конвертом без прогресса и отмены, а в облаке умирал
 * арифметически: релей ждёт ответ агента не дольше 60 с (proxy.go), так что
 * архив логов на 300 МБ не доезжал никогда. Теперь — та же схема, что у файлов
 * ПК (downloadBlob): куски по 8 МБ с offset/len и сверкой expect_size/
 * expect_mtime, один повтор на кусок, прогресс и AbortSignal.
 *
 * `size` берём из листинга каталога; без него (или на мелком файле) остаётся
 * один запрос, как раньше — с ним же работает и агент старой версии.
 */
export async function sshSftpDownloadBlob(c: SshSftpConnArg, o: DownloadOpts = {}): Promise<Blob> {
  const total = o.size ?? 0;
  if (!total || total <= DOWNLOAD_CHUNK) {
    const b = await sshSftpChunk(c, 0, 0, o);
    o.onProgress?.(b.size, b.size);
    return b;
  }

  const parts: Blob[] = [];
  let done = 0;
  while (done < total) {
    const len = Math.min(DOWNLOAD_CHUNK, total - done);
    let part: Blob;
    try {
      part = await sshSftpChunk(c, done, len, o);
    } catch (e: any) {
      if (o.signal?.aborted) throw e;
      // Один повтор: обрыв одного куска из двух десятков в метро иначе убьёт
      // всё скачивание. Чтение идемпотентно, повтор безопасен.
      part = await sshSftpChunk(c, done, len, o);
    }
    if (part.size > len) {
      // Агент старой версии не понял offset/len и отдал файл ЦЕЛИКОМ: если это
      // ровно тот файл, что мы ждали, — берём его, иначе честно просим обновить
      // Remotai на компьютере, а не склеиваем мусор.
      if (done === 0 && part.size === total) {
        o.onProgress?.(total, total);
        return part;
      }
      throw new Error("Компьютеру нужно обновление: он не умеет отдавать файл частями");
    }
    if (part.size === 0) break; // защита от бесконечного цикла на пустом ответе
    parts.push(part);
    done += part.size;
    o.onProgress?.(done, total);
  }
  return new Blob(parts);
}

// ── Healthcheck ─────────────────────────────────────────────────
// Разошлась с miniapp-версией (cloud-ветка) — остаётся локальной.

export async function getHealthReport(): Promise<HealthReport> {
  // In cloud mode go through the relay proxy — a direct getBase() fetch would
  // be empty-based (relative) and return the app's own index.html, breaking
  // JSON.parse ("Unexpected token '<'").
  if (syncRoute() === "cloud") return cloudApi<HealthReport>("/api/health/full");
  // Auth-less endpoint — no X-API-Token required. Обёртка нужна и здесь: у
  // выключенного ПК fetch бросает TypeError, и карточка готовности показывала
  // «Failed to fetch» вместо «Компьютер не в сети» (находка V3).
  const res = await fetchOrNetworkError(`${getBase()}/api/health/full`, undefined, "direct");
  // Со статусом и кодом: иначе «ПК не в сети» выглядело как «Не удалось
  // проверить готовность: health 502» (isPcOffline не видел ни status, ни code).
  if (!res.ok) throw await blobError(res, false);
  return res.json();
}

// ── WebSocket ───────────────────────────────────────────────────

export type WSCallback = (event: WSEvent) => void;
export interface ConnectionState {
  connected: boolean;
  reason?: "pc_offline" | "net";
}
let _connectionReason: ConnectionState["reason"];
/**
 * Что релей сказал о САМОМ компьютере кадром `agent_status`: `true` — на связи,
 * `false` — нет, `null` — ещё не говорил. Наш сокет живёт до релея, поэтому его
 * «подключено» о компьютере не говорит ничего, а этот кадр — говорит.
 */
let _agentOnline: boolean | null = null;

// Ключ позиции в потоке событий. Тот же, что и до перехода на общий клиент, —
// иначе после обновления приложения `?since=` начался бы с нуля.
const LAST_EVENT_ID_KEY = "tgcontrol_last_event_id";

/**
 * URL потока событий для активного режима. `""` = слушать нечего (нет ПК или
 * авторизации): общий клиент в этом случае не заводит ладдер реконнектов и
 * честно рисует «нет связи» — раньше без выбранного ПК экраны считали, что
 * компьютер на связи, и молча получали ошибки на каждый запрос.
 */
function eventsWSUrl(since: number): string {
  // Заодно точка синхронизации маршрута для shared: поток событий пересоздаётся
  // на каждой смене режима и выбранного ПК (reconnectWS), поэтому баннер
  // соединения всегда подписан честно (см. syncRoute).
  if (syncRoute() === "cloud") {
    const relayBase = getRelayBase().replace(/\/+$/, "");
    const deviceId = getSelectedDeviceId();
    // Telegram: авторизация tma initData; иначе — user-JWT.
    const hasAuth = getTelegram()?.initData ? true : !!getCloudJWT();
    if (!relayBase || !deviceId || !hasAuth) return "";
    const wsBase = relayBase.replace(/^http/, "ws");
    return `${wsBase}/v1/client/${encodeURIComponent(deviceId)}/ws?${cloudStreamAuthQuery()}`;
  }
  const base = getBase(); // бросает на небезопасном URL — клиент трактует как ""
  if (!base) return "";
  const wsBase = base.replace(/^http/, "ws");
  let url = `${wsBase}/ws?initData=${encodeURIComponent(getInitData())}`;
  if (since > 0) url += `&since=${since}`;
  return url;
}

/**
 * Идентичность соединения (замена прежнего `_wsDeviceId`, правка 2.28.0): после
 * смены ПК сокет продолжал слушать ПРЕЖНЮЮ машину — события с одной, REST в
 * другую. Расхождение ключа = общий клиент молча закрывает старый сокет и
 * открывает новый.
 */
function eventsWSKey(): string {
  return getMode() === "cloud" ? `cloud:${getSelectedDeviceId()}` : `lan:${getServerUrl()}`;
}

/**
 * Разбор кадра ПО ФОРМЕ, а не по getMode(): режим может смениться между
 * созданием сокета и приходом кадра.
 *   облако — конверт `{type:"event", payload}` + `{type:"agent_status"}`;
 *   LAN    — плоское событие;
 *   оба    — `{type:"pong"}` как ответ на наш ping (наружу не отдаём, им
 *            вооружается idle-вотчдог).
 */
function parseEventsMsg(raw: unknown): WSParsed<WSEvent> {
  if (!raw || typeof raw !== "object") return {};
  const msg = raw as Record<string, unknown>;
  if (msg.type === "pong") return { keepalive: true };
  if (msg.type === "event" && msg.payload && typeof msg.payload === "object") {
    const payload = msg.payload as Record<string, unknown>;
    return {
      event: payload as unknown as WSEvent,
      id: typeof payload.id === "number" ? payload.id : undefined,
    };
  }
  if (msg.type === "agent_status") {
    _connectionReason = msg.online ? undefined : "pc_offline";
    _agentOnline = !!msg.online;
    return { connected: !!msg.online };
  }
  return {
    event: msg as unknown as WSEvent,
    id: typeof msg.id === "number" ? msg.id : undefined,
  };
}

/**
 * Проба «авторизация ДЕЙСТВИТЕЛЬНО потеряна?».
 *
 * Провал WS-хендшейка по 401/403 браузер отдаёт кодом 1006 — неотличимо от
 * «сети нет» (и агент, и релей отвечают ДО upgrade). Поэтому после нескольких
 * закрытий подряд, не дошедших до onopen, спрашиваем релей по REST. Побочный
 * полезный эффект: /v1/me продлевает user-JWT (slidingUserJWT), а WS берёт его
 * из getCloudJWT() на каждом коннекте — почти протухший токен лечится сам.
 *
 * На LAN не пробуем: там 401 от обрыва не отличить, а токен не протухает.
 */
async function probeCloudAuthLost(): Promise<boolean> {
  if (getMode() !== "cloud") return false;
  try {
    await getMe();
    return false;
  } catch (e) {
    // Сеть/5xx — не приговор: приговор только явный отказ в авторизации.
    return e instanceof CloudError && (e.status === 401 || e.status === 403);
  }
}

const wsClient = createWSClient<WSEvent>({
  getUrl: eventsWSUrl,
  getKey: eventsWSKey,
  lastEventIdKey: LAST_EVENT_ID_KEY,
  pingIntervalMs: 25000,   // < 120 с read-deadline релея
  idleFactor: 2.5,         // 62,5 с молчания ПОСЛЕ доказанного pong
  parse: parseEventsMsg,
  // ГЛАВНОЕ: в нативном APK паузы в фоне НЕТ. Поток событий у свёрнутого
  // приложения — единственный работающий канал уведомлений «агент ждёт
  // ответа» (NotificationBridge в App.tsx). В вебе /app/, Telegram /tg/ и окне
  // exe такого канала нет вовсе, там пауза безопасна и экономит батарею.
  pauseWhenHidden: !isNativeApp,
  probeAuth: probeCloudAuthLost,
  authProbeAfter: 3,
  onAuthLost: notifyCloudAuthLost, // → CloudAuthGuard уводит на экран входа
  log: tlog,
});

/** Открыть поток событий. Все вызывающие — точки «доступ только что появился»
 *  (холодный старт, пейринг, вход в аккаунт), поэтому снимаем возможный стоп
 *  после потери авторизации; живой сокет при этом переиспользуется. */
export function connectWS(): void { wsClient.resume(); }

/** Ручное отключение: ладдер стоит до connectWS()/reconnectWS(). */
export function disconnectWS(): void { wsClient.disconnect(); }

/** Форс-реконнект: пересоздать сокет с текущим lastEventId (реплей `?since=`).
 *  Смена ПК, ручной ретрай баннера, возврат в приложение. */
export function reconnectWS(): void { wsClient.reconnect(); }

/** Возврат из фона / сеть вернулась: реконнект ТОЛЬКО если сокет мёртв или не
 *  доказал, что жив. Живой облачный сокет не трогаем — слепой реконнект жёг бы
 *  мост на релее (та же логика, что wakeIfDead в терминале). */
export function wakeWS(): void { wsClient.wake(); }

/**
 * Доказательство «компьютер на связи ПРЯМО СЕЙЧАС» для shared. Без него ответ
 * 502 объявляет выключенным работающий компьютер (аудит путей 29.08.2026).
 *
 * Доказательство РАЗНОЕ на двух путях, и в этом всё дело:
 *
 * - **облако**: наш сокет живёт до РЕЛЕЯ, а не до компьютера. Живой сокет о
 *   компьютере не говорит ничего — нужен кадр `agent_status online`;
 * - **прямой путь** (LAN, окно exe): сокет идёт К САМОМУ АГЕНТУ, и кадра
 *   `agent_status` там не бывает вовсе. Живой сокет здесь и ЕСТЬ доказательство:
 *   компьютер держит это соединение, значит он включён и отвечает.
 *
 * Пока условие было общим (`_agentOnline === true && connected`), на прямом
 * пути доказательства не существовало НИКОГДА, и один сорвавшийся запрос с 502
 * писал «Компьютер не в сети. Включите его» — на главной в карточке готовности
 * и всплывашкой в «Файлах», при живом компьютере под зелёной шапкой «Этот
 * компьютер ●». Человек шёл включать работающую машину.
 * Замер: `build/qa/journey/j10-pk-oflayn.mjs`, акт B, 05.09.2026.
 */
export function isPcLive(): boolean {
  if (!wsClient.isConnected()) return false;
  return getMode() === "cloud" ? _agentOnline === true : true;
}
setPcLiveProbe(isPcLive);

export function onWSEvent(cb: WSCallback): () => void { return wsClient.onEvent(cb); }

export function onConnectionChange(cb: (state: ConnectionState) => void): () => void {
  return wsClient.onConnectionChange((connected) => {
    cb({
      connected,
      reason: connected ? undefined : (_connectionReason || "net"),
    });
  });
}
