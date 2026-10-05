/**
 * Открыть внешнюю ссылку — одинаково правильно на всех четырёх поверхностях.
 *
 * Живая жалоба владельца (2026-07-28): «нажал войти через Telegram, вошёл, но
 * окно осталось на этом переходе». Окно Remotai на ПК — это WebView2 БЕЗ
 * обработчика новых окон: `window.open(..., "_blank")` там не открывает ничего,
 * срабатывал фолбэк `location.href = url`, и окно уезжало на страницу Telegram.
 * Кнопки «назад» в WebView2 нет — приложение оставалось за чужой страницей, а
 * вход, подтверждённый в настоящем Telegram, некуда было вернуть.
 *
 * Правило: в окне на ПК ссылку открывает АГЕНТ своим системным браузером
 * (`POST /api/setup/open-external`, loopback-only, белый список хостов —
 * remotai.ru и t.me), а окно остаётся там, где было. На остальных поверхностях
 * поведение прежнее: Telegram Mini App — `openTelegramLink`, APK (Capacitor) и
 * веб — новая вкладка. Вход из APK открывает Telegram нативной ссылкой tg://.
 */
import { AppLauncher } from "@capacitor/app-launcher";
import { isOnPCPanel, isNativeApp } from "./config";
import { getTelegram } from "./telegram";
import { telegramLoginAppLink } from "./telegramLink";

/** Просим агента открыть ссылку системным браузером. Возвращает успех. */
async function openViaAgent(url: string): Promise<boolean> {
  // The browser belongs to this desktop, regardless of the selected machine.
  const base = window.location.origin;
  if (!base) return false;
  try {
    const res = await fetch(`${base}/api/setup/open-external`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url }),
    });
    if (!res.ok) return false;
    // A static SPA fallback can return HTML with 200 for an unknown endpoint.
    // Only the launcher's explicit acknowledgement proves it opened a browser.
    const result = await res.json();
    return result?.ok === true;
  } catch {
    return false;
  }
}

/**
 * Открыть ссылку так, как правильно на этой поверхности.
 *
 * Асинхронна намеренно: в окне на ПК ответ агента — единственный способ узнать,
 * что ссылка ДЕЙСТВИТЕЛЬНО открылась; если он отказал (старый агент, хост не в
 * белом списке), возвращаемся к обычному пути, а не оставляем человека без
 * реакции на нажатие.
 */
export async function openExternalLink(url: string): Promise<void> {
  const tg = getTelegram() as { openTelegramLink?: (u: string) => void } | null;
  if (tg && typeof tg.openTelegramLink === "function" && /^https:\/\/(t\.me|telegram\.me)\//i.test(url)) {
    tg.openTelegramLink(url); // Mini App → бот открывается внутри Telegram
    return;
  }
  const telegramURL = isNativeApp ? telegramLoginAppLink(url) : null;
  if (telegramURL) {
    try {
      const { completed } = await AppLauncher.openUrl({ url: telegramURL });
      if (completed) return;
    } catch { /* Telegram isn't installed or the OS declined: keep the HTTPS fallback. */ }
  }
  // Окно на ПК: новых окон WebView2 не открывает, а навигация увела бы само
  // приложение на чужую страницу без пути назад.
  if (isOnPCPanel() && await openViaAgent(url)) return;
  try {
    // ⚠ Без "noopener" НАМЕРЕННО. По спецификации HTML при `noopener`/`noreferrer`
    // window.open возвращает null ВСЕГДА — и тогда «не открылось» неотличимо от
    // «открылось», фолбэк ниже срабатывает каждый раз и уносит САМО приложение
    // на внешний адрес (аудит путей 29.08.2026, восемь мест вызова, среди них
    // вход через Telegram и обновление). Защиту даёт `opener = null` вручную:
    // обратного доступа к нашему окну у открытой страницы всё равно нет.
    const w = window.open(url, "_blank");
    if (w) {
      try { w.opener = null; } catch { /* кросс-origin — доступа уже нет */ }
      return;
    }
  } catch { /* блокировщик или платформа без window.open */ }
  window.location.href = url;
}
