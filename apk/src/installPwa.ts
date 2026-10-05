/**
 * Установка веб-версии на главный экран — «приложение на iPhone» без App Store.
 *
 * Почему это вообще нужно. Владельцу iPhone мы отдаём веб-версию (лендинг так и
 * решает: `#cta-phone` → /app/), и она уже собрана как PWA — manifest,
 * `display: standalone`, apple-touch-icon, apple-mobile-web-app-*. То есть
 * Safari УМЕЕТ поставить её на главный экран: своя иконка, полный экран без
 * адресной строки, отдельное окно в переключателе задач. Не хватало одного —
 * человек об этом не знает: Safari, в отличие от Chrome, кнопку «Установить»
 * не показывает НИКОГДА и подсказки не выводит. Без нашей подсказки функция
 * есть в продукте и не существует для пользователя.
 *
 * Подсказка ТОЛЬКО для iOS. На Android есть APK — он умеет больше (фон,
 * уведомления, файлы), и предлагать там «поставить сайт» значит уводить с
 * лучшего пути; на десктопе управляемая машина ставится установщиком. Поэтому
 * ветку `beforeinstallprompt` здесь не разбираем намеренно.
 *
 * ⚠ Ставить ДО входа. У приложения с главного экрана отдельное от Safari
 * хранилище: вход, сделанный в Safari, в установленное приложение не переедет —
 * человек введёт код повторно и прочтёт это как поломку. Поэтому подсказка
 * висит и на экране входа, и текст там говорит про порядок прямо.
 */
import { isNativeApp } from "./config";
import { getTelegram } from "./telegram";

/** Что предлагать этому посетителю. null — ничего (не iOS, уже установлено). */
export type IosInstallHint =
  /** Safari на iPhone/iPad: показываем три шага «Поделиться → На экран Домой». */
  | "steps"
  /** iOS, но браузер не Safari (Chrome/Firefox/встроенный в чужое приложение):
   *  надёжный путь один — открыть страницу в Safari. */
  | "open-in-safari"
  | null;

export interface InstallEnv {
  ua: string;
  /** Уже открыто как приложение (с главного экрана). */
  standalone: boolean;
  /** iPadOS 13+ в «десктопном» режиме зовётся Macintosh — отличаем по тачу. */
  maxTouchPoints: number;
  /** Нативный APK (Capacitor) — там ставить нечего. */
  native: boolean;
  /** Telegram Mini App — окно внутри Telegram, на главный экран не ставится. */
  telegram: boolean;
}

/** Чистая функция ради тестов: вся неопределённость среды — во входе. */
export function iosInstallHint(env: InstallEnv): IosInstallHint {
  if (env.native || env.telegram) return null;
  if (env.standalone) return null;
  const ua = env.ua || "";
  const handheld = /iphone|ipod|ipad/i.test(ua);
  // iPad с «Запросить версию для ПК» присылает UA Mac. Тач у настоящего Mac — 0.
  const desktopModeIpad = /macintosh/i.test(ua) && env.maxTouchPoints > 1;
  if (!handheld && !desktopModeIpad) return null;
  // Chrome (CriOS), Firefox (FxiOS), Edge (EdgiOS), Yandex (YaBrowser) и
  // встроенные webview чужих приложений: у части из них «На экран Домой» либо
  // нет, либо лежит в другом меню. Не гадаем — отправляем в Safari.
  if (/crios|fxios|edgios|opt\/|yabrowser|instagram|fbav|fban|line\/|micromessenger/i.test(ua)) {
    return "open-in-safari";
  }
  // Настоящий Safari сообщает и Version/, и Safari/. Встроенный WKWebView
  // (в том числе внутри Telegram) Version/ не присылает.
  const isSafari = /version\/[\d.]+/i.test(ua) && /safari/i.test(ua);
  return isSafari ? "steps" : "open-in-safari";
}

/** Открыто как приложение: с главного экрана (iOS) или установленное (Chrome). */
export function isStandaloneDisplay(): boolean {
  if (typeof window === "undefined") return false;
  // iOS сообщает только через navigator.standalone; display-mode там появился
  // позже и на старых версиях врёт «browser».
  const legacy = (window.navigator as Navigator & { standalone?: boolean }).standalone;
  if (legacy === true) return true;
  try {
    return ["standalone", "fullscreen", "minimal-ui"].some(
      (mode) => window.matchMedia(`(display-mode: ${mode})`).matches,
    );
  } catch {
    return false;
  }
}

const SNOOZE_KEY = "remotai_pwa_hint_snoozed_until";
/** «Позже» — на месяц: подсказка нужна один раз, но человек может передумать. */
const SNOOZE_MS = 30 * 24 * 60 * 60 * 1000;

export function snoozeInstallHint(now = Date.now()): void {
  try {
    localStorage.setItem(SNOOZE_KEY, String(now + SNOOZE_MS));
  } catch {
    /* приватный режим Safari: подсказка вернётся при следующем заходе */
  }
}

export function isInstallHintSnoozed(now = Date.now()): boolean {
  try {
    const until = Number(localStorage.getItem(SNOOZE_KEY) || 0);
    return Number.isFinite(until) && until > now;
  } catch {
    return false;
  }
}

/** Что показать на этом устройстве прямо сейчас (с учётом «Позже»). */
export function currentInstallHint(): IosInstallHint {
  if (typeof navigator === "undefined") return null;
  if (isInstallHintSnoozed()) return null;
  return iosInstallHint({
    ua: navigator.userAgent,
    standalone: isStandaloneDisplay(),
    maxTouchPoints: navigator.maxTouchPoints || 0,
    native: isNativeApp,
    telegram: !!getTelegram(),
  });
}
