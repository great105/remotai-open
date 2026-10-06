import { useNavigate } from "react-router-dom";
import { LanguageSelector, useLanguage } from "@tgcontrol/shared";
import { t } from "../i18n";
import { haptic } from "../telegram";
import { SETTINGS_SECTIONS, settingsPath, type SettingsSection } from "./navigation";
import "./settings.css";

function SectionIcon({ section }: { section: SettingsSection }) {
  const paths: Record<SettingsSection, ReactNode> = {
    computer: <><rect x="3" y="4" width="18" height="13" rx="2" /><path d="M8 21h8M12 17v4" /></>,
    notifications: <><path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4" /></>,
    connection: <><path d="M8 3 4 7l4 4M4 7h11a5 5 0 0 1 5 5M16 21l4-4-4-4M20 17H9a5 5 0 0 1-5-5" /></>,
    account: <><circle cx="12" cy="8" r="4" /><path d="M4 21v-2a8 8 0 0 1 16 0v2" /></>,
    help: <><circle cx="12" cy="12" r="9" /><path d="M9.5 8.5a2.5 2.5 0 1 1 4 2c-1.5 1-1.5 1.5-1.5 3M12 17h.01" /></>,
    advanced: <><path d="M4 6h4m4 0h8M4 12h10m4 0h2M4 18h2m4 0h10" /><circle cx="10" cy="6" r="2" /><circle cx="16" cy="12" r="2" /><circle cx="8" cy="18" r="2" /></>,
  };
  return <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">{paths[section]}</svg>;
}

export function SettingsIndex({ cloud, machineName, loading, available, unread }: {
  cloud: boolean; machineName: string; loading: boolean; available: boolean; unread: number;
}) {
  useLanguage();
  const navigate = useNavigate();
  return (
    <div className="settings-index">
      <label className="settings-index-language">
        <span>{t("settings.language")}</span>
        <LanguageSelector />
      </label>
      <nav className="settings-index-list" aria-label={t("settings.nav.sections")}>
        {SETTINGS_SECTIONS.filter(section => cloud || section !== "account").map(section => (
          <button key={section} type="button" className="settings-index-row" data-settings-section={section}
            onClick={() => { haptic(); navigate(settingsPath(section)); }}>
            <SectionIcon section={section} />
            <span className="settings-index-text">
              <strong>{t(`settings.nav.${section}`)}</strong>
              <span>{t(`settings.nav.${section}Hint`)}</span>
              {section === "computer" && <small className="settings-index-machine">
                {machineName}{" · "}{t(loading ? "settings.configLoadingShort" : available ? "settings.connectionOkShort" : "settings.configOfflineShort")}
              </small>}
            </span>
            {section === "help" && unread > 0 && <span className="settings-badge">{unread}</span>}
            <span className="settings-index-chevron" aria-hidden="true">›</span>
          </button>
        ))}
      </nav>
      <button type="button" className="settings-info-row settings-index-plan" onClick={() => { haptic(); navigate("/plan"); }}>
        <span className="settings-index-text"><strong>{t("plan.title")}</strong><span>{t("settings.nav.planHint")}</span></span>
        <span aria-hidden="true">›</span>
      </button>
    </div>
  );
}

export function SettingsUnavailable({ loading, onConnection }: { loading: boolean; onConnection: () => void }) {
  return <div className="settings-unavailable" role="status">
    <h2>{t(loading ? "settings.configLoadingShort" : "settings.nav.unavailable")}</h2>
    <p>{t(loading ? "settings.configLoadingHint" : "settings.nav.unavailableHint")}</p>
    <button type="button" className="btn btn-secondary" onClick={onConnection}>{t("settings.nav.openConnection")}</button>
  </div>;
}
import type { ReactNode } from "react";
