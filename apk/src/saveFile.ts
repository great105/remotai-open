/**
 * Единственный путь «сохранить скачанные байты на устройстве» — общий для APK,
 * Telegram Mini App, окна exe и веба.
 *
 * ЧЕСТНЫЙ ИСХОД (находки N51/N117). Раньше не-нативная ветка всегда делала
 * `<a download>` с blob-URL и возвращала "downloaded", а вызывающие рапортовали
 * «Скачано: файл». В мобильном WebView (а Telegram Mini App на Android/iOS —
 * это он) такой якорь молча не делает НИЧЕГО: человек качал мегабайты и получал
 * вибрацию успеха без файла. Поэтому функция теперь умеет вернуть "failed" —
 * вызывающий обязан предложить забрать файл сообщением от бота
 * (POST /v1/files/send, до 49 МБ) или сказать «Не удалось сохранить».
 *
 * Порядок попыток вне нативного APK:
 *   1) navigator.share({files}) — единственный путь, доводящий файл до
 *      системного «Сохранить в Файлы» внутри WebView;
 *   2) `<a download>` — обычный браузер, окно exe, Telegram Desktop/Web;
 *   3) "failed" — мобильный Telegram без Web Share: врать про успех нельзя.
 * WebApp.downloadFile (Bot API 8.0) в цепочку НЕ входит осознанно: он скачивает
 * ПУБЛИЧНУЮ https-ссылку, а файлы ПК ходят авторизованно (LAN — http, облако —
 * POST через релей), одноразовых ссылок релей не выдаёт. Появится такая ручка —
 * шаг встанет между 1 и 2; до тех пор честный выход — доставка ботом.
 *
 * БОЛЬШИЕ ФАЙЛЫ (находка N110). Нативная ветка писала base64 всего файла одной
 * строкой: гигабайт превращался в ~1,6 млрд символов, а строка в V8 не длиннее
 * ~5*10^8 — падение было гарантировано арифметикой. Пишем кусками по 3 МБ
 * (writeFile + appendFile), поэтому размер файла ограничен только диском.
 */
import { Capacitor } from "@capacitor/core";
import { Filesystem, Directory } from "@capacitor/filesystem";
import { Share } from "@capacitor/share";

/** Итог сохранения: расшарено / скачано браузером / сохранить не удалось. */
export type SaveOutcome = "shared" | "downloaded" | "failed";

// Кратно 3: base64 куска, кратного 3 байтам, не содержит хвостовых «=», и
// куски склеиваются на диске без выравнивания.
const NATIVE_CHUNK = 3 * 1024 * 1024;

// Символы, которые нельзя пускать в путь кэша и в атрибут download (разделители
// и запрещённые в именах Windows). Всё остальное — пробелы, кириллицу, дефисы —
// сохраняем: это имя человек ищет потом в «Файлах» телефона.
const UNSAFE_NAME_CHARS = ["\\", "/", ":", "*", "?", "\"", "<", ">", "|"];

function blobToB64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve((r.result as string).split(",", 2)[1] || "");
    r.onerror = () => reject(r.error);
    r.readAsDataURL(blob);
  });
}

function safeName(name: string): string {
  let out = name;
  for (const ch of UNSAFE_NAME_CHARS) out = out.split(ch).join("_");
  out = out.trim();
  return out || "file";
}

/** True when the user dismissed the share sheet — not an error. */
export function isShareCancel(e: unknown): boolean {
  if ((e as any)?.name === "AbortError") return true;
  return /cancel|abort|dismiss/i.test((e as any)?.message || String(e ?? ""));
}

/** Кэш-каталог приложения: разрешений не требует ни на одной версии Android. */
async function writeToCache(blob: Blob, name: string): Promise<string> {
  const path = `shared/${safeName(name)}`;
  // Первый кусок создаёт файл (и затирает остаток прошлой попытки), дальше —
  // appendFile. Пустой blob всё равно должен дать пустой файл, поэтому первый
  // проход выполняется всегда.
  const head = blob.slice(0, Math.min(NATIVE_CHUNK, blob.size));
  const written = await Filesystem.writeFile({
    path,
    data: await blobToB64(head),
    directory: Directory.Cache,
    recursive: true,
  });
  let offset = head.size;
  while (offset < blob.size) {
    const part = blob.slice(offset, Math.min(offset + NATIVE_CHUNK, blob.size));
    if (part.size === 0) break;
    await Filesystem.appendFile({
      path,
      data: await blobToB64(part),
      directory: Directory.Cache,
    });
    offset += part.size;
  }
  return written.uri;
}

/** Классический якорь: обычный браузер, окно exe, Telegram Desktop/Web. */
function saveViaAnchor(blob: Blob, name: string): "downloaded" {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 4000);
  return "downloaded";
}

/**
 * Мини-апп внутри МОБИЛЬНОГО Telegram: именно там blob-якорь молча теряет файл.
 * Telegram Desktop/Web — обычный браузер, там якорь работает, и врать не нужно.
 */
function isTelegramMobile(): boolean {
  const tg = (window as any)?.Telegram?.WebApp;
  if (!tg?.initData) return false;
  const p = String(tg.platform || "").toLowerCase();
  return p === "android" || p === "ios";
}

/**
 * Сохранить/расшарить блоб под указанным именем.
 * Возвращает "shared" (системный лист), "downloaded" (браузер скачал) или
 * "failed" (сохранить нечем — предложите доставку ботом).
 * Бросает на реальных ошибках; отмену листа пользователем тоже бросает —
 * фильтруйте isShareCancel().
 */
export async function saveBlob(blob: Blob, name: string): Promise<SaveOutcome> {
  if (Capacitor.isNativePlatform()) {
    // Share-цель копирует байты себе, поэтому исходник в кэше можно не хранить.
    const uri = await writeToCache(blob, name);
    await Share.share({ title: name, files: [uri] });
    return "shared";
  }

  const file = new File([blob], safeName(name), {
    type: blob.type || "application/octet-stream",
  });
  const nav = navigator as Navigator & { canShare?: (d: unknown) => boolean };
  if (typeof nav.share === "function" && (!nav.canShare || nav.canShare({ files: [file] }))) {
    try {
      await nav.share({ files: [file], title: file.name } as ShareData);
      return "shared";
    } catch (e) {
      // Отмену пользователя пробрасываем: это не провал сохранения.
      if (isShareCancel(e)) throw e;
      // Остальное (нет transient activation после долгой загрузки, отказ
      // клиента) — не повод врать: идём дальше по цепочке.
      console.debug("[save] web share недоступен:", (e as any)?.name || e);
    }
  }

  if (isTelegramMobile()) return "failed";
  return saveViaAnchor(blob, file.name);
}
