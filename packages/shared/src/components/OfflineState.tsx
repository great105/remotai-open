import { t } from "../i18n";

interface Props {
  /** Имя компьютера, если известно (config.hostname). */
  hostname?: string;
  /** Повторить загрузку. */
  onRetry?: () => void;
  /** Открыть список машин (в облачном режиме). */
  onDevices?: () => void;
  /** Короткая версия — для встроенных секций (SSH внутри списка терминалов). */
  compact?: boolean;
}

/**
 * Единое состояние «компьютер не в сети».
 *
 * До этого каждый экран врал по-своему: список терминалов показывал «Нет
 * терминалов» (при работающем на ПК агенте), файлы крутили вечный спиннер,
 * «Система» — бесконечный skeleton, SSH-секция советовала «обновите Remotai на
 * ПК», а карточка готовности печатала английское «agent offline». Причина одна:
 * выключенный/спящий компьютер, и говорить об этом надо прямо.
 */
export function OfflineState({ hostname, onRetry, onDevices, compact }: Props) {
  if (compact) {
    return (
      <div className="offline-inline">
        <span className="offline-inline-ic" aria-hidden>{"🔌"}</span>
        <span>{hostname ? t("offline.titleNamed", { name: hostname }) : t("offline.title")}</span>
        {onRetry && (
          <button className="offline-inline-retry" onClick={onRetry}>{t("offline.retry")}</button>
        )}
      </div>
    );
  }
  return (
    <div className="offline-state">
      <div className="offline-state-glyph" aria-hidden>{"🔌"}</div>
      <h3>{hostname ? t("offline.titleNamed", { name: hostname }) : t("offline.title")}</h3>
      <p>{t("offline.hint")}</p>
      <div className="offline-state-actions">
        {onRetry && (
          <button className="btn btn-primary" onClick={onRetry}>{"↻ "}{t("offline.retry")}</button>
        )}
        {onDevices && (
          <button className="btn btn-secondary" onClick={onDevices}>{t("offline.devices")}</button>
        )}
      </div>
    </div>
  );
}
