import { useState } from "react";
import { useParams } from "react-router-dom";
import { useGoBack } from "../navBack";
import { SshSection } from "../components/SshSection";
import { BottomNav } from "../components/BottomNav";
import { DeviceChip } from "../components/DeviceChip";
import { haptic } from "../telegram";
import { HelpSheet } from "../components/HelpSheet";
import { t } from "../i18n";

export function SshView() {
  // «←» ведёт туда, откуда пришли (список терминалов, настройки, «Мои
  // компьютеры»), а не жёстко на главную; без истории — к родителю из
  // navBack.ts (карточка → список → главная). Аудит ИА 02.09.2026, P0-2.
  const goBack = useGoBack();
  const { hostId } = useParams<{ hostId?: string }>();
  const [helpOpen, setHelpOpen] = useState(false);
  // Заголовок карточки называет сам сервер: «SSH-сервер» одинаково подписывал
  // и прод, и домашнюю малину. Имя приходит из секции, когда список загружен.
  const [hostName, setHostName] = useState("");

  return (
    <div className="page ssh-page">
      <div className="page-header">
        <button
          className="back-btn"
          aria-label={t("generic.back")}
          onClick={() => {
            haptic();
            goBack();
          }}
        >
          {"←"}
        </button>
        {/* Чип машины: серверы хранятся на конкретном компьютере с Remotai, и
            без него список «пустой без причины» после смены машины. */}
        <div className="page-header-context">
          <h1>{hostId ? (hostName || t("ssh.hostTitle")) : t("ssh.title")}</h1>
          <DeviceChip />
        </div>
        <button
          className="icon-btn"
          aria-label={t("ssh.helpTitle")}
          onClick={() => {
            haptic();
            setHelpOpen(true);
          }}
        >
          {"?"}
        </button>
      </div>
      <div className="page-content">
        <SshSection
          key={hostId || "all"}
          expanded
          onToggle={() => {}}
          pageMode
          focusHostId={hostId}
          onFocusHostName={setHostName}
        />
      </div>
      <HelpSheet
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        title={t("ssh.helpTitle")}
        guide="ssh"
        items={[
          { icon: "⌨", title: t("ssh.help.connectTitle"), text: t("ssh.help.connectText") },
          { icon: "📁", title: t("ssh.help.filesTitle"), text: t("ssh.help.filesText") },
          { icon: "⇄", title: t("ssh.help.tunnelTitle"), text: t("ssh.help.tunnelText") },
          // Раздел объясняет, как сервер работает СЕЙЧАС, но молчал о том, что
          // с ним будет дальше: сервер с Remotai перестаёт быть SSH-хостом за
          // компьютером и встаёт в общий список машин наравне с ними.
          { icon: "⬇", title: t("ssh.help.installTitle"), text: t("ssh.help.installText") },
        ]}
      />
      <BottomNav active="ssh" />
    </div>
  );
}
