/**
 * Экспорт лога терминала — один честный путь для списка (/pty) и самого
 * терминала (/pty/:id).
 *
 * Зачем отдельный модуль: оба экрана отчитывались «Экспортировано» безусловно,
 * хотя saveBlob вне нативного APK может не сохранить файл вовсе (в мобильном
 * Telegram blob-якорь молча теряет байты) — человек шёл искать лог упавшего
 * агента и не находил его. Здесь возвращается ФАКТ доставки, а вызывающий по
 * нему выбирает текст тоста.
 *
 * Второй путь — «прислать файлом в чат»: sendToTelegram принимает ПУТЬ файла на
 * ПК, а экспорт это поток из scrollback, поэтому лог сначала кладётся файлом во
 * временную папку компьютера (тот же эндпоинт, что у скрепки), и уже этот путь
 * уходит боту. Новых серверных ручек не нужно.
 */
import { ptyExportBlob, uploadPtyFile, sendToTelegram } from "./api";
import { saveBlob, type SaveOutcome } from "./saveFile";

/** Как лог реально доехал до человека ("failed" — никак, см. saveFile.ts). */
export type LogDelivery = SaveOutcome | "telegram";

/** Имя файла лога: без символов, запрещённых в путях, и без длинного хвоста. */
export function ptyLogFileName(base: string, format: "txt" | "md"): string {
  const clean = Array.from(base || "terminal")
    .map((ch) => (ch.charCodeAt(0) < 32 || '<>:"/|?*\\'.includes(ch) ? "_" : ch))
    .join("")
    .trim()
    .slice(0, 80);
  return `${clean || "terminal"}.${format}`;
}

/** Положить лог файлом на ПК и попросить бота прислать его в чат. */
async function deliverViaTelegram(blob: Blob, name: string): Promise<void> {
  await sendBlobToTelegram(blob, name);
}

/**
 * Тот же путь для любого готового файла (трасса «Зафиксировать проблему»):
 * файл кладётся на ПК, бот присылает его в чат. Вызывающий сам решает, нужно
 * ли подтверждение (запись вывода — только после отдельного согласия, I-15).
 */
export async function sendBlobToTelegram(blob: Blob, name: string): Promise<void> {
  const file = new File([blob], name, { type: blob.type || "text/plain" });
  const { path } = await uploadPtyFile(file);
  await sendToTelegram(path, name);
}

/**
 * Сохранить лог на устройство; если сохранять нечем (мобильный Telegram) —
 * прислать файлом в чат, когда бот доступен. Возвращает, ЧТО произошло.
 * Байты забираются один раз и переиспользуются фолбэком.
 */
export async function savePtyLog(
  id: string,
  format: "txt" | "md",
  name: string,
  canTelegram = false,
): Promise<LogDelivery> {
  const blob = await ptyExportBlob(id, format);
  const how = await saveBlob(blob, name);
  if (how !== "failed") return how;
  if (!canTelegram) return "failed";
  await deliverViaTelegram(blob, name);
  return "telegram";
}

/** Явное «прислать лог файлом в Telegram» — без попытки сохранения на устройство. */
export async function sendPtyLogToTelegram(
  id: string,
  format: "txt" | "md",
  name: string,
): Promise<void> {
  await deliverViaTelegram(await ptyExportBlob(id, format), name);
}
