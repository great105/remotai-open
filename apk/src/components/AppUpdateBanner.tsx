import { useEffect, useState } from "react";
import { useToast } from "@tgcontrol/shared";
import { t } from "../i18n";
import { haptic } from "../telegram";
import {
  checkAppUpdateThrottled,
  onAppUpdateChange,
  openApkDownload,
  pendingAppUpdate,
  refreshRunningVersion,
  snoozeAppUpdate,
} from "../appUpdate";

/**
 * Плашка «доступна новая версия» на главной — единственный сигнал об обновлении
 * APK вне экрана «Настройки» (N167).
 *
 * До этого проверка версии запускалась ровно при открытии настроек: кто туда не
 * заходит, месяцами жил на старой сборке, считая продукт сырым (из браузера APK
 * ставится вручную, Play Store его не обновляет).
 *
 * Проверка НЕ учащается: `checkAppUpdateThrottled` ходит в сеть не чаще раза в
 * сутки, вне нативного APK не ходит вовсе и молчит при любой ошибке. «Позже»
 * убирает плашку на неделю — для этой версии; следующая покажется сразу.
 */
export function AppUpdateBanner() {
  const { toastSuccess } = useToast();
  const [update, setUpdate] = useState(pendingAppUpdate);

  useEffect(() => {
    const unsubscribe = onAppUpdateChange(() => setUpdate(pendingAppUpdate()));
    void refreshRunningVersion().then(() => setUpdate(pendingAppUpdate()));
    void checkAppUpdateThrottled();
    return unsubscribe;
  }, []);

  if (!update) return null;

  return (
    <div style={{ padding: "0 12px 8px" }}>
      <div className="app-update-banner">
        <div className="app-update-text">
          <b>{t("appUpdate.title", { version: `v${update.latest}` })}</b>
          <small>{update.changelog ? update.changelog : t("appUpdate.hint")}</small>
        </div>
        <div className="app-update-actions">
          <button
            className="btn btn-primary btn-sm"
            onClick={() => { haptic(); openApkDownload(update.apkUrl); }}
          >
            {t("appUpdate.update")}
          </button>
          <button
            className="app-update-later"
            onClick={() => {
              haptic();
              snoozeAppUpdate(update.latest);
              toastSuccess(t("appUpdate.laterToast"));
            }}
          >
            {t("appUpdate.later")}
          </button>
        </div>
      </div>
    </div>
  );
}
