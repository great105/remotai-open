/**
 * Стартовый экран пустой вкладки.
 *
 * Пустая вкладка Chrome — это внутренняя страница браузера, свёрстанная под
 * компьютер: крупная плитка Google, мелкие ярлыки, ничего своего. С телефона
 * она читается как «открылся чужой компьютер», а именно от этого ощущения мы и
 * уходим. У телефонного браузера на этом месте своё: строка поиска под палец,
 * сайты, куда человек ходит, и закладки.
 *
 * Экран рисуется ПОВЕРХ кадра, а не внутри страницы: так он появляется мгновенно
 * (не ждёт ни навигации, ни кадра с сервера) и работает даже пока браузер на
 * машине ещё поднимается.
 */

import { useEffect, useState } from "react";
import type { BrowserBookmark, BrowserTopSite } from "../api";
import { forgetBrowserSite, getBrowserPlaces, removeBrowserBookmark } from "../api";
import { t } from "@tgcontrol/shared";
import { haptic } from "../telegram";

export interface BrowserStartPageProps {
  /** Открыть адрес или поисковый запрос (правило — remote/browserNav). */
  onOpen: (text: string) => void;
  /** Тап по строке поиска: полноэкранная адресная строка с клавиатурой. */
  onSearchFocus: () => void;
  /** Обновлять список при каждом появлении экрана. */
  visible: boolean;
}

/** Цвет плитки по имени сайта — стабильный, чтобы сайт всегда был одного цвета. */
function tileColor(host: string): string {
  let hash = 0;
  for (let i = 0; i < host.length; i++) hash = (hash * 31 + host.charCodeAt(i)) % 360;
  return `hsl(${hash}, 52%, 42%)`;
}

function tileLetter(host: string): string {
  const clean = host.replace(/^www\./, "");
  return (clean[0] || "?").toUpperCase();
}

export default function BrowserStartPage({ onOpen, onSearchFocus, visible }: BrowserStartPageProps) {
  const [top, setTop] = useState<BrowserTopSite[]>([]);
  const [marks, setMarks] = useState<BrowserBookmark[]>([]);

  useEffect(() => {
    if (!visible) return;
    let alive = true;
    getBrowserPlaces()
      .then((res) => {
        if (!alive) return;
        setTop(res.top || []);
        setMarks(res.bookmarks || []);
      })
      .catch(() => { /* браузер мог не подняться — экран остаётся с поиском */ });
    return () => { alive = false; };
  }, [visible]);

  const forget = async (host: string) => {
    haptic("light");
    setTop((list) => list.filter((item) => item.host !== host));
    try { await forgetBrowserSite(host); } catch { /* уже убрано */ }
  };

  const unbookmark = async (url: string) => {
    haptic("light");
    setMarks((list) => list.filter((item) => item.url !== url));
    try { await removeBrowserBookmark(url); } catch { /* уже убрано */ }
  };

  return (
    <div className="vb-start">
      <button className="vb-start-search" onClick={() => { haptic("light"); onSearchFocus(); }}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" aria-hidden="true">
          <circle cx="11" cy="11" r="7" strokeWidth="2" />
          <path d="M20 20l-3.5-3.5" strokeWidth="2" strokeLinecap="round" />
        </svg>
        <span>{t("remote.startSearch")}</span>
      </button>

      {top.length > 0 && (
        <>
          <div className="vb-start-section">{t("remote.startFrequent")}</div>
          <div className="vb-start-grid">
            {top.map((site) => (
              <div key={site.host} className="vb-start-tile-wrap">
                <button className="vb-start-tile" onClick={() => { haptic("light"); onOpen(site.url); }}>
                  <span className="vb-start-ava" style={{ background: tileColor(site.host) }} aria-hidden="true">
                    {tileLetter(site.host)}
                  </span>
                  <span className="vb-start-host">{site.host.replace(/^www\./, "")}</span>
                </button>
                <button className="vb-start-forget" aria-label={t("remote.startForget")}
                  onClick={() => void forget(site.host)}>×</button>
              </div>
            ))}
          </div>
        </>
      )}

      {marks.length > 0 && (
        <>
          <div className="vb-start-section">{t("remote.startBookmarks")}</div>
          <div className="vb-start-marks">
            {marks.map((mark) => (
              <div key={mark.url} className="vb-start-mark">
                <button className="vb-start-mark-open" onClick={() => { haptic("light"); onOpen(mark.url); }}>
                  <span className="vb-start-ava small" style={{ background: tileColor(mark.host) }} aria-hidden="true">
                    {tileLetter(mark.host)}
                  </span>
                  <span className="vb-start-mark-texts">
                    <span className="vb-start-mark-title">{mark.title || mark.host}</span>
                    <span className="vb-start-mark-host">{mark.host}</span>
                  </span>
                </button>
                <button className="vb-start-forget" aria-label={t("remote.bookmarkRemove")}
                  onClick={() => void unbookmark(mark.url)}>×</button>
              </div>
            ))}
          </div>
        </>
      )}

      {top.length === 0 && marks.length === 0 && (
        <div className="vb-start-empty">{t("remote.startEmpty")}</div>
      )}
    </div>
  );
}
