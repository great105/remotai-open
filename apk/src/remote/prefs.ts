/**
 * Память экрана компьютера: что этот телефон помнит об этой машине и о себе.
 *
 * Раньше всё это (двенадцать функций работы с localStorage, определение сети и
 * физической клавиатуры) жило в первых 290 строках `RemoteView.tsx` — файла на
 * 3337 строк, который правится в каждом релизе. Правила хранения от разметки не
 * зависят и React не требуют, поэтому живут отдельно и проверяются тестом.
 *
 * Настройки делятся на два вида, и это различие намеренное:
 *   • ОБЩИЕ для всех машин (профиль качества, чувствительность трекпада, режим
 *     управления) — человек настраивает их под себя, а не под компьютер;
 *   • ПРО ЭТУ МАШИНУ (сколько мониторов, не даётся ли ей H.264) — они привязаны
 *     к устройству, иначе один сломанный ПК испортил бы картинку на всех.
 */
import { getMode, getSelectedDeviceId, getServerUrl } from "../config";

export type RemoteProfile = "auto" | "smooth" | "sharp" | "saver";

export const remoteProfiles: RemoteProfile[] = ["auto", "smooth", "sharp", "saver"];
export const remoteImageClipboardMax = 200 * 1024;
export const remoteSensitivityLevels = [0.8, 1.0, 1.5, 2.0, 3.0];
export const remoteSensitivityDefault = 1.5;
/**
 * Порог, после которого честно показываем расход трафика и предлагаем «Эконом»:
 * на мобильном интернете 150 МБ за сеанс — это уже заметные деньги.
 */
export const remoteTrafficHintBytes = 150 * 1024 * 1024;
/**
 * Совместимый JPEG-режим запоминаем на сутки, а не навсегда: одно неудачное
 * рукопожатие на медленном LTE не должно портить картинку на этом ПК навсегда.
 */
export const remoteJpegFallbackTtl = 24 * 60 * 60 * 1000;

/** Ключ настройки, привязанной к конкретной машине (облачной или по адресу). */
export function remoteDevicePreferenceKey(suffix: string): string {
  const identity = getMode() === "cloud"
    ? `cloud:${getSelectedDeviceId() || "unknown"}`
    : `lan:${getServerUrl() || "unknown"}`;
  return `tgcontrol.remote.${suffix}.${identity}`;
}

/** Сеть, на которой картинку надо экономить (мобильная, 2G/3G, Data Saver). */
export function constrainedNetwork(): boolean {
  const connection = (navigator as Navigator & {
    connection?: { saveData?: boolean; type?: string; effectiveType?: string };
  }).connection;
  return !!connection && (
    connection.saveData === true ||
    connection.type === "cellular" ||
    /(^|-)2g$|3g/.test(connection.effectiveType || "")
  );
}

export function hasStoredRemoteProfile(): boolean {
  try {
    return localStorage.getItem("tgcontrol.remote.profile") != null;
  } catch {
    return false;
  }
}

/**
 * Устройство с физической клавиатурой и мышью: ноутбук, десктоп, окно exe,
 * Telegram Desktop. Там нажатия обязаны уходить на ПК напрямую, без мобильной
 * строки ввода — она и место занимает, и съедает буквы (N148). Планшет с
 * подключённой мышью тоже попадает сюда, и это верно: клавиатура у него есть.
 */
export function hasPhysicalKeyboard(): boolean {
  try {
    return !!window.matchMedia?.("(hover: hover) and (pointer: fine)").matches;
  } catch {
    return false;
  }
}

/**
 * Телефон/планшет без Network Information API (весь iOS, включая Telegram-iOS):
 * признака мобильной сети там нет вовсе, поэтому «Эконом» сам не включится —
 * вместо этого предлагаем его чипом (N140).
 */
export function unknownNetworkMobile(): boolean {
  const hasApi = !!(navigator as Navigator & { connection?: unknown }).connection;
  if (hasApi) return false;
  try {
    return !!window.matchMedia?.("(pointer: coarse)").matches;
  } catch {
    return false;
  }
}

/** Отметка «этому ПК H.264 не даётся»: когда поставлена и сколько раз подряд. */
export interface JpegFallbackMark { ts: number; count: number }

export function readJpegFallbackMark(): JpegFallbackMark | null {
  try {
    const raw = localStorage.getItem(remoteDevicePreferenceKey("jpegFallback"));
    if (!raw) return null;
    // Прежний формат — просто "1", без срока: считаем, что поставлен сейчас, и
    // дальше он живёт по общему правилу (сутки + отмена вручную).
    const mark: JpegFallbackMark = raw === "1"
      ? { ts: Date.now(), count: 1 }
      : JSON.parse(raw) as JpegFallbackMark;
    if (!mark || typeof mark.ts !== "number") return null;
    if (Date.now() - mark.ts > remoteJpegFallbackTtl) {
      clearJpegFallbackMark();
      return null;
    }
    return { ts: mark.ts, count: Number(mark.count) || 1 };
  } catch {
    return null;
  }
}

export function writeJpegFallbackMark(): void {
  const prev = readJpegFallbackMark();
  try {
    localStorage.setItem(
      remoteDevicePreferenceKey("jpegFallback"),
      JSON.stringify({ ts: Date.now(), count: (prev?.count || 0) + 1 }),
    );
  } catch { /* storage is optional */ }
}

export function clearJpegFallbackMark(): void {
  try {
    localStorage.removeItem(remoteDevicePreferenceKey("jpegFallback"));
  } catch { /* storage is optional */ }
}

export function getStoredSensitivity(): number {
  try {
    const value = Number(localStorage.getItem("tgcontrol.remote.sensitivity"));
    return remoteSensitivityLevels.includes(value) ? value : remoteSensitivityDefault;
  } catch {
    return remoteSensitivityDefault;
  }
}

export function getStoredRemoteProfile(): RemoteProfile {
  try {
    const value = localStorage.getItem("tgcontrol.remote.profile") as RemoteProfile | null;
    if (value && remoteProfiles.includes(value)) return value;
    return constrainedNetwork() ? "saver" : "auto";
  } catch {
    return constrainedNetwork() ? "saver" : "auto";
  }
}

/**
 * Сколько мониторов у этой машины — по прошлому сеансу.
 *
 * Экран запуска обязан сказать это ДО подключения, а спросить не у кого:
 * список дисплеев приходит только в {t:"info"} уже открытого потока. Поэтому
 * запоминаем его на устройство и показываем как факт прошлого раза — ни одного
 * лишнего запроса ради этой строки не делаем.
 */
export function readKnownDisplays(): number {
  try {
    const value = Number(localStorage.getItem(remoteDevicePreferenceKey("displays")));
    return Number.isFinite(value) && value > 0 ? Math.round(value) : 0;
  } catch {
    return 0;
  }
}

export function writeKnownDisplays(count: number): void {
  try {
    localStorage.setItem(remoteDevicePreferenceKey("displays"), String(count));
  } catch { /* storage is optional */ }
}

export function getStoredControlMode(): "screen" | "trackpad" {
  try {
    return localStorage.getItem("tgcontrol.remote.controlMode") === "trackpad" ? "trackpad" : "screen";
  } catch {
    return "screen";
  }
}
