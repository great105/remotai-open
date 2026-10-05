import { useState } from "react";
import { Link } from "react-router-dom";
import { RELAY_BASE, REMOTAI_SERVER_INSTALL_COMMAND, useToast } from "@tgcontrol/shared";
import { openExternalLink } from "../openExternal";
import { isNativeApp } from "../config";
import type { InstallOS } from "../installFlow";
import { desktopDownloadFor, detectDesktopOS, type LinuxArch } from "../downloads";
import { t } from "../i18n";
import "../onboarding.css";

export function isInstallHandheld() {
  return isNativeApp || (typeof window !== "undefined" && window.matchMedia("(pointer: coarse)").matches && window.innerWidth < 900);
}

/** Начинаем с текущего компьютера; другую ОС можно выбрать явно. */
export function InstallTarget({ server = false, os: selectedOS, onOSChange }: {
  server?: boolean;
  os?: InstallOS;
  onOSChange?: (os: InstallOS) => void;
}) {
  const [localOS, setLocalOS] = useState<InstallOS>(() => {
    const detected = detectDesktopOS(typeof navigator === "undefined" ? "" : navigator.userAgent);
    return detected === "unknown" ? "windows" : detected;
  });
  const [arch, setArch] = useState<LinuxArch>("amd64");
  const os = selectedOS ?? localOS;
  const { toastSuccess, toastError } = useToast();
  const handheld = isInstallHandheld();
  const download = desktopDownloadFor(os, arch)!;
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      toastSuccess("Скопировано");
    } catch {
      toastError("Не удалось скопировать. Выделите текст и скопируйте вручную.");
    }
  };
  const bridge = <>
    <p>Откройте этот адрес на компьютере, которым будете управлять. Установка выполняется на нём.</p>
    <code>{RELAY_BASE}/app/#/start?task=host</code>
    <button className="btn btn-secondary" onClick={() => void copy(`${RELAY_BASE}/app/#/start?task=host`)}>Скопировать ссылку для компьютера</button>
  </>;
  const help = <p className="onb-hint">Не получается установить? <Link to={`/support?topic=installation&os=${server ? "linux" : os}`}>Написать в поддержку</Link>. Ответ будет в чате на сайте; установленное приложение не требуется.</p>;
  if (handheld && !server) return <div className="start-install">{bridge}{help}</div>;
  return (
    <div className="start-install">
      {!server && <label className="login-field">
        <span>Система компьютера, которым будете управлять</span>
        <select value={os} onChange={e => {
          const nextOS = e.target.value as InstallOS;
          setLocalOS(nextOS);
          onOSChange?.(nextOS);
        }}>
          <option value="windows">Windows 10 / 11</option>
          <option value="macos">macOS</option>
          <option value="linux">Linux</option>
        </select>
      </label>}
      {server ? <>
        <p>Выполните в SSH-терминале нужного сервера:</p>
        <code>{REMOTAI_SERVER_INSTALL_COMMAND}</code>
        <button className="btn btn-secondary" onClick={() => void copy(REMOTAI_SERVER_INSTALL_COMMAND)}>Скопировать команду установки</button>
        <p>Установщик покажет состояние фонового запуска. Если он сообщает о ручном режиме, выполните команду запуска из его вывода перед закрытием терминала.</p>
      </> : os === "windows" ? <>
        <button className="btn btn-secondary" onClick={() => void openExternalLink(`${RELAY_BASE}/download/remotai-setup.exe`)}>
          Скачать Remotai для Windows
        </button>
        <p>Установщик пока без цифровой подписи. Windows может показать «Неизвестный издатель». Проверьте, что файл скачан с remotai.ru; затем «Подробнее» → «Выполнить в любом случае». В Edge: «⋯» → «Сохранить» → «Всё равно сохранить».</p>
      </> : <>
        {os === "linux" && <label className="login-field">
          <span>Процессор компьютера</span>
          <select value={arch} onChange={e => setArch(e.target.value as LinuxArch)}>
            <option value="amd64">Intel / AMD — большинство компьютеров</option>
            <option value="arm64">ARM64 — например, Raspberry Pi 64-bit</option>
          </select>
        </label>}
        <button className="btn btn-secondary" onClick={() => void openExternalLink(download.url)}>{download.label}</button>
        {download.altUrl && <button className="btn btn-secondary" onClick={() => void openExternalLink(download.altUrl!)}>{download.altLabel}</button>}
        <p className="onb-hint">{t("install.previousNativePackage")}</p>
        {os === "macos" ? <>
          <p>Для macOS 12 и новее, Intel и Apple Silicon. Откройте скачанный DMG, перетащите Remotai в Applications («Программы»), затем откройте Remotai оттуда.</p>
          <details>
            <summary>Mac не разрешает открыть приложение?</summary>
            <p>Пакет пока без подписи Apple Developer ID. Если macOS сообщает, что разработчик не проверен, после попытки открытия зайдите в «Системные настройки» → «Конфиденциальность и безопасность» → «Всё равно открыть». Используйте только файл с remotai.ru.</p>
          </details>
          <p>Доступны терминалы, файлы, SSH, мониторинг и ИИ-агенты. Экран самого Mac пока недоступен.</p>
        </> : <>
          <p>Откройте скачанный файл в программе установки приложений, нажмите «Установить», затем найдите Remotai в меню приложений. При установке система может запросить пароль компьютера.</p>
          <p>Для экрана Linux нужна графическая сессия X11; работа с Wayland пока не проверена.</p>
        </>}
        <p>{os === "macos" ? "Remotai откроется в отдельном окне с иконкой в Dock. Закрытие окна сохраняет работу терминалов. " : "Интерфейс откроется в браузере. "}Подключите компьютер по подсказкам Remotai. Автозапуск можно включить в «Панели ПК».</p>
      </>}
      {!server && <details>
        <summary>Нужный компьютер сейчас не перед вами?</summary>
        {bridge}
      </details>}
      {help}
    </div>
  );
}
