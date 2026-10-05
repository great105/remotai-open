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
      toastSuccess(t("guide.copied"));
    } catch {
      toastError(t("ui.installtarget.m5356b97f3d"));
    }
  };
  const bridge = <>
    <p>{t("ui.installtarget.mb293f26e6c")}</p>
    <code>{RELAY_BASE}/app/#/start?task=host</code>
    <button className="btn btn-secondary" onClick={() => void copy(`${RELAY_BASE}/app/#/start?task=host`)}>{t("ui.installtarget.m4a2525fda3")}</button>
  </>;
  const help = <p className="onb-hint">{t("ui.installtarget.m573ab6d3c1")}<Link to={`/support?topic=installation&os=${server ? "linux" : os}`}>{t("guide.supportRow")}</Link>{t("ui.installtarget.mb369f46431")}</p>;
  if (handheld && !server) return <div className="start-install">{bridge}{help}</div>;
  return (
    <div className="start-install">
      {!server && <label className="login-field">
        <span>{t("ui.installtarget.m0270353439")}</span>
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
        <p>{t("ui.installtarget.m80e3a11a1e")}</p>
        <code>{REMOTAI_SERVER_INSTALL_COMMAND}</code>
        <button className="btn btn-secondary" onClick={() => void copy(REMOTAI_SERVER_INSTALL_COMMAND)}>{t("ui.installtarget.m8b6e724d3a")}</button>
        <p>{t("ui.installtarget.m411583e051")}</p>
      </> : os === "windows" ? <>
        <button className="btn btn-secondary" onClick={() => void openExternalLink(`${RELAY_BASE}/download/remotai-setup.exe`)}>
          {t("ui.installtarget.m2254aa6696")}</button>
        <p>{t("ui.installtarget.m46bf281b54")}</p>
      </> : <>
        {os === "linux" && <label className="login-field">
          <span>{t("ui.installtarget.m8c350cfbe1")}</span>
          <select value={arch} onChange={e => setArch(e.target.value as LinuxArch)}>
            <option value="amd64">{t("ui.installtarget.mf8c9e1a6d5")}</option>
            <option value="arm64">{t("ui.installtarget.mf5d5b46b84")}</option>
          </select>
        </label>}
        <button className="btn btn-secondary" onClick={() => void openExternalLink(download.url)}>{download.label}</button>
        {download.altUrl && <button className="btn btn-secondary" onClick={() => void openExternalLink(download.altUrl!)}>{download.altLabel}</button>}
        <p className="onb-hint">{t("install.previousNativePackage")}</p>
        {os === "macos" ? <>
          <p>{t("ui.installtarget.m95a55de0bc")}</p>
          <details>
            <summary>{t("ui.installtarget.m4b3ec3a2e5")}</summary>
            <p>{t("ui.installtarget.m8a32677adf")}</p>
          </details>
          <p>{t("ui.installtarget.m4e6e70b24f")}</p>
        </> : <>
          <p>{t("ui.installtarget.md622309b07")}</p>
          <p>{t("ui.installtarget.mbc45828024")}</p>
        </>}
        <p>{os === "macos" ? t("ui.installtarget.m3658be3491") : t("ui.installtarget.m32a16046bb")}{t("ui.installtarget.me4bb82f33d")}</p>
      </>}
      {!server && <details>
        <summary>{t("ui.installtarget.mebd9059704")}</summary>
        {bridge}
      </details>}
      {help}
    </div>
  );
}
