/**
 * Самообновление веб-клиента (Telegram Mini App и remotai.ru/app).
 *
 * ЖИВОЙ СЛУЧАЙ 04.08.2026, из-за которого модуль и появился. Владелец трижды
 * подряд прислал скриншот дефекта, который в коде был уже исправлен и выпущен:
 * его iPad держал сборку двухдневной давности. Доказательство нашлось не в
 * рассуждениях, а в логе агента — новый клиент шлёт при открытии терминала
 * строку диагностики, и её не было НИ ОДНОЙ.
 *
 * Причин у залипания было две, и обе закрыты:
 *   • у `/tg/` и `/app/` не стояли заголовки кэширования — клиент держал
 *     index.html столько, сколько сам решал (Caddyfile: точка входа
 *     no-cache, ассеты с хешем — immutable);
 *   • кнопки бота вели на неизменный адрес — теперь в нём метка версии
 *     (`withClientVersion` в релее).
 * Но остаётся третья, которую снаружи не вылечить: **мини-апп на планшете живёт
 * свёрнутым сутками**. Человек нажимает «Открыть» и получает ту же самую живую
 * сессию, а не новую загрузку — старый код продолжает работать.
 *
 * Поэтому клиент проверяет себя сам: сравнивает свою версию с выпущенной и,
 * если отстал, перезагружает страницу — но ТОЛЬКО там, где перезагрузка ничего
 * не отнимает (см. safeToReload). Посреди работы в терминале или на экране
 * компьютера мы не перезагружаем никогда.
 */
import { getRelayBase } from "./config";

/** Версия сборки. Проставляется publish-release через VITE_APP_VERSION. */
export const CLIENT_VERSION: string = import.meta.env.VITE_APP_VERSION || "dev";

/**
 * Полный commit исходника сборки (ST-00, T-40). Проставляется publish-release
 * через VITE_APP_COMMIT. Версия одна на выпуск, а сборок с ней бывает несколько
 * (стенд, повторная публикация): без commit трасса «Зафиксировать проблему» не
 * говорит, какой именно код её написал. Локальная сборка честно называет себя
 * "unknown", а не выдумывает commit.
 */
export const CLIENT_COMMIT: string = import.meta.env.VITE_APP_COMMIT || "unknown";

const CHECK_QUIET_MS = 10 * 60 * 1000; // чаще раза в 10 минут не спрашиваем
const RELOAD_MARK = "tgcontrol.clientReloadedFor";

let lastCheck = 0;

/** Числовое сравнение версий: 2.49.9 старше 2.49.22, строкой это не видно. */
function isOlder(a: string, b: string): boolean {
  const pa = a.split(".").map((n) => parseInt(n, 10));
  const pb = b.split(".").map((n) => parseInt(n, 10));
  for (let i = 0; i < 3; i++) {
    const x = Number.isFinite(pa[i]) ? pa[i] : 0;
    const y = Number.isFinite(pb[i]) ? pb[i] : 0;
    if (x !== y) return x < y;
  }
  return false;
}

/**
 * Можно ли перезагрузить страницу прямо сейчас, ничего не отняв у человека.
 *
 * Нельзя: терминал (там живой вывод агента и набранный промпт), экран
 * компьютера (стрим), любой экран с открытой формой. Маршрут в Telegram живёт в
 * памяти роутера, поэтому после перезагрузки человек окажется на главной — на
 * корневых экранах это незаметно, внутри работы это потеря места.
 */
function safeToReload(): boolean {
  if (typeof document === "undefined") return false;
  if (document.visibilityState !== "visible") return false;
  // Где человек находится, спрашиваем у ЭКРАНА, а не у адреса: в Telegram и в
  // нативном APK роутер memory, и `location.hash` там не про наш маршрут вовсе
  // — по нему мы бы решили, что человек на главной, и перезагрузили терминал с
  // живым выводом агента.
  if (document.querySelector(".pty-terminal, .remote-page, .xterm")) return false;
  // Открытая шторка/диалог — признак начатого действия.
  if (document.querySelector(".sheet-backdrop, .modal-backdrop, .folder-menu")) return false;
  // Набранный, но не отправленный текст терять нельзя.
  const typed = [...document.querySelectorAll("input, textarea")]
    .some((el) => (el as HTMLInputElement).value?.trim().length > 0);
  return !typed;
}

/**
 * Проверить, не устарел ли клиент, и обновиться, если можно.
 *
 * Молча возвращается, когда версия неизвестна (dev-сборка, локальный запуск) —
 * иначе разработка превратилась бы в бесконечную перезагрузку.
 */
export async function checkClientVersion(): Promise<void> {
  if (CLIENT_VERSION === "dev") return;
  const now = Date.now();
  if (now - lastCheck < CHECK_QUIET_MS) return;
  lastCheck = now;

  const base = getRelayBase();
  if (!base) return;
  let latest = "";
  try {
    const res = await fetch(`${base.replace(/\/$/, "")}/download/latest.json`, {
      cache: "no-store",
    });
    if (!res.ok) return;
    const data = await res.json();
    latest = String(data?.version || "");
  } catch {
    return; // нет сети — не наше дело
  }
  if (!latest || !isOlder(CLIENT_VERSION, latest)) return;

  // Один раз на версию: если после перезагрузки клиент почему-то остался
  // прежним (кэш прокси, отдающий старое), не крутим цикл.
  try {
    if (sessionStorage.getItem(RELOAD_MARK) === latest) return;
    sessionStorage.setItem(RELOAD_MARK, latest);
  } catch { /* приватный режим — переживём и без метки */ }

  if (!safeToReload()) return;
  window.location.reload();
}

/**
 * Подписки: холодный старт и каждое возвращение к приложению. Второе — главное
 * ради планшета, где мини-апп неделями не закрывают, а лишь сворачивают.
 */
export function watchClientVersion(): void {
  if (typeof document === "undefined") return;
  void checkClientVersion();
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void checkClientVersion();
  });
}
