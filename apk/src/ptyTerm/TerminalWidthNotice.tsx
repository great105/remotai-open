import { useEffect, useState } from "react";
import { IconClose, IconRefresh } from "../components/icons";
import { t } from "../i18n";
import "./widthNotice.css";

export function TerminalWidthNotice({ narrow, onReopen }: {
  narrow: boolean;
  onReopen: () => void;
}) {
  const [visible, setVisible] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  useEffect(() => {
    if (!narrow) {
      setVisible(false);
      setDismissed(false);
      return;
    }
    // Opening normally negotiates its width quickly. Do not briefly insert a
    // banner (and change the terminal height) during that ordinary handshake.
    const timer = window.setTimeout(() => setVisible(true), 1800);
    return () => window.clearTimeout(timer);
  }, [narrow]);
  if (!narrow || !visible || dismissed) return null;
  return <div className="pty-width-notice" role="status">
    <div className="pty-width-notice-copy" tabIndex={0} aria-label={t("pty.narrowOutputTitle")}>
      <strong>{t("pty.narrowOutputTitle")}</strong>
      <span>{t("pty.narrowOutputHint")}</span>
    </div>
    <button type="button" className="pty-width-reopen" aria-label={t("pty.reopenView")} title={t("pty.reopenView")} onClick={onReopen}>
      <IconRefresh size={16} /><span>{t("pty.reopenView")}</span>
    </button>
    <button type="button" className="pty-width-dismiss" aria-label={t("pty.a11y.hideBanner")} onClick={() => setDismissed(true)}>
      <IconClose size={18} />
    </button>
  </div>;
}
