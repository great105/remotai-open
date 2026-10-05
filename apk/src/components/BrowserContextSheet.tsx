/**
 * Меню долгого нажатия по странице — то, что на телефоне открывается само.
 *
 * Через кадр этого не сделать никак: страница на сервере, а нативное меню
 * Chrome в видеопоток не попадает (да и рисуется оно под мышь). Поэтому мы
 * спрашиваем у страницы, что лежит под пальцем, и показываем ровно те действия,
 * которые в этом месте имеют смысл: на ссылке — открыть в новой вкладке и
 * скопировать адрес, на картинке — открыть её, на тексте — скопировать.
 */

import type { BrowserHit } from "../api";
import { t, useEscape } from "@tgcontrol/shared";

export interface BrowserContextSheetProps {
  hit: BrowserHit;
  onClose: () => void;
  onOpenNewTab: (url: string) => void;
  onOpenHere: (url: string) => void;
  onCopy: (text: string) => void;
  onShare: (url: string) => void;
}

/** Короткая подпись адреса: полный URL в две строки не читается. */
function shortURL(url: string): string {
  try {
    const u = new URL(url);
    const path = u.pathname === "/" ? "" : u.pathname;
    return (u.host + path).replace(/^www\./, "").slice(0, 64);
  } catch {
    return url.slice(0, 64);
  }
}

export default function BrowserContextSheet(props: BrowserContextSheetProps) {
  // Системная «Назад» закрывает меню, а не уводит с экрана (аудит ИА 02.09.2026, P0-6).
  useEscape(true, props.onClose);
  const { hit, onClose, onOpenNewTab, onOpenHere, onCopy, onShare } = props;
  const target = hit.link || hit.image || hit.video || "";
  const text = hit.selection || hit.text || "";

  return (
    <div className="remote-sheet-backdrop" onClick={onClose}>
      <div className="remote-menu-sheet" onClick={(e) => e.stopPropagation()}>
        {target && <div className="remote-quick-section vb-ctx-target">{shortURL(target)}</div>}

        {hit.link && (
          <>
            <button className="remote-quick-row" onClick={() => onOpenNewTab(hit.link!)}>
              <span className="remote-quick-row-icon">{"⧉"}</span>
              <span className="remote-quick-row-label">{t("remote.ctxOpenNewTab")}</span>
            </button>
            <button className="remote-quick-row" onClick={() => onOpenHere(hit.link!)}>
              <span className="remote-quick-row-icon">{"→"}</span>
              <span className="remote-quick-row-label">{t("remote.ctxOpenHere")}</span>
            </button>
            <button className="remote-quick-row" onClick={() => onCopy(hit.link!)}>
              <span className="remote-quick-row-icon">{"⧉"}</span>
              <span className="remote-quick-row-label">{t("remote.ctxCopyLink")}</span>
            </button>
          </>
        )}

        {hit.image && (
          <button className="remote-quick-row" onClick={() => onOpenNewTab(hit.image!)}>
            <span className="remote-quick-row-icon">{"🖼"}</span>
            <span className="remote-quick-row-label">{t("remote.ctxOpenImage")}</span>
          </button>
        )}

        {text && (
          <button className="remote-quick-row" onClick={() => onCopy(text)}>
            <span className="remote-quick-row-icon">{"⎘"}</span>
            <span className="remote-quick-row-label">{t("remote.ctxCopyText")}</span>
          </button>
        )}

        {target && (
          <button className="remote-quick-row" onClick={() => onShare(target)}>
            <span className="remote-quick-row-icon">{"↗"}</span>
            <span className="remote-quick-row-label">{t("remote.ctxShare")}</span>
          </button>
        )}
      </div>
    </div>
  );
}
