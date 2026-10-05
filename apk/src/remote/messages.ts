/**
 * Что человек читает, когда экран компьютера не открылся.
 *
 * Это правила, а не разметка: какой отказ фатален (лестница из десяти попыток
 * его не разбудит — нужно честное состояние с «Повторить»), а какой стоит
 * пережить молча; какой текст соответствует машинному коду агента. Сырой
 * `message` в интерфейс не пускаем никогда — и релей, и агент присылают его
 * по-английски («agent offline», «no displays found», «Failed to fetch»).
 *
 * Жили внутри RemoteView.tsx (3337 строк) и в отрыве от экрана не вызывались,
 * значит и не проверялись. Теперь проверяются — messages.test.ts.
 */
import { mapApiError } from "@tgcontrol/shared";
import { isRemoteOffline } from "./transport";
import { t } from "../i18n";

export function formatTraffic(bytes: number): string {
  if (bytes < 1024) return t("ui.messages.mfc7867d93f", { p0: (Math.round(bytes)) });
  if (bytes < 1024 * 1024) return t("ui.messages.mefbf1bb049", { p0: ((bytes / 1024).toFixed(1)) });
  return t("ui.messages.m3fac5a808d", { p0: ((bytes / 1024 / 1024).toFixed(1)) });
}

export function remoteErrorMessage(error: unknown): string {
  // «Компьютер не в сети» — самая частая причина отказа; проверяем ДО остальных
  // ветвей, иначе 502 pc_offline от релея превращался в общее «Ошибка
  // подключения», пока баннер сверху писал правду.
  if (isRemoteOffline(error)) return t("conn.pcOffline");
  const value = error as { code?: string; status?: number; message?: string } | null;
  switch (value?.code) {
    case "no_display": return t("remote.noDisplay");
    case "capture_failed": return t("remote.captureFailed");
    case "service_mode": return t("remote.serviceMode");
    case "remote_limit": return t("remote.sessionLimit");
    case "rate_limited": return t("remote.rateLimited");
  }
  const status = Number(value?.status || 0);
  switch (status) {
    case 401: return t("remote.authExpired");
    case 403: return t("remote.accessDenied");
    case 409: return t("remote.serviceMode");
    case 429: return t("remote.rateLimited");
    case 503: return t("remote.noDisplay");
  }
  // Остальные статусы — через общий словарь (402, 410, 5xx …).
  if (status > 0) {
    const mapped = mapApiError(error);
    if (mapped && mapped !== String(value?.message ?? "")) return mapped;
  }
  return t("remote.connectionError");
}

/** Отказ, который повторными попытками не лечится: нужен человек. */
export function fatalRemoteError(error: unknown): boolean {
  const value = error as { code?: string; status?: number } | null;
  if (["no_display", "service_mode", "remote_limit", "rate_limited"].includes(value?.code || "")) {
    return true;
  }
  // Выключенный ПК — тоже приговор: лестница из 10 попыток его не разбудит,
  // человеку нужно честное состояние с «Повторить», а не минута спиннера.
  if (isRemoteOffline(error)) return true;
  return [401, 403, 409, 429, 503].includes(Number(value?.status || 0));
}

export function clipboardErrorMessage(code: string): string {
  switch (code) {
    case "xclip_missing": return t("remote.clipXclipMissing");
    case "clipboard_empty": return t("remote.emptyClipboard");
    case "clipboard_busy": return t("remote.clipBusy");
    case "image_too_large": return t("remote.clipImageTooLarge");
    case "invalid_image": return t("remote.clipInvalidImage");
    default: return t("remote.clipFailed");
  }
}
