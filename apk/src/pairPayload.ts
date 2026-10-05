/** Пейринг режима «По локальной сети»: разбор кода с экрана ПК и подключение
 *  по нему. Общий путь для LoginView (буфер/ручной ввод) и ScanView (QR). */
import { getServerUrlSecurityError, normalizeServerUrl, saveConfig } from "./config";
import { reconnectWS } from "./api";
import { resetCapabilities } from "./capabilities";

/**
 * Parse pairing code into {url, token}.
 * Supports three formats:
 *  1) tgcontrol://pair?url=...&token=...[&device=...]  (new deep-link, URL-encoded)
 *  2) base64("url|token")                              (compact code, post-P0)
 *  3) base64("url|token|deviceID")                     (legacy, pre-P0 setup wizard)
 */
export function parsePairingCode(code: string): { url: string; token: string } | null {
  const input = code.trim();
  // 1) Deep-link
  if (input.startsWith("tgcontrol://pair?")) {
    try {
      const q = new URLSearchParams(input.slice("tgcontrol://pair?".length));
      const url = q.get("url") || "";
      const token = q.get("token") || "";
      if (url && token) return { url, token };
    } catch {
      // fall through
    }
    return null;
  }
  // 2/3) base64 with "|" separators — take first two fields
  try {
    const decoded = atob(input);
    const parts = decoded.split("|");
    if (parts.length >= 2 && parts[0] && parts[1]) {
      // Адрес всегда содержит «://» либо точку/порт. Проверка нужна с тех пор,
      // как этот разбор зовут прямо из полей ввода: случайная строка тоже может
      // «раздекодироваться» в мусор с «|», и тогда вместо честного «не похоже на
      // код» человек получал бы «Неверный адрес сервера».
      if (!/[.:]/.test(parts[0])) return null;
      return { url: parts[0], token: parts[1] };
    }
  } catch {
    // fall through
  }
  return null;
}

/**
 * Ссылка доступа, которую окно Remotai на ПК кладёт в буфер целиком:
 * `http://192.168.1.50:8080/miniapp?token=<ключ>` («Зайти с другого
 * компьютера» → нажать на адрес). Панель сама предлагает её скопировать,
 * поэтому она приходит и в поле кода, и в поле адреса, и в буфер — разбираем
 * её везде, а не только на вкладке «Адрес и ключ».
 */
export function parseAccessLink(value: string): { url: string; token: string } | null {
  const raw = value.trim();
  if (!/[?&]token=/i.test(raw)) return null;
  try {
    const parsed = new URL(raw.includes("://") ? raw : `http://${raw}`);
    const token = parsed.searchParams.get("token") || "";
    if (!token) return null;
    return { url: `${parsed.protocol}//${parsed.host}`, token };
  } catch {
    return null;
  }
}

/**
 * Единая точка разбора всего, что человек может вставить, чтобы попасть на свой
 * ПК по локальной сети: код с экрана (base64 «адрес|токен»), payload QR-кода
 * `tgcontrol://pair?…` и скопированную панелью ссылку доступа. Раньше каждое
 * поле знало только один из трёх форматов, и правильная вставка получала
 * «Неверный код подключения».
 */
export function parseLanInput(value: string): { url: string; token: string } | null {
  return parsePairingCode(value) ?? parseAccessLink(value);
}

/**
 * Подключается к ПК по локальной сети: нормализует адрес, проверяет токен
 * через /api/system/stats, сохраняет конфиг и открывает WS. В случае ошибки
 * бросает Error с понятным русским сообщением (показывайте его как есть).
 */
/**
 * QR входа через Telegram, который показывает сам Remotai на большом экране
 * (`https://t.me/<бот>?start=login_<nonce>`).
 *
 * Разбирать его обязан и НАШ сканер, а не только системная камера. Живой
 * случай 23.08: на компьютере нажали «Показать QR», навели на него сканер
 * внутри приложения — и он ответил «Это не код Remotai». Ответ формально
 * верный (это не код пейринга) и совершенно бесполезный: код показал сам
 * Remotai двумя экранами раньше.
 *
 * Строгость намеренная: принимаем только ссылку на бота с параметром
 * `start=login_…`. Любая другая ссылка t.me — чужой QR, открывать её по
 * наведению камеры нельзя.
 */
export function parseTelegramLoginLink(value: string): { url: string; token: string } | null {
  const raw = (value || "").trim();
  if (!/^(https?:\/\/(www\.)?t\.me\/|tg:\/\/resolve\?)/i.test(raw)) return null;
  let start = "";
  try {
    const u = new URL(raw);
    start = u.searchParams.get("start") || u.searchParams.get("startapp") || "";
  } catch {
    const m = /[?&]start(?:app)?=([^&\s]+)/i.exec(raw);
    start = m ? decodeURIComponent(m[1]) : "";
  }
  if (!start.startsWith("login_")) return null;
  const token = start.slice("login_".length);
  if (!token) return null;
  return { url: raw, token };
}

export async function connectLan(serverUrl: string, serverToken: string): Promise<void> {
  let cleanUrl = "";
  try {
    cleanUrl = normalizeServerUrl(serverUrl);
  } catch {
    throw new Error("Неверный адрес сервера");
  }
  if (!cleanUrl || !serverToken) {
    throw new Error("Заполните все поля");
  }
  if (getServerUrlSecurityError(cleanUrl)) {
    throw new Error("HTTP разрешён только для локальной сети. Для публичного адреса используйте HTTPS.");
  }

  let res: Response;
  try {
    res = await fetch(`${cleanUrl}/api/system/stats`, {
      headers: { "X-API-Token": serverToken },
      signal: AbortSignal.timeout(10000),
    });
  } catch (e) {
    if ((e as Error)?.name === "TimeoutError") {
      throw new Error("Сервер не отвечает. Проверьте, что Remotai запущен и телефон в той же Wi-Fi сети.");
    }
    throw new Error("Не удалось подключиться. Проверьте адрес и что телефон в одной сети с ПК.");
  }
  if (!res.ok) {
    if (res.status === 401) throw new Error("Неверный токен");
    if (res.status === 403) throw new Error("Доступ запрещён");
    throw new Error(`Ошибка сервера: ${res.status}`);
  }
  saveConfig({
    mode: "self_hosted",
    url: cleanUrl,
    token: serverToken,
    selectedDeviceName: new URL(cleanUrl).hostname,
    selectedDevicePlatform: undefined,
  });
  resetCapabilities();
  reconnectWS();
}
