import { setLanguage, useLanguage, type Language } from "../locale";
import { t } from "../i18n";

export function LanguageSelector({ className }: { className?: string }) {
  const language = useLanguage();
  return (
    <select
      className={className}
      aria-label={t("settings.language")}
      value={language}
      onChange={(event) => setLanguage(event.target.value as Language)}
      style={{ font: "inherit", color: "inherit", background: "var(--tg-bg)", border: "1px solid var(--tg-separator)", borderRadius: 8, padding: "6px 10px", minHeight: 44 }}
    >
      <option value="ru">Русский</option>
      <option value="en">English</option>
    </select>
  );
}
