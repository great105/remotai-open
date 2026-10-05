/**
 * Поиск по странице.
 *
 * На длинной статье или в списке товаров это единственный способ дойти до
 * нужного места, не листая экран за экраном через удалённый канал — а листать
 * тут дороже, чем в обычном браузере: каждый экран это ещё и кадры по сети.
 * Ищет сам браузер на машине, поэтому найденное подсвечено и прокручено ровно
 * так, как в настоящем «найти на странице».
 */

import { useEffect, useRef, useState } from "react";
import { findOnBrowserPage } from "../api";
import { mapApiError, t, useEscape, useToast } from "@tgcontrol/shared";
import { haptic } from "../telegram";

export interface BrowserFindBarProps {
  onClose: () => void;
}

export default function BrowserFindBar({ onClose }: BrowserFindBarProps) {
  // Системная «Назад» закрывает поиск, а не уводит с экрана (аудит ИА 02.09.2026, P0-6).
  useEscape(true, onClose);
  const [query, setQuery] = useState("");
  const [matches, setMatches] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const { toastError } = useToast();

  useEffect(() => { inputRef.current?.focus(); }, []);

  const run = async (fresh: boolean, forward = true) => {
    const text = query.trim();
    if (!text || busy) return;
    setBusy(true);
    haptic("light");
    try {
      const res = await findOnBrowserPage(text, { fresh, forward });
      setMatches(res.matches);
    } catch (e: any) {
      toastError(mapApiError(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="vb-find">
      <input ref={inputRef} className="vb-find-input" value={query}
        placeholder={t("remote.findPlaceholder")}
        autoCapitalize="off" autoCorrect="off" spellCheck={false}
        onChange={(e) => { setQuery(e.target.value); setMatches(null); }}
        onKeyDown={(e) => {
          if (e.key === "Enter") void run(matches === null);
          if (e.key === "Escape") onClose();
        }} />
      <span className="vb-find-count">
        {matches === null ? "" : matches > 0 ? t("remote.findMatches", { n: matches }) : t("remote.findNothing")}
      </span>
      <button className="vb-find-btn" aria-label={t("remote.findPrev")}
        onClick={() => void run(false, false)}>{"↑"}</button>
      <button className="vb-find-btn" aria-label={t("remote.findNext")}
        onClick={() => void run(matches === null, true)}>{"↓"}</button>
      <button className="vb-find-btn" onClick={onClose} aria-label={t("modal.cancel")}>{"✖"}</button>
    </div>
  );
}
