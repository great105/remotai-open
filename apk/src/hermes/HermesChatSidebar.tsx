import { t } from "@tgcontrol/shared";
import { useId, useState } from "react";
import { SheetShell } from "@tgcontrol/shared";
import {
  IconClose, IconPlus, IconRobot, IconSearch,
} from "../components/icons";
import type { StoredSession } from "./client";

export interface HermesChatSidebarProps {
  desktop: boolean;
  open: boolean;
  sessions: StoredSession[];
  selectedId: string;
  disabled: boolean;
  ready: boolean;
  deviceName: string;
  historyError?: string;
  onClose: () => void;
  onNewChat: () => void;
  onSelect: (id: string) => void;
  onSettings: () => void;
  onConnection: () => void;
  onCommands: () => void;
  onFolder: () => void;
}

export function HermesChatSidebar({
  desktop, open, sessions, selectedId, disabled, deviceName, historyError,
  onClose, onNewChat, onSelect,
}: HermesChatSidebarProps) {
  const headingId = useId();
  const historyHeadingId = useId();
  const [query, setQuery] = useState("");
  const search = query.trim().toLocaleLowerCase("ru");
  const visibleSessions = search ? sessions.filter(session => [
    session.title, session.preview, session.cwd, session.git_repo_root,
  ].some(value => value?.toLocaleLowerCase("ru").includes(search))) : sessions;

  const body = <>
    <div className="hermes-sidebar-top">
      <h2 id={headingId}><IconRobot size={22} /><span>{t("agentSessions.title")}</span></h2>
      {!desktop && <button type="button" className="hermes-icon-button" onClick={onClose} aria-label={t("ui.hermeschatsidebar.mbce1786a4a")}><IconClose size={20} /></button>}
    </div>

    <nav className="hermes-sidebar-nav" aria-label={t("ui.hermeschatsidebar.m0636380c21")}>
      <button type="button" aria-label={t("ui.hermeschatsidebar.m1e4d2a5212")} className="btn btn-primary hermes-sidebar-new" disabled={disabled} onClick={onNewChat}><IconPlus size={18} /><span>{t("ui.hermeschatsidebar.m88c78c9ee6")}</span></button>

    </nav>

    <label className="hermes-sidebar-search">
      <IconSearch size={18} />
      <input id="hermes-chat-search" type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder={t("ui.hermeschatsidebar.m072e4fe370")} aria-label={t("ui.hermeschatsidebar.m255a9e6a2c")} />
    </label>

    <section className="hermes-sidebar-history" aria-labelledby={historyHeadingId}>
      <h3 id={historyHeadingId}>{t("folder.tab.recent")}</h3>
      {historyError && <p className="hermes-sidebar-error" role="alert">{historyError}</p>}
      {visibleSessions.length ? <ul className="hermes-sidebar-list">
        {visibleSessions.map(session => {
          const title = session.title?.trim() || session.preview?.trim() || t("ui.hermeschatsidebar.m376b62b78f");
          const folder = session.cwd?.trim() || session.git_repo_root?.trim();
          const current = selectedId === session.id;
          return <li key={session.id}>
            <button type="button" className={`hermes-sidebar-row${current ? " is-current" : ""}`} disabled={disabled} aria-current={current ? true : undefined} onClick={() => onSelect(session.id)}>
              <span className="hermes-sidebar-row-title" title={title}>{title}</span>
              {folder && <small className="hermes-sidebar-row-folder" title={folder}>{folder}</small>}
            </button>
          </li>;
        })}
      </ul> : !(historyError && !sessions.length) && <p className="hermes-sidebar-empty">{search ? t("ui.hermeschatsidebar.m7b4777676d") : t("ui.hermeschatsidebar.mcc129417d1")}</p>}
    </section>

    <footer className="hermes-sidebar-footer">
      <small className="hermes-sidebar-device" title={deviceName}>{deviceName || t("infra.local.thisPcName")}</small>



    </footer>
  </>;

  return desktop
    ? <aside className="hermes-chat-sidebar" aria-label={t("ui.hermeschatsidebar.m69570bba3c")}>{body}</aside>
    : <SheetShell open={open} onClose={onClose} overlayClassName="hermes-drawer-overlay" className="hermes-drawer" labelledBy={headingId}>{body}</SheetShell>;
}
