// Самообновление нативного APK (sideload c remotai.ru, не из Play Store).
//
// Android не даёт приложению молча заменить себя — система всегда показывает
// диалог установки. Поэтому «обновить» = сравнить версии и отдать системному
// браузеру скачивание свежего remotai.apk, который юзер ставит одним
// подтверждением ОС. Для exe есть тихий апдейтер, для веба обновление не нужно —
// этот путь только для Capacitor-native (вызывать под `isNativeApp`).

import { CapacitorHttp } from "@capacitor/core";
import { App } from "@capacitor/app";
import { isNativeApp } from "./config";
import { openExternalLink } from "./openExternal";

const LATEST_URL = "https://remotai.ru/download/latest.json";
const APK_URL = "https://remotai.ru/download/remotai.apk";

export type AppUpdateInfo = {
  current: string;   // versionName текущего APK (нативный манифест)
  latest: string;    // версия из latest.json
  hasUpdate: boolean;
  changelog: string;
  apkUrl: string;
};

/**
 * Сигнал о новой версии вне экрана «Настройки» (N167).
 *
 * До этого проверка жила ровно в одном useEffect настроек: кто туда не заходит,
 * месяцами оставался на старой сборке (Play Store этот APK не обновляет).
 * Теперь результат последней проверки лежит в localStorage, главная показывает
 * по нему плашку, а сама проверка НЕ учащается: раз в сутки на старте плюс
 * явное «Проверить обновления» в настройках.
 */
const STATE_KEY = "remotai.app-update.v1";
/** Не чаще раза в сутки: latest.json статичен, чаще смысла нет. */
const CHECK_INTERVAL_MS = 24 * 60 * 60 * 1000;
/** «Позже» глушит плашку на неделю — но только для ЭТОЙ версии. */
const SNOOZE_MS = 7 * 24 * 60 * 60 * 1000;
/** Минимальный промежуток между ПОПЫТКАМИ фоновой проверки (in-memory). */
const ATTEMPT_INTERVAL_MS = 10 * 60 * 1000;
let lastAttemptAt = 0;

type AppUpdateState = {
  checkedAt: number;
  current: string;
  latest: string;
  changelog: string;
  hasUpdate: boolean;
  /** Версия, по которой нажали «Позже», и до какого времени молчим. */
  snoozedVersion?: string;
  snoozedUntil?: number;
};

const listeners = new Set<() => void>();

function readState(): AppUpdateState | null {
  try {
    const raw = localStorage.getItem(STATE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as AppUpdateState | null;
    return parsed && typeof parsed.checkedAt === "number" ? parsed : null;
  } catch {
    return null;
  }
}

function writeState(patch: Partial<AppUpdateState>): void {
  const next = { ...(readState() ?? { checkedAt: 0, current: "", latest: "", changelog: "", hasUpdate: false }), ...patch };
  try {
    localStorage.setItem(STATE_KEY, JSON.stringify(next));
  } catch { /* приватный режим — плашка проживёт до перезапуска */ }
  listeners.forEach((cb) => {
    try { cb(); } catch { /* подписчик размонтирован */ }
  });
}

/**
 * Реальная версия ЗАПУЩЕННОГО APK. Плашка рисуется по слепку прошлой проверки,
 * и после обновления приложения она висела до следующего суточного чека (или
 * визита в настройки) — живая жалоба. Зовём один раз на старте: если слепок
 * отстал от факта, гасим плашку сразу, без сети.
 */
let runningVersion: string | null = null;
export async function refreshRunningVersion(): Promise<void> {
  if (!isNativeApp) return;
  try {
    const info = await App.getInfo();
    runningVersion = info.version || null;
    const st = readState();
    if (st?.hasUpdate && st.latest && runningVersion && cmpSemver(st.latest, runningVersion) <= 0) {
      writeState({ hasUpdate: false });
    }
  } catch { /* без версии просто работает старая логика */ }
}

/** Подписка на изменение состояния обновления (плашка на главной). */
export function onAppUpdateChange(cb: () => void): () => void {
  listeners.add(cb);
  return () => { listeners.delete(cb); };
}

/**
 * Что показать вне «Настроек»: версия и changelog, если обновление найдено и
 * «Позже» по нему ещё не нажимали. null — показывать нечего.
 */
export function pendingAppUpdate(): { latest: string; changelog: string; apkUrl: string } | null {
  if (!isNativeApp) return null;
  const state = readState();
  if (!state?.hasUpdate || !state.latest) return null;
  // Слепок старше факта: приложение уже обновлено, а проверка ещё не бегала.
  if (runningVersion && cmpSemver(state.latest, runningVersion) <= 0) return null;
  const snoozed = state.snoozedVersion === state.latest
    && typeof state.snoozedUntil === "number"
    && Date.now() < state.snoozedUntil;
  if (snoozed) return null;
  return { latest: state.latest, changelog: state.changelog || "", apkUrl: APK_URL };
}

/** «Позже»: не напоминать про эту версию неделю (новая версия покажется сразу). */
export function snoozeAppUpdate(version: string): void {
  writeState({ snoozedVersion: version, snoozedUntil: Date.now() + SNOOZE_MS });
}

/**
 * Фоновая проверка при старте приложения. Ходит в сеть не чаще раза в сутки и
 * молча выходит вне нативного APK (в вебе/Telegram/exe обновляться нечему).
 */
export async function checkAppUpdateThrottled(): Promise<void> {
  if (!isNativeApp) return;
  const state = readState();
  if (state && Date.now() - state.checkedAt < CHECK_INTERVAL_MS) return;
  // Отдельный ГАРД ПОПЫТКИ: успешная проверка двигает checkedAt, а провальная —
  // нет, и без этого возврат на главную (каждый раз новый монтаж плашки) слал
  // бы запрос заново при выключенном интернете.
  if (Date.now() - lastAttemptAt < ATTEMPT_INTERVAL_MS) return;
  lastAttemptAt = Date.now();
  try {
    await checkAppUpdate();
  } catch {
    // Нет сети / статика не ответила: повторим не раньше ATTEMPT_INTERVAL_MS.
  }
}

/** Сравнивает semver "x.y.z": >0 если a новее b, <0 если старше, 0 если равны. */
function cmpSemver(a: string, b: string): number {
  const pa = a.split(".").map((n) => parseInt(n, 10) || 0);
  const pb = b.split(".").map((n) => parseInt(n, 10) || 0);
  for (let i = 0; i < 3; i++) {
    const d = (pa[i] || 0) - (pb[i] || 0);
    if (d !== 0) return d > 0 ? 1 : -1;
  }
  return 0;
}

/**
 * Проверяет наличие новой версии APK. Текущую версию берём из нативного
 * манифеста (App.getInfo → versionName), доступную — из latest.json. Запрос идёт
 * через CapacitorHttp (нативный HTTP-слой), чтобы обойти CORS статики nginx:
 * авто-патч fetch у плагина выключен (capacitor.config), но прямой вызов API
 * работает и ходит мимо браузерного fetch.
 */
export async function checkAppUpdate(): Promise<AppUpdateInfo> {
  const info = await App.getInfo();
  const current = info.version || "0.0.0";
  const res = await CapacitorHttp.get({
    url: LATEST_URL,
    headers: { "Cache-Control": "no-cache" },
  });
  const data = typeof res.data === "string" ? JSON.parse(res.data) : res.data;
  const latest = String(data?.version || "0.0.0");
  const result: AppUpdateInfo = {
    current,
    latest,
    hasUpdate: cmpSemver(latest, current) > 0,
    changelog: String(data?.changelog || ""),
    apkUrl: APK_URL,
  };
  // Результат переживает уход с экрана: по нему главная рисует плашку, и по нему
  // же считается «прошли ли сутки» до следующего запроса.
  writeState({
    checkedAt: Date.now(),
    current: result.current,
    latest: result.latest,
    changelog: result.changelog,
    hasUpdate: result.hasUpdate,
  });
  return result;
}

/**
 * Открывает скачивание APK во внешнем браузере. Капаситор отдаёт window.open
 * системному браузеру (тот же приём, что в cloud/tgLogin.ts), его менеджер
 * загрузок тянет .apk → юзер ставит обновление системным диалогом.
 */
export function openApkDownload(url: string = APK_URL): void {
  // Через общую дверь (openExternal.ts): в окне на ПК скачивание открывает
  // агент системным браузером, иначе прежний фолбэк увёл бы само приложение на
  // страницу загрузки без пути назад.
  void openExternalLink(url);
}
