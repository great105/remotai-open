import { useNavigate } from "react-router-dom";
import { t } from "../i18n";
import { SheetShell, useEscape } from "@tgcontrol/shared";
import { haptic } from "../telegram";
import type { GuideGroupId } from "../guide/sections";

export interface HelpItem {
  icon: string;
  title: string;
  text: string;
}

interface Props {
  open: boolean;
  title: string;
  items: HelpItem[];
  onClose: () => void;
  /**
   * Группа гида, о которой этот экран («files», «ssh», …). Когда задана, внизу
   * листа — дверь «Подробнее в справке →» в гид, открытый на этой теме.
   * Подсказки по экрану отвечают «как нажать», а «зачем и что будет» живёт в
   * гиде — и до сих пор из одного в другое не вело ничего (аудит ИА
   * 02.09.2026, P1-5, P1-31).
   */
  guide?: GuideGroupId;
}

export function HelpSheet({ open, title, items, onClose, guide }: Props) {
  const navigate = useNavigate();
  useEscape(open, onClose);
  if (!open) return null;

  return (
    <SheetShell open={open} onClose={onClose}
      overlayClassName="modal-overlay" className="modal-sheet help-sheet" labelledBy="help-sheet-title">
        <div className="help-sheet-header">
          <div className="modal-title" id="help-sheet-title">{title}</div>
          <button className="icon-btn" onClick={onClose} aria-label={t("modal.close")}>
            {"\u2715"}
          </button>
        </div>
        <div className="help-sheet-list">
          {items.map((item) => (
            <div key={item.title} className="help-sheet-item">
              <span className="help-sheet-icon">{item.icon}</span>
              <div className="help-sheet-copy">
                <div className="help-sheet-title">{item.title}</div>
                <div className="help-sheet-text">{item.text}</div>
              </div>
            </div>
          ))}
        </div>
        {guide && (
          // Тот же класс, что у дверей внутри гида: дорога «экран → справка»
          // выглядит как дорога «справка → экран». Лист закрываем сами — иначе
          // при возврате «Назад» он встретит человека открытым.
          <button
            className="guide-open-btn"
            onClick={() => { haptic(); onClose(); navigate(`/guide?topic=${guide}`); }}
          >
            {t("help.moreInGuide")}
          </button>
        )}
    </SheetShell>
  );
}
