import { t } from "@tgcontrol/shared";
/**
 * Откуда взять Remotai для КОМПЬЮТЕРА, за которым человек сидит прямо сейчас.
 *
 * Экран входа в браузере на ПК до этого советовал «откройте remotai.ru на
 * компьютере и скачайте приложение там» — человек уже стоял на remotai.ru на
 * компьютере, и совет отправлял его по кругу. Прямая ссылка на телефоне
 * действительно бесполезна (тянет .exe на телефон, находка N96), но на
 * настольном браузере она — единственное, что закрывает вопрос.
 *
 * Правило вынесено из экрана в модуль: платформа определяется по строке
 * user-agent, и это проверяется тестом в node, без React и DOM.
 */
import { RELAY_BASE } from "@tgcontrol/shared";

export type DesktopOS = "windows" | "macos" | "linux" | "unknown";
export type LinuxArch = "amd64" | "arm64";

export interface DesktopDownload {
  os: DesktopOS;
  /** Подпись кнопки: «Скачать для Windows». */
  label: string;
  /** Прямая ссылка на установщик. */
  url: string;
  /** Команда установки — там, где бинарь ставят скриптом (Linux). */
  command?: string;
  /** Пошаговая установка и получение кода на Unix. */
  guideUrl?: string;
  /** Вторая ссылка того же семейства (Intel-мак, portable-сборка). */
  altLabel?: string;
  altUrl?: string;
}

/** ПК-платформа по user-agent. Телефоны сюда не попадают — они "unknown". */
export function detectDesktopOS(ua: string): DesktopOS {
  const s = ua.toLowerCase();
  // Android содержит "linux" в UA, поэтому проверяется первым и отсекается.
  if (/android|iphone|ipad|ipod/.test(s)) return "unknown";
  if (/windows|win32|win64/.test(s)) return "windows";
  if (/mac os x|macintosh/.test(s)) return "macos";
  if (/linux|x11|cros/.test(s)) return "linux";
  return "unknown";
}

/** Что предложить скачать для этой ОС. null — платформа не настольная. */
export function desktopDownloadFor(os: DesktopOS, arch: LinuxArch = "amd64"): DesktopDownload | null {
  const base = RELAY_BASE.replace(/\/+$/, "");
  switch (os) {
    case "windows":
      return {
        os,
        label: t("ui.installtarget.m2254aa6696"),
        url: `${base}/download/remotai-setup.exe`,
        altLabel: t("ui.downloads.m5f8570b2bc"),
        altUrl: `${base}/download/remotai.exe`,
      };
    case "macos":
      // Both Apple Silicon and Intel can report Intel in browser user agents.
      // A universal app avoids asking people to identify their Mac processor.
      return {
        os,
        label: t("ui.downloads.m04c253eae7"),
        url: `${base}/download/remotai-macos.dmg`,
        guideUrl: `${base}/app/#/start?task=host&os=macos`,
      };
    case "linux":
      return {
        os,
        label: t("ui.downloads.m35583c8439"),
        url: `${base}/download/remotai-linux-${arch}.deb`,
        altLabel: t("ui.downloads.m2d3b674228"),
        altUrl: `${base}/download/remotai-linux-${arch}.rpm`,
        guideUrl: `${base}/app/#/start?task=host&os=linux`,
      };
    default:
      return null;
  }
}

/** Готовое предложение для текущего браузера (null — телефон/неизвестно). */
export function currentDesktopDownload(): DesktopDownload | null {
  if (typeof navigator === "undefined") return null;
  return desktopDownloadFor(detectDesktopOS(navigator.userAgent || ""));
}
