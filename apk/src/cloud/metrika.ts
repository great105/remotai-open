/**
 * ЯНДЕКС.МЕТРИКА В ПРИЛОЖЕНИИ — ТОЛЬКО ВЕБ НА remotai.ru.
 *
 * Зачем. Рекламный трафик (РСЯ, сети Директа) оптимизируется по целям
 * Метрики, а до 07.09.2026 счётчик стоял только на лендинге: он видел
 * «скачали», «открыли веб-версию», но НЕ видел регистрацию, привязку
 * компьютера, первый запуск агента и оплату — всё это происходит уже в
 * приложении по адресу remotai.ru/app/. Кампании нечего было оптимизировать,
 * а цену установки и цену оплаты нельзя было посчитать.
 *
 * Как. Тот же счётчик 111692890, что на лендинге (тот же домен — визит один,
 * `_ym_uid` первичной кукой продолжается). Тег грузится ЛЕНИВО и только когда:
 *   • страница открыта на remotai.ru (не APK, не мини-апп Telegram, не стенд
 *     на 127.0.0.1 — стенд однажды уже накрутил втрое визиты лендинга);
 *   • человек не выключил аналитику в настройках (та же галочка, что и у
 *     вех воронки в support.ts).
 * Имена целей совпадают с вехами воронки релея (`register`, `pair_success`,
 * `first_terminal`, `first_agent`) плюс деньги (`checkout_start`,
 * `payment_success`) — те же идентификаторы заведены в Метрике целями типа
 * «JavaScript-событие». Ни одно поле с содержимым терминалов, файлов или
 * экрана в цель не попадает — только имя вехи.
 */

export const METRIKA_COUNTER = 111692890;

/** Цели приложения, заведённые в Метрике под этими же идентификаторами. */
export type MetrikaGoal =
  | "register"
  | "pair_success"
  | "first_terminal"
  | "first_agent"
  | "checkout_start"
  | "payment_success";

export interface MetrikaEnvironment {
  hostname: string;
  nativeApp: boolean;
  telegramMiniApp: boolean;
  analyticsEnabled: boolean;
}

/** Чистое правило: можно ли грузить счётчик и слать цели в этом окружении. */
export function metrikaAllowed(env: MetrikaEnvironment): boolean {
  if (!env.analyticsEnabled) return false;
  if (env.nativeApp || env.telegramMiniApp) return false;
  return /(^|\.)remotai\.ru$/i.test(env.hostname || "");
}

/**
 * Какую цель скачивания шлёт лендинг по ссылке загрузки. Раньше всё, что не
 * APK, считалось «Скачали для Windows» — и DMG/DEB/RPM из 2.66.0 копились в
 * чужой цели; по такой цели нельзя ни оптимизировать кампанию, ни понять,
 * сколько стоит установка на каждой ОС.
 */
export function downloadGoalFor(href: string): "download_apk" | "download_mac" | "download_linux" | "download_win" {
  const h = (href || "").toLowerCase();
  if (h.endsWith(".apk")) return "download_apk";
  if (h.endsWith(".dmg")) return "download_mac";
  if (h.endsWith(".deb") || h.endsWith(".rpm")) return "download_linux";
  return "download_win";
}

type YM = (counter: number, action: string, ...rest: unknown[]) => void;

let loadStarted = false;
const pending: MetrikaGoal[] = [];

function currentEnv(deps: { nativeApp: boolean; telegramMiniApp: boolean; analyticsEnabled: boolean }): MetrikaEnvironment {
  return {
    hostname: typeof location !== "undefined" ? location.hostname : "",
    nativeApp: deps.nativeApp,
    telegramMiniApp: deps.telegramMiniApp,
    analyticsEnabled: deps.analyticsEnabled,
  };
}

function ensureTag(): void {
  if (loadStarted || typeof document === "undefined") return;
  loadStarted = true;
  const w = window as unknown as { ym?: YM & { a?: unknown[]; l?: number } };
  if (typeof w.ym !== "function") {
    // Официальная заглушка: вызовы копятся в ym.a, тег доиграет их после загрузки.
    const stub = ((...args: unknown[]) => { (stub.a = stub.a || []).push(args); }) as YM & { a?: unknown[]; l?: number };
    stub.l = Date.now();
    w.ym = stub;
    const s = document.createElement("script");
    s.async = true;
    s.src = "https://mc.yandex.ru/metrika/tag.js";
    document.head.appendChild(s);
  }
  // Без webvisor/clickmap: приложение — это терминалы и файлы человека, записи
  // сессий тут не место. Считаем только визиты и цели.
  w.ym?.(METRIKA_COUNTER, "init", { clickmap: false, trackLinks: false, accurateTrackBounce: true, webvisor: false });
  for (const g of pending.splice(0)) w.ym?.(METRIKA_COUNTER, "reachGoal", g);
}

/**
 * Начать визит в Метрике без цели — при открытии веб-приложения. Так визит с
 * лендинга (тот же домен, та же кука `_ym_uid`) продолжается в приложении и у
 * рекламы виден весь путь, а не только момент цели. Тег грузится один раз.
 */
export function initMetrikaVisit(
  deps: { nativeApp: boolean; telegramMiniApp: boolean; analyticsEnabled: boolean },
): boolean {
  try {
    if (!metrikaAllowed(currentEnv(deps))) return false;
    ensureTag();
    return true;
  } catch {
    return false;
  }
}

/**
 * Достичь цели в Метрике, если окружение это разрешает. Никогда не бросает:
 * аналитика не повод ронять экран.
 */
export function reachMetrikaGoal(
  goal: MetrikaGoal,
  deps: { nativeApp: boolean; telegramMiniApp: boolean; analyticsEnabled: boolean },
): boolean {
  try {
    if (!metrikaAllowed(currentEnv(deps))) return false;
    pending.push(goal);
    ensureTag();
    const w = window as unknown as { ym?: YM };
    if (typeof w.ym === "function" && pending.length) {
      for (const g of pending.splice(0)) w.ym(METRIKA_COUNTER, "reachGoal", g);
    }
    return true;
  } catch {
    return false;
  }
}
