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
      <h2 id={headingId}><IconRobot size={22} /><span>Беседы</span></h2>
      {!desktop && <button type="button" className="hermes-icon-button" onClick={onClose} aria-label="Закрыть меню"><IconClose size={20} /></button>}
    </div>

    <nav className="hermes-sidebar-nav" aria-label="Действия чатов">
      <button type="button" aria-label="Создать новый чат" className="btn btn-primary hermes-sidebar-new" disabled={disabled} onClick={onNewChat}><IconPlus size={18} /><span>Новый чат</span></button>

    </nav>

    <label className="hermes-sidebar-search">
      <IconSearch size={18} />
      <input id="hermes-chat-search" type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder="Найти чат" aria-label="Поиск чатов" />
    </label>

    <section className="hermes-sidebar-history" aria-labelledby={historyHeadingId}>
      <h3 id={historyHeadingId}>Недавние</h3>
      {historyError && <p className="hermes-sidebar-error" role="alert">{historyError}</p>}
      {visibleSessions.length ? <ul className="hermes-sidebar-list">
        {visibleSessions.map(session => {
          const title = session.title?.trim() || session.preview?.trim() || "Чат Hermes";
          const folder = session.cwd?.trim() || session.git_repo_root?.trim();
          const current = selectedId === session.id;
          return <li key={session.id}>
            <button type="button" className={`hermes-sidebar-row${current ? " is-current" : ""}`} disabled={disabled} aria-current={current ? true : undefined} onClick={() => onSelect(session.id)}>
              <span className="hermes-sidebar-row-title" title={title}>{title}</span>
              {folder && <small className="hermes-sidebar-row-folder" title={folder}>{folder}</small>}
            </button>
          </li>;
        })}
      </ul> : !(historyError && !sessions.length) && <p className="hermes-sidebar-empty">{search ? "Чаты не найдены. Попробуйте другое слово." : "Здесь появятся ваши чаты с Hermes."}</p>}
    </section>

    <footer className="hermes-sidebar-footer">
      <small className="hermes-sidebar-device" title={deviceName}>{deviceName || "Компьютер"}</small>



    </footer>
  </>;

  return desktop
    ? <aside className="hermes-chat-sidebar" aria-label="Чаты Hermes">{body}</aside>
    : <SheetShell open={open} onClose={onClose} overlayClassName="hermes-drawer-overlay" className="hermes-drawer" labelledBy={headingId}>{body}</SheetShell>;
}
